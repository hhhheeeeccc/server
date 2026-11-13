
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
