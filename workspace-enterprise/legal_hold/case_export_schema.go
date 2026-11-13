package legal_hold

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// InstallCaseExportJournal is an explicit internal installation, after the
// case journal and before serving requests. It is not registered in production.
func InstallCaseExportJournal(ctx context.Context, journal *CaseJournal) error {
	if journal == nil {
		return errors.New("legal hold case journal is required")
	}
	if _, err := InstallCaseJournal(ctx, journal.coordinator); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := journal.coordinator.fence.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT to_regclass('workspace_legal_hold.case_export_installation') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.ExecContext(ctx, caseExportSchema); err != nil {
			return err
		}
	}
	if err = installCaseExportRecovery(ctx, tx); err != nil {
		return err
	}
	if err = validateCaseExportJournal(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

type exportQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validateCaseExportJournal(ctx context.Context, db exportQueryer) error {
	var intact bool
	err := db.QueryRowContext(ctx, `WITH expected(relation,name) AS (
	 SELECT to_regclass('workspace_legal_hold.'||r)::oid,t
	 FROM unnest(ARRAY['case_export_installation','case_exports','case_export_outcomes']) r
	 CROSS JOIN unnest(ARRAY['immutable_rows','immutable_truncate']) t)
	 SELECT (SELECT count(*)=1 AND bool_and(version=1) FROM workspace_legal_hold.case_export_installation)
	 AND count(*)=6 AND bool_and(t.oid IS NOT NULL AND t.tgenabled='A')
	 FROM expected e LEFT JOIN pg_trigger t ON t.tgrelid=e.relation AND t.tgname=e.name`).Scan(&intact)
	if err != nil || !intact {
		return errors.Join(errors.New("legal hold export journal or triggers are incomplete"), err)
	}
	return validateCaseExportRecovery(ctx, db)
}

const caseExportSchema = `
CREATE TABLE workspace_legal_hold.case_export_installation(version integer PRIMARY KEY CHECK(version=1));
INSERT INTO workspace_legal_hold.case_export_installation VALUES(1);
CREATE TABLE workspace_legal_hold.case_exports(
 id varchar(26) PRIMARY KEY,case_id varchar(26) NOT NULL REFERENCES workspace_legal_hold.cases(id),
 actor_id varchar(26) NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp());
CREATE INDEX ON workspace_legal_hold.case_exports(case_id,created_at,id);
CREATE TABLE workspace_legal_hold.case_export_outcomes(
 export_id varchar(26) PRIMARY KEY REFERENCES workspace_legal_hold.case_exports(id),
 state text NOT NULL CHECK(state IN('complete','failed')),manifest_sha256 varchar(64),sha256 varchar(64),size bigint,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((state='complete' AND manifest_sha256 IS NOT NULL AND manifest_sha256 ~ '^[0-9a-f]{64}$'
  AND sha256 IS NOT NULL AND sha256 ~ '^[0-9a-f]{64}$' AND size IS NOT NULL AND size>0)
  OR (state='failed' AND manifest_sha256 IS NULL AND sha256 IS NULL AND size IS NULL)));
DO $$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['case_export_installation','case_exports','case_export_outcomes'] LOOP
  EXECUTE format('CREATE TRIGGER immutable_rows BEFORE UPDATE OR DELETE ON workspace_legal_hold.%I FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('CREATE TRIGGER immutable_truncate BEFORE TRUNCATE ON workspace_legal_hold.%I FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_rows',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_truncate',name);
 END LOOP;
END $$;
`
