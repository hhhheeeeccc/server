package legal_hold

import (
	"context"
	"database/sql"
	"errors"
)

func validateFileMutationSchema(ctx context.Context, tx *sql.Tx) error {
	var intact bool
	err := tx.QueryRowContext(ctx, `WITH expected(relation,name) AS (
	 SELECT to_regclass('workspace_legal_hold.'||r)::oid,t
	 FROM unnest(ARRAY['archive_binding','mutation_intents','mutation_files','mutation_observations','mutation_completions']) r
	 CROSS JOIN unnest(ARRAY['immutable_rows','immutable_truncate']) t
	 UNION ALL VALUES ('workspace_legal_hold.mutation_pending'::regclass::oid,'pending_rows'),
	 ('workspace_legal_hold.mutation_pending'::regclass::oid,'pending_truncate'),
	 ('workspace_legal_hold.mutation_intents'::regclass::oid,'open_intent'),
	 ('workspace_legal_hold.mutation_completions'::regclass::oid,'validate_completion'),
	 ('workspace_legal_hold.mutation_completions'::regclass::oid,'close_intent'))
	 SELECT count(*)=15 AND bool_and(t.oid IS NOT NULL AND t.tgenabled='A')
	 FROM expected e LEFT JOIN pg_trigger t ON t.tgrelid=e.relation AND t.tgname=e.name`).Scan(&intact)
	if err != nil || !intact {
		return errors.New("legal hold physical mutation schema or triggers are incomplete")
	}
	return nil
}

const fileMutationSchema = `
CREATE TABLE workspace_legal_hold.archive_binding(singleton boolean PRIMARY KEY CHECK(singleton),archive_id varchar(32) NOT NULL UNIQUE CHECK(archive_id ~ '^[0-9a-f]{32}$'));
CREATE TABLE workspace_legal_hold.mutation_intents(
 id varchar(26) PRIMARY KEY,archive_id varchar(32) NOT NULL REFERENCES workspace_legal_hold.archive_binding(archive_id),
 operation text NOT NULL CHECK(operation IN ('remove','remove-directory','write','append','copy','move','test-connection')),
 paths jsonb NOT NULL CHECK(jsonb_typeof(paths)='array'),recursive boolean NOT NULL,
 observed_paths jsonb NOT NULL CHECK(jsonb_typeof(observed_paths)='array'),created_at timestamptz NOT NULL DEFAULT clock_timestamp());
CREATE TABLE workspace_legal_hold.mutation_files(intent_id varchar(26) NOT NULL REFERENCES workspace_legal_hold.mutation_intents(id),
 file_id varchar(26) NOT NULL REFERENCES workspace_legal_hold.file_reservations(file_id),version_id bigint NOT NULL REFERENCES workspace_legal_hold.file_versions(id),PRIMARY KEY(intent_id,file_id));
CREATE TABLE workspace_legal_hold.mutation_observations(intent_id varchar(26) NOT NULL REFERENCES workspace_legal_hold.mutation_intents(id),
 phase text NOT NULL CHECK(phase IN ('before','after','recovery')),path text NOT NULL,present boolean NOT NULL,sha256 varchar(64),size bigint,
 PRIMARY KEY(intent_id,phase,path),CHECK((present AND sha256 ~ '^[0-9a-f]{64}$' AND sha256 IS NOT NULL AND size>=0 AND size IS NOT NULL) OR (NOT present AND sha256 IS NULL AND size IS NULL)));
CREATE TABLE workspace_legal_hold.mutation_completions(intent_id varchar(26) PRIMARY KEY REFERENCES workspace_legal_hold.mutation_intents(id),
 result text NOT NULL CHECK(result IN ('success','failed','recovered')),completed_at timestamptz NOT NULL DEFAULT clock_timestamp());
CREATE TABLE workspace_legal_hold.mutation_pending(singleton boolean PRIMARY KEY CHECK(singleton),intent_id varchar(26) NOT NULL UNIQUE REFERENCES workspace_legal_hold.mutation_intents(id));

DO $$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['archive_binding','mutation_intents','mutation_files','mutation_observations','mutation_completions'] LOOP
  EXECUTE format('CREATE TRIGGER immutable_rows BEFORE UPDATE OR DELETE ON workspace_legal_hold.%I FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('CREATE TRIGGER immutable_truncate BEFORE TRUNCATE ON workspace_legal_hold.%I FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_rows',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_truncate',name);
 END LOOP;
END $$;
CREATE FUNCTION workspace_legal_hold.check_file_mutations() RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM workspace_legal_hold.mutation_pending) THEN
  RAISE EXCEPTION 'legal hold physical mutation is pending' USING ERRCODE='55000';
 END IF;
END $$;
CREATE FUNCTION workspace_legal_hold.guard_pending_intent() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF TG_OP='INSERT' AND NOT EXISTS(SELECT 1 FROM workspace_legal_hold.mutation_completions WHERE intent_id=NEW.intent_id) THEN RETURN NEW; END IF;
 IF TG_OP='DELETE' AND EXISTS(SELECT 1 FROM workspace_legal_hold.mutation_completions WHERE intent_id=OLD.intent_id) THEN RETURN OLD; END IF;
 RAISE EXCEPTION 'legal hold pending intent requires a completion' USING ERRCODE='55000';
END $$;
CREATE TRIGGER pending_rows BEFORE INSERT OR UPDATE OR DELETE ON workspace_legal_hold.mutation_pending FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.guard_pending_intent();
CREATE TRIGGER pending_truncate BEFORE TRUNCATE ON workspace_legal_hold.mutation_pending FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation();
ALTER TABLE workspace_legal_hold.mutation_pending ENABLE ALWAYS TRIGGER pending_rows;
ALTER TABLE workspace_legal_hold.mutation_pending ENABLE ALWAYS TRIGGER pending_truncate;
CREATE FUNCTION workspace_legal_hold.open_file_intent() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN INSERT INTO workspace_legal_hold.mutation_pending VALUES(true,NEW.id); RETURN NEW; END $$;
CREATE TRIGGER open_intent AFTER INSERT ON workspace_legal_hold.mutation_intents FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.open_file_intent();
ALTER TABLE workspace_legal_hold.mutation_intents ENABLE ALWAYS TRIGGER open_intent;
CREATE FUNCTION workspace_legal_hold.validate_file_completion() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE phase_name text;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM workspace_legal_hold.mutation_pending WHERE intent_id=NEW.intent_id) THEN
  RAISE EXCEPTION 'legal hold completion has no pending intent' USING ERRCODE='55000';
 END IF;
 phase_name:=CASE WHEN NEW.result='recovered' THEN 'recovery' ELSE 'after' END;
 IF EXISTS(SELECT 1 FROM workspace_legal_hold.mutation_intents i,jsonb_array_elements_text(i.observed_paths) p
  WHERE i.id=NEW.intent_id AND NOT EXISTS(SELECT 1 FROM workspace_legal_hold.mutation_observations o WHERE o.intent_id=i.id AND o.phase=phase_name AND o.path=p)) THEN
  RAISE EXCEPTION 'legal hold completion lacks path observations' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER validate_completion BEFORE INSERT ON workspace_legal_hold.mutation_completions FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.validate_file_completion();
ALTER TABLE workspace_legal_hold.mutation_completions ENABLE ALWAYS TRIGGER validate_completion;
CREATE FUNCTION workspace_legal_hold.close_file_intent() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN DELETE FROM workspace_legal_hold.mutation_pending WHERE intent_id=NEW.intent_id; RETURN NEW; END $$;
CREATE TRIGGER close_intent AFTER INSERT ON workspace_legal_hold.mutation_completions FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.close_file_intent();
ALTER TABLE workspace_legal_hold.mutation_completions ENABLE ALWAYS TRIGGER close_intent;

ALTER TABLE workspace_legal_hold.installation DROP CONSTRAINT installation_version_check;
ALTER TABLE workspace_legal_hold.installation ADD CONSTRAINT installation_version_check CHECK(version IN(1,2));
INSERT INTO workspace_legal_hold.installation(version,source_oid) SELECT 2,source_oid FROM workspace_legal_hold.installation WHERE version=1;
`
