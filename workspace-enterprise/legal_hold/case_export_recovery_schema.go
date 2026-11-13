package legal_hold

import (
	"context"
	"database/sql"
	"errors"
)

// The v1 journal remains immutable. Recovery is an additive installation; its
// attempt outcomes never erase a legacy failure or a previous attempt.
func installCaseExportRecovery(ctx context.Context, tx *sql.Tx) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('workspace_legal_hold.case_export_recovery_installation') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		_, err := tx.ExecContext(ctx, caseExportRecoverySchema)
		return err
	}
	return nil
}

func validateCaseExportRecovery(ctx context.Context, db exportQueryer) error {
	var intact bool
	err := db.QueryRowContext(ctx, `WITH expected(relation,name) AS (
	 SELECT to_regclass('workspace_legal_hold.'||r)::oid,t
	 FROM unnest(ARRAY['case_export_recovery_installation','case_export_store_binding','case_export_keys','case_export_snapshots','case_export_attempts','case_export_attempt_outcomes']) r
	 CROSS JOIN unnest(ARRAY['immutable_rows','immutable_truncate']) t
	 UNION ALL VALUES('workspace_legal_hold.case_export_snapshots'::regclass::oid,'validate_snapshot'),
	 ('workspace_legal_hold.case_export_attempt_outcomes'::regclass::oid,'validate_completion'))
	 SELECT (SELECT count(*)=1 AND bool_and(version=2) FROM workspace_legal_hold.case_export_recovery_installation)
	 AND count(*)=14 AND bool_and(t.oid IS NOT NULL AND t.tgenabled='A')
	 FROM expected e LEFT JOIN pg_trigger t ON t.tgrelid=e.relation AND t.tgname=e.name`).Scan(&intact)
	if err != nil || !intact {
		return errors.Join(errors.New("legal hold export recovery journal or triggers are incomplete"), err)
	}
	return nil
}

const caseExportRecoverySchema = `
CREATE TABLE workspace_legal_hold.case_export_recovery_installation(singleton boolean PRIMARY KEY CHECK(singleton),version integer NOT NULL CHECK(version=2));
INSERT INTO workspace_legal_hold.case_export_recovery_installation VALUES(true,2);
CREATE TABLE workspace_legal_hold.case_export_store_binding(singleton boolean PRIMARY KEY CHECK(singleton),store_id varchar(32) NOT NULL CHECK(store_id ~ '^[0-9a-f]{32}$'));
CREATE TABLE workspace_legal_hold.case_export_keys(actor_id varchar(26),request_key varchar(26),export_id varchar(26) NOT NULL UNIQUE REFERENCES workspace_legal_hold.case_exports(id),PRIMARY KEY(actor_id,request_key));
CREATE TABLE workspace_legal_hold.case_export_snapshots(export_id varchar(26) PRIMARY KEY REFERENCES workspace_legal_hold.case_exports(id),manifest jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp());
CREATE TABLE workspace_legal_hold.case_export_attempts(id varchar(26) PRIMARY KEY,export_id varchar(26) NOT NULL REFERENCES workspace_legal_hold.case_exports(id),actor_id varchar(26) NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),UNIQUE(id,export_id));
CREATE INDEX ON workspace_legal_hold.case_export_attempts(export_id,created_at,id);
CREATE TABLE workspace_legal_hold.case_export_attempt_outcomes(
 attempt_id varchar(26) PRIMARY KEY,export_id varchar(26) NOT NULL,state text NOT NULL CHECK(state IN('complete','failed')),
 manifest_sha256 varchar(64),sha256 varchar(64),size bigint,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(attempt_id,export_id) REFERENCES workspace_legal_hold.case_export_attempts(id,export_id),
 CHECK((state='complete' AND manifest_sha256 IS NOT NULL AND manifest_sha256 ~ '^[0-9a-f]{64}$'
  AND sha256 IS NOT NULL AND sha256 ~ '^[0-9a-f]{64}$' AND size IS NOT NULL AND size>0)
  OR (state='failed' AND manifest_sha256 IS NULL AND sha256 IS NULL AND size IS NULL)));
CREATE UNIQUE INDEX ON workspace_legal_hold.case_export_attempt_outcomes(export_id) WHERE state='complete';
DO $$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['case_export_recovery_installation','case_export_store_binding','case_export_keys','case_export_snapshots','case_export_attempts','case_export_attempt_outcomes'] LOOP
  EXECUTE format('CREATE TRIGGER immutable_rows BEFORE UPDATE OR DELETE ON workspace_legal_hold.%I FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('CREATE TRIGGER immutable_truncate BEFORE TRUNCATE ON workspace_legal_hold.%I FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_rows',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_truncate',name);
 END LOOP;
END $$;
CREATE FUNCTION workspace_legal_hold.validate_export_snapshot() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM workspace_legal_hold.case_exports r CROSS JOIN workspace_legal_hold.archive_binding b
  WHERE r.id=NEW.export_id AND NEW.manifest->>'export_id'=r.id AND NEW.manifest->>'case_id'=r.case_id
  AND NEW.manifest->>'actor_id'=r.actor_id AND NEW.manifest->>'archive_id'=b.archive_id
  AND NEW.manifest->>'version'='1' AND jsonb_typeof(NEW.manifest->'datasets')='array'
  AND jsonb_array_length(NEW.manifest->'datasets')=12) THEN
  RAISE EXCEPTION 'legal hold export snapshot identity differs' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER validate_snapshot BEFORE INSERT ON workspace_legal_hold.case_export_snapshots FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.validate_export_snapshot();
ALTER TABLE workspace_legal_hold.case_export_snapshots ENABLE ALWAYS TRIGGER validate_snapshot;
CREATE FUNCTION workspace_legal_hold.validate_export_completion() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.state='complete' AND NOT EXISTS(SELECT 1 FROM workspace_legal_hold.case_export_snapshots WHERE export_id=NEW.export_id) THEN
  RAISE EXCEPTION 'legal hold export completion lacks durable snapshot' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER validate_completion BEFORE INSERT ON workspace_legal_hold.case_export_attempt_outcomes FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.validate_export_completion();
ALTER TABLE workspace_legal_hold.case_export_attempt_outcomes ENABLE ALWAYS TRIGGER validate_completion;
`
