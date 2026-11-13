package legal_hold

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// InstallCaseAPIAudit is an explicit startup migration. No route is mounted and
// no production registration is made. Existing case evidence is not changed.
func InstallCaseAPIAudit(ctx context.Context, journal *CaseJournal) error {
	if err := InstallCaseScopeJournal(ctx, journal); err != nil {
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
	if err = tx.QueryRowContext(ctx, `SELECT to_regclass('workspace_legal_hold.api_audit_installation') IS NOT NULL`).Scan(&installed); err != nil {
		return err
	}
	if !installed {
		if _, err = tx.ExecContext(ctx, caseAPIAuditSchema); err != nil {
			return err
		}
	}
	if err = validateCaseAPIAudit(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func validateCaseAPIAudit(ctx context.Context, db exportQueryer) error {
	var intact bool
	err := db.QueryRowContext(ctx, `WITH expected(relation,name) AS (
	 SELECT to_regclass('workspace_legal_hold.'||r)::oid,t FROM unnest(ARRAY['api_audit_installation','api_requests','api_identities','api_actions','api_downloads','api_results']) r
	 CROSS JOIN unnest(ARRAY['immutable_rows','immutable_truncate']) t
	 UNION ALL VALUES('workspace_legal_hold.api_actions'::regclass::oid,'require_authorized'),('workspace_legal_hold.api_downloads'::regclass::oid,'require_download'))
	 SELECT (SELECT count(*)=1 AND bool_and(version=1) FROM workspace_legal_hold.api_audit_installation)
	 AND count(*)=14 AND bool_and(t.oid IS NOT NULL AND t.tgenabled='A')
	 FROM expected e LEFT JOIN pg_trigger t ON t.tgrelid=e.relation AND t.tgname=e.name`).Scan(&intact)
	if err != nil || !intact {
		return errors.Join(errors.New("legal hold API audit or triggers are incomplete"), err)
	}
	return nil
}

type caseAPIAudit struct{ db *sql.DB }

func (a caseAPIAudit) exec(ctx context.Context, query string, args ...any) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := a.db.ExecContext(ctx, query, args...)
	return err
}

func (a caseAPIAudit) begin(ctx context.Context, id, operation, method string) error {
	return a.exec(ctx, `INSERT INTO workspace_legal_hold.api_requests(id,operation,method) VALUES($1,$2,$3)`, id, operation, method)
}

func (a caseAPIAudit) identity(ctx context.Context, id, actor string, allowed bool) error {
	return a.exec(ctx, `INSERT INTO workspace_legal_hold.api_identities(request_id,actor_id,authorized) VALUES($1,NULLIF($2,''),$3)`, id, actor, allowed)
}

func (a caseAPIAudit) action(ctx context.Context, s *caseAPIRequest, key, caseID, resourceID string) error {
	return a.exec(ctx, `INSERT INTO workspace_legal_hold.api_actions(request_id,request_key,case_id,resource_id) VALUES($1,NULLIF($2,''),NULLIF($3,''),NULLIF($4,''))`, s.id, key, caseID, resourceID)
}

func (a caseAPIAudit) finish(ctx context.Context, s *caseAPIRequest, status int, bytesWritten int64) error {
	// A disconnect must not cancel recording the known server result. No raw
	// error, request body, credentials, URL/query, IP or evidence is retained.
	return a.exec(context.WithoutCancel(ctx), `INSERT INTO workspace_legal_hold.api_results(request_id,http_status,outcome,error_code,case_id,export_id,scope_request_id,bytes_written) VALUES($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),$8)`, s.id, status, s.outcome, s.code, s.caseID, s.exportID, s.scopeID, bytesWritten)
}

const caseAPIAuditSchema = `
CREATE TABLE workspace_legal_hold.api_audit_installation(version integer PRIMARY KEY CHECK(version=1));
INSERT INTO workspace_legal_hold.api_audit_installation VALUES(1);
CREATE TABLE workspace_legal_hold.api_requests(id varchar(26) PRIMARY KEY,sequence bigserial NOT NULL UNIQUE,operation text NOT NULL CHECK(length(operation) BETWEEN 1 AND 64),method text NOT NULL CHECK(length(method) BETWEEN 1 AND 16),started_at timestamptz NOT NULL DEFAULT clock_timestamp());
CREATE TABLE workspace_legal_hold.api_identities(request_id varchar(26) PRIMARY KEY REFERENCES workspace_legal_hold.api_requests(id),actor_id varchar(26),authorized boolean NOT NULL,checked_at timestamptz NOT NULL DEFAULT clock_timestamp(),CHECK(NOT authorized OR actor_id IS NOT NULL));
CREATE TABLE workspace_legal_hold.api_actions(request_id varchar(26) PRIMARY KEY REFERENCES workspace_legal_hold.api_requests(id),request_key varchar(26),case_id varchar(26),resource_id varchar(26),admitted_at timestamptz NOT NULL DEFAULT clock_timestamp());
CREATE INDEX ON workspace_legal_hold.api_actions(case_id,request_id);
CREATE TABLE workspace_legal_hold.api_downloads(request_id varchar(26) PRIMARY KEY REFERENCES workspace_legal_hold.api_actions(request_id),export_id varchar(26) NOT NULL REFERENCES workspace_legal_hold.case_exports(id),sha256 varchar(64) NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),size bigint NOT NULL CHECK(size>=0),started_at timestamptz NOT NULL DEFAULT clock_timestamp());
CREATE TABLE workspace_legal_hold.api_results(request_id varchar(26) PRIMARY KEY REFERENCES workspace_legal_hold.api_requests(id),http_status integer NOT NULL CHECK(http_status BETWEEN 100 AND 599),outcome text NOT NULL CHECK(outcome IN('succeeded','rejected','failed','pending','interrupted')),error_code text NOT NULL CHECK(length(error_code)<=64),case_id varchar(26),export_id varchar(26),scope_request_id varchar(26),bytes_written bigint NOT NULL CHECK(bytes_written>=0),finished_at timestamptz NOT NULL DEFAULT clock_timestamp());
DO $$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['api_audit_installation','api_requests','api_identities','api_actions','api_downloads','api_results'] LOOP
  EXECUTE format('CREATE TRIGGER immutable_rows BEFORE UPDATE OR DELETE ON workspace_legal_hold.%I FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('CREATE TRIGGER immutable_truncate BEFORE TRUNCATE ON workspace_legal_hold.%I FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_rows',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_truncate',name);
 END LOOP;
END $$;
CREATE FUNCTION workspace_legal_hold.validate_api_action() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM workspace_legal_hold.api_identities WHERE request_id=NEW.request_id AND authorized) THEN
  RAISE EXCEPTION 'legal hold API action requires recorded authorization' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER require_authorized BEFORE INSERT ON workspace_legal_hold.api_actions FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.validate_api_action();
ALTER TABLE workspace_legal_hold.api_actions ENABLE ALWAYS TRIGGER require_authorized;
CREATE FUNCTION workspace_legal_hold.validate_api_download() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM workspace_legal_hold.api_requests r JOIN workspace_legal_hold.api_actions a ON a.request_id=r.id WHERE r.id=NEW.request_id AND r.operation='download' AND a.resource_id=NEW.export_id) THEN
  RAISE EXCEPTION 'legal hold API download requires its admitted export' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER require_download BEFORE INSERT ON workspace_legal_hold.api_downloads FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.validate_api_download();
ALTER TABLE workspace_legal_hold.api_downloads ENABLE ALWAYS TRIGGER require_download;
`
