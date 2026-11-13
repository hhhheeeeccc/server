package legal_hold

import (
	"context"
	"errors"
	"time"
)

// InstallCaseStartJournal is an explicit, additive early-startup migration.
// Legacy cases keep their identity and evidence; no request key is invented.
func InstallCaseStartJournal(ctx context.Context, journal *CaseJournal) error {
	if err := InstallCaseLifecycle(ctx, journal); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := journal.coordinator
	lease, err := c.vault.mutationLease(ctx)
	if err != nil {
		return err
	}
	defer lease.Close()
	tx, err := c.fence.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var installed bool
	if err = tx.QueryRowContext(ctx, `SELECT to_regclass('workspace_legal_hold.case_start_installation') IS NOT NULL`).Scan(&installed); err != nil {
		return err
	}
	if !installed {
		if _, err = tx.ExecContext(ctx, caseStartSchema); err != nil {
			return err
		}
	}
	if err = validateCaseStartJournal(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func validateCaseStartJournal(ctx context.Context, db exportQueryer) error {
	var intact bool
	err := db.QueryRowContext(ctx, `WITH expected(relation,name) AS (
	 SELECT to_regclass('workspace_legal_hold.'||r)::oid,t FROM unnest(ARRAY['case_start_installation','case_start_keys']) r
	 CROSS JOIN unnest(ARRAY['immutable_rows','immutable_truncate']) t
	 UNION ALL VALUES('workspace_legal_hold.case_start_keys'::regclass::oid,'validate_start_key'))
	 SELECT (SELECT count(*)=1 AND bool_and(version=1) FROM workspace_legal_hold.case_start_installation)
	 AND count(*)=5 AND bool_and(t.oid IS NOT NULL AND t.tgenabled='A')
	 FROM expected e LEFT JOIN pg_trigger t ON t.tgrelid=e.relation AND t.tgname=e.name`).Scan(&intact)
	if err != nil || !intact {
		return errors.Join(errors.New("legal hold case start journal or triggers are incomplete"), err)
	}
	return nil
}

const caseStartSchema = `
CREATE TABLE workspace_legal_hold.case_start_installation(version integer PRIMARY KEY CHECK(version=1));
INSERT INTO workspace_legal_hold.case_start_installation VALUES(1);
CREATE TABLE workspace_legal_hold.case_start_keys(actor_id varchar(26) NOT NULL,request_key varchar(26) NOT NULL,case_id varchar(26) NOT NULL UNIQUE REFERENCES workspace_legal_hold.cases(id),request_payload jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),PRIMARY KEY(actor_id,request_key));
DO $$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['case_start_installation','case_start_keys'] LOOP
  EXECUTE format('CREATE TRIGGER immutable_rows BEFORE UPDATE OR DELETE ON workspace_legal_hold.%I FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('CREATE TRIGGER immutable_truncate BEFORE TRUNCATE ON workspace_legal_hold.%I FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_rows',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_truncate',name);
 END LOOP;
END $$;
CREATE FUNCTION workspace_legal_hold.validate_case_start_key() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 PERFORM workspace_legal_hold.case_barrier();
 IF NOT EXISTS(SELECT 1 FROM workspace_legal_hold.cases c WHERE c.id=NEW.case_id AND c.created_by=NEW.actor_id
  AND EXISTS(SELECT 1 FROM workspace_legal_hold.case_events WHERE case_id=c.id AND state='preparing')
  AND NEW.request_payload=jsonb_build_object('title',c.title,'scopes',
   (SELECT jsonb_agg(jsonb_build_object('kind',s.kind,'target_id',s.target_id) ORDER BY s.kind,s.target_id) FROM workspace_legal_hold.case_scopes s WHERE s.case_id=c.id))) THEN
  RAISE EXCEPTION 'legal hold start key does not match its case and scope' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER validate_start_key BEFORE INSERT ON workspace_legal_hold.case_start_keys FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.validate_case_start_key();
ALTER TABLE workspace_legal_hold.case_start_keys ENABLE ALWAYS TRIGGER validate_start_key;
`
