package legal_hold

import (
	"context"
	"errors"
	"strings"
	"time"
)

// InstallCaseScopeJournal is an additive, explicit migration before serving
// requests. All cooperating writers must be upgraded together. No production
// hook, route, license or background worker is installed here.
func InstallCaseScopeJournal(ctx context.Context, journal *CaseJournal) error {
	if err := InstallCaseStartJournal(ctx, journal); err != nil {
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
	installed, err := scopeJournalInstalled(ctx, tx)
	if err != nil {
		return err
	}
	if !installed {
		// Keep the original evidence writer and its user-secret allowlist.
		record, err := caseVoidFunction("capture_case_object")
		if err != nil {
			return err
		}
		record = strings.Replace(record, "capture_case_object(", "record_case_object(", 1)
		record = strings.Replace(record, "BEGIN\n", "BEGIN\n IF NOT workspace_legal_hold.case_is_open(c) THEN RETURN; END IF;\n", 1)
		schema := caseScopeTables + record + caseScopeCapture + caseScopeBackfill + caseScopeGuards
		// The old release guard remains authoritative; add the pending-scope gate.
		start := strings.Index(caseLifecycleSchema, "CREATE FUNCTION workspace_legal_hold.validate_case_release()")
		if start < 0 {
			return errors.New("legal hold release validation is missing")
		}
		end := strings.Index(caseLifecycleSchema[start:], "END $$;")
		if end < 0 {
			return errors.New("legal hold release validation is incomplete")
		}
		release := strings.Replace(caseLifecycleSchema[start:start+end+len("END $$;")], "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1)
		release = strings.Replace(release, "BEGIN\n", "BEGIN\n IF workspace_legal_hold.case_scope_pending(NEW.case_id) THEN RAISE EXCEPTION 'legal hold scope change is pending' USING ERRCODE='55000'; END IF;\n", 1)
		schema += release
		for name, source := range journal.sources {
			schema = strings.ReplaceAll(schema, "{{"+name+"}}", source)
		}
		if _, err = tx.ExecContext(ctx, schema); err != nil {
			return err
		}
	}
	if err = validateCaseScopeJournal(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func scopeJournalInstalled(ctx context.Context, db exportQueryer) (bool, error) {
	var installed bool
	err := db.QueryRowContext(ctx, `SELECT to_regclass('workspace_legal_hold.case_scope_installation') IS NOT NULL`).Scan(&installed)
	return installed, err
}

func validateCaseScopeJournal(ctx context.Context, db exportQueryer) error {
	var intact bool
	err := db.QueryRowContext(ctx, `WITH expected(relation,name) AS (
	 SELECT to_regclass('workspace_legal_hold.'||r)::oid,t FROM unnest(ARRAY['case_scope_installation','case_scope_requests','case_scope_actions','case_scope_completions','case_scope_members','case_scope_intent_order']) r
	 CROSS JOIN unnest(ARRAY['immutable_rows','immutable_truncate']) t
	 UNION ALL VALUES('workspace_legal_hold.case_scope_requests'::regclass::oid,'validate_request'),
	 ('workspace_legal_hold.case_scope_actions'::regclass::oid,'validate_action'),
	 ('workspace_legal_hold.case_scope_completions'::regclass::oid,'validate_completion'),
	 ('workspace_legal_hold.case_scope_members'::regclass::oid,'acquire_scoped_file'),
	 ('workspace_legal_hold.mutation_intents'::regclass::oid,'scope_order'))
	 SELECT (SELECT count(*)=1 AND bool_and(version=1) FROM workspace_legal_hold.case_scope_installation)
	 AND count(*)=17 AND bool_and(t.oid IS NOT NULL AND t.tgenabled='A')
	 FROM expected e LEFT JOIN pg_trigger t ON t.tgrelid=e.relation AND t.tgname=e.name`).Scan(&intact)
	if err != nil || !intact {
		return errors.Join(errors.New("legal hold scope journal or triggers are incomplete"), err)
	}
	return nil
}

const caseScopeTables = `
CREATE TABLE workspace_legal_hold.case_scope_installation(version integer PRIMARY KEY CHECK(version=1));
INSERT INTO workspace_legal_hold.case_scope_installation VALUES(1);
CREATE TABLE workspace_legal_hold.case_scope_requests(id varchar(26) PRIMARY KEY,case_id varchar(26) NOT NULL REFERENCES workspace_legal_hold.cases(id),actor_id varchar(26) NOT NULL,request_key varchar(26) NOT NULL,payload jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),UNIQUE(actor_id,request_key));
CREATE INDEX ON workspace_legal_hold.case_scope_requests(case_id,id);
CREATE TABLE workspace_legal_hold.case_scope_actions(id bigserial PRIMARY KEY,request_id varchar(26) NOT NULL REFERENCES workspace_legal_hold.case_scope_requests(id),case_id varchar(26) NOT NULL REFERENCES workspace_legal_hold.cases(id),kind text NOT NULL CHECK(kind IN('users','channels')),target_id varchar(26) NOT NULL,action text NOT NULL CHECK(action IN('add','remove')),closes_activation bigint,version_ceiling bigint,intent_ceiling bigint,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),UNIQUE(request_id,kind,target_id));
CREATE INDEX ON workspace_legal_hold.case_scope_actions(case_id,kind,target_id,id DESC);
CREATE TABLE workspace_legal_hold.case_scope_completions(request_id varchar(26) PRIMARY KEY REFERENCES workspace_legal_hold.case_scope_requests(id),actor_id varchar(26) NOT NULL,completed_at timestamptz NOT NULL DEFAULT clock_timestamp());
-- A new activation gets new links. Readding a scope must not reactivate its
-- historical members or import file generations from the unheld interval.
CREATE TABLE workspace_legal_hold.case_scope_members(case_id varchar(26) NOT NULL REFERENCES workspace_legal_hold.cases(id),scope_kind text NOT NULL CHECK(scope_kind IN('users','channels')),scope_target varchar(26) NOT NULL,activation bigint NOT NULL,kind text NOT NULL CHECK(kind IN('posts','temporaryposts','users','channels','fileinfo')),entity_id varchar(26) NOT NULL,version_floor bigint NOT NULL DEFAULT 0,intent_floor bigint NOT NULL DEFAULT 0,PRIMARY KEY(case_id,scope_kind,scope_target,activation,kind,entity_id));
CREATE INDEX ON workspace_legal_hold.case_scope_members(kind,entity_id,case_id);
CREATE TABLE workspace_legal_hold.case_scope_intent_order(intent_id varchar(26) PRIMARY KEY REFERENCES workspace_legal_hold.mutation_intents(id),sequence bigserial NOT NULL UNIQUE);
INSERT INTO workspace_legal_hold.case_scope_intent_order(intent_id) SELECT id FROM workspace_legal_hold.mutation_intents ORDER BY created_at,id;
CREATE VIEW workspace_legal_hold.effective_case_scopes AS
 WITH last_action AS (SELECT DISTINCT ON(case_id,kind,target_id) * FROM workspace_legal_hold.case_scope_actions ORDER BY case_id,kind,target_id,id DESC)
 SELECT s.case_id,s.kind,s.target_id,0::bigint AS activation FROM workspace_legal_hold.case_scopes s
 WHERE NOT EXISTS(SELECT 1 FROM last_action a WHERE (a.case_id,a.kind,a.target_id)=(s.case_id,s.kind,s.target_id))
 UNION ALL SELECT case_id,kind,target_id,id FROM last_action WHERE action='add';
CREATE VIEW workspace_legal_hold.active_case_scope_members AS
 SELECT m.* FROM workspace_legal_hold.case_scope_members m JOIN workspace_legal_hold.effective_case_scopes s
 ON (s.case_id,s.kind,s.target_id,s.activation)=(m.case_id,m.scope_kind,m.scope_target,m.activation)
 WHERE workspace_legal_hold.case_is_open(m.case_id);
CREATE VIEW workspace_legal_hold.case_file_windows AS
 SELECT m.case_id,m.entity_id AS file_id,m.version_floor,m.intent_floor,a.version_ceiling,a.intent_ceiling
 FROM workspace_legal_hold.case_scope_members m LEFT JOIN workspace_legal_hold.case_scope_actions a
 ON (a.case_id,a.kind,a.target_id,a.closes_activation)=(m.case_id,m.scope_kind,m.scope_target,m.activation) AND a.action='remove'
 WHERE m.kind='fileinfo';
CREATE FUNCTION workspace_legal_hold.case_scope_pending(c text) RETURNS boolean LANGUAGE sql VOLATILE SET search_path=pg_catalog AS $$
 SELECT EXISTS(SELECT 1 FROM workspace_legal_hold.case_scope_requests r WHERE r.case_id=c AND NOT EXISTS(SELECT 1 FROM workspace_legal_hold.case_scope_completions WHERE request_id=r.id))
$$;
CREATE OR REPLACE FUNCTION workspace_legal_hold.file_preservation_required(i text) RETURNS boolean LANGUAGE sql VOLATILE SET search_path=pg_catalog AS $$
 SELECT EXISTS(SELECT 1 FROM workspace_legal_hold.independent_file_holds WHERE file_id=i)
 OR EXISTS(SELECT 1 FROM workspace_legal_hold.active_case_scope_members WHERE kind='fileinfo' AND entity_id=i)
$$;
`

const caseScopeGuards = `
DO $$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['case_scope_installation','case_scope_requests','case_scope_actions','case_scope_completions','case_scope_members','case_scope_intent_order'] LOOP
  EXECUTE format('CREATE TRIGGER immutable_rows BEFORE UPDATE OR DELETE ON workspace_legal_hold.%I FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('CREATE TRIGGER immutable_truncate BEFORE TRUNCATE ON workspace_legal_hold.%I FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_rows',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_truncate',name);
 END LOOP;
END $$;
CREATE FUNCTION workspace_legal_hold.validate_scope_request() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE item jsonb; n integer;
BEGIN
 UPDATE workspace_legal_hold.control SET revision=revision+1 WHERE singleton;
 IF NOT FOUND THEN RAISE EXCEPTION 'legal hold control row missing' USING ERRCODE='55000'; END IF;
 IF NOT workspace_legal_hold.case_is_open(NEW.case_id)
 OR NOT EXISTS(SELECT 1 FROM workspace_legal_hold.case_events WHERE case_id=NEW.case_id AND state='active')
 OR workspace_legal_hold.case_scope_pending(NEW.case_id) THEN
  RAISE EXCEPTION 'legal hold scope change requires an active case without a pending change' USING ERRCODE='55000';
 END IF;
 IF NEW.payload->>'case_id' IS DISTINCT FROM NEW.case_id OR jsonb_typeof(NEW.payload->'reason') IS DISTINCT FROM 'string' OR length(NEW.payload->>'reason') NOT BETWEEN 1 AND 2048
 OR jsonb_typeof(NEW.payload->'add') IS DISTINCT FROM 'array' OR jsonb_typeof(NEW.payload->'remove') IS DISTINCT FROM 'array' THEN
  RAISE EXCEPTION 'invalid legal hold scope request' USING ERRCODE='23514';
 END IF;
 SELECT count(*) INTO n FROM jsonb_array_elements((NEW.payload->'add')||(NEW.payload->'remove'));
 IF n NOT BETWEEN 1 AND 256 OR n<>(SELECT count(DISTINCT s) FROM jsonb_array_elements((NEW.payload->'add')||(NEW.payload->'remove')) s) THEN
  RAISE EXCEPTION 'duplicated legal hold scope change' USING ERRCODE='23514';
 END IF;
 FOR item IN SELECT jsonb_array_elements((NEW.payload->'add')||(NEW.payload->'remove')) LOOP
  IF item IS DISTINCT FROM jsonb_build_object('kind',item->>'kind','target_id',item->>'target_id') OR COALESCE(item->>'kind','') NOT IN('users','channels') OR COALESCE(item->>'target_id','') !~ '^[a-z0-9]{26}$' THEN
   RAISE EXCEPTION 'invalid legal hold scope target' USING ERRCODE='23514';
  END IF;
 END LOOP;
 IF EXISTS(SELECT 1 FROM jsonb_array_elements(NEW.payload->'add') s JOIN workspace_legal_hold.effective_case_scopes e ON e.case_id=NEW.case_id AND e.kind=s->>'kind' AND e.target_id=s->>'target_id')
 OR EXISTS(SELECT 1 FROM jsonb_array_elements(NEW.payload->'remove') s WHERE NOT EXISTS(SELECT 1 FROM workspace_legal_hold.effective_case_scopes e WHERE e.case_id=NEW.case_id AND e.kind=s->>'kind' AND e.target_id=s->>'target_id')) THEN
  RAISE EXCEPTION 'legal hold addition is already present or removal is absent' USING ERRCODE='23514';
 END IF;
 n:=(SELECT count(*) FROM workspace_legal_hold.effective_case_scopes WHERE case_id=NEW.case_id)+jsonb_array_length(NEW.payload->'add')-jsonb_array_length(NEW.payload->'remove');
 IF n NOT BETWEEN 1 AND 256 THEN RAISE EXCEPTION 'legal hold final scope must contain one to 256 targets' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER validate_request BEFORE INSERT ON workspace_legal_hold.case_scope_requests FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.validate_scope_request();
ALTER TABLE workspace_legal_hold.case_scope_requests ENABLE ALWAYS TRIGGER validate_request;
CREATE FUNCTION workspace_legal_hold.validate_scope_action() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE r workspace_legal_hold.case_scope_requests%ROWTYPE;
BEGIN
 UPDATE workspace_legal_hold.control SET revision=revision+1 WHERE singleton;
 IF NOT FOUND THEN RAISE EXCEPTION 'legal hold control row missing' USING ERRCODE='55000'; END IF;
 PERFORM workspace_legal_hold.check_file_mutations();
 SELECT * INTO STRICT r FROM workspace_legal_hold.case_scope_requests WHERE id=NEW.request_id;
 IF r.case_id<>NEW.case_id OR NOT workspace_legal_hold.case_is_open(NEW.case_id)
 OR EXISTS(SELECT 1 FROM workspace_legal_hold.case_scope_completions WHERE request_id=r.id)
 OR NOT EXISTS(SELECT 1 FROM jsonb_array_elements(r.payload->NEW.action) s WHERE s=jsonb_build_object('kind',NEW.kind,'target_id',NEW.target_id)) THEN
  RAISE EXCEPTION 'legal hold scope action does not match its request' USING ERRCODE='23514';
 END IF;
 IF NEW.action='add' THEN
  IF EXISTS(SELECT 1 FROM workspace_legal_hold.effective_case_scopes WHERE case_id=NEW.case_id AND kind=NEW.kind AND target_id=NEW.target_id) THEN
   RAISE EXCEPTION 'legal hold scope already active' USING ERRCODE='23514';
  END IF;
  NEW.closes_activation:=NULL; NEW.version_ceiling:=NULL; NEW.intent_ceiling:=NULL;
 ELSE
  SELECT activation INTO STRICT NEW.closes_activation FROM workspace_legal_hold.effective_case_scopes WHERE case_id=NEW.case_id AND kind=NEW.kind AND target_id=NEW.target_id;
  IF EXISTS(SELECT 1 FROM workspace_legal_hold.active_case_scope_members m WHERE m.case_id=NEW.case_id AND m.kind='fileinfo'
   AND NOT EXISTS(SELECT 1 FROM workspace_legal_hold.file_versions v JOIN workspace_legal_hold.file_receipts fr ON fr.version_id=v.id WHERE v.file_id=m.entity_id AND v.id=(SELECT max(id) FROM workspace_legal_hold.file_versions WHERE file_id=m.entity_id))) THEN
   RAISE EXCEPTION 'legal hold scope removal attachments are not sealed' USING ERRCODE='55000';
  END IF;
  NEW.version_ceiling:=COALESCE((SELECT max(id) FROM workspace_legal_hold.file_versions),0);
  NEW.intent_ceiling:=COALESCE((SELECT max(sequence) FROM workspace_legal_hold.case_scope_intent_order),0);
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER validate_action BEFORE INSERT ON workspace_legal_hold.case_scope_actions FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.validate_scope_action();
ALTER TABLE workspace_legal_hold.case_scope_actions ENABLE ALWAYS TRIGGER validate_action;
CREATE FUNCTION workspace_legal_hold.validate_scope_completion() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE r workspace_legal_hold.case_scope_requests%ROWTYPE;
BEGIN
 UPDATE workspace_legal_hold.control SET revision=revision+1 WHERE singleton;
 IF NOT FOUND THEN RAISE EXCEPTION 'legal hold control row missing' USING ERRCODE='55000'; END IF;
 PERFORM workspace_legal_hold.check_file_mutations();
 SELECT * INTO STRICT r FROM workspace_legal_hold.case_scope_requests WHERE id=NEW.request_id;
 IF NOT workspace_legal_hold.case_is_open(r.case_id)
 OR (SELECT count(*) FROM workspace_legal_hold.case_scope_actions WHERE request_id=r.id)<>jsonb_array_length(r.payload->'add')+jsonb_array_length(r.payload->'remove') THEN
  RAISE EXCEPTION 'legal hold scope change actions are incomplete' USING ERRCODE='23514';
 END IF;
 IF EXISTS(SELECT 1 FROM workspace_legal_hold.active_case_scope_members m WHERE m.case_id=r.case_id AND m.kind='fileinfo'
  AND NOT EXISTS(SELECT 1 FROM workspace_legal_hold.file_versions v JOIN workspace_legal_hold.file_receipts f ON f.version_id=v.id WHERE v.file_id=m.entity_id AND v.id=(SELECT max(id) FROM workspace_legal_hold.file_versions WHERE file_id=m.entity_id))) THEN
  RAISE EXCEPTION 'legal hold scope change attachments are not sealed' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER validate_completion BEFORE INSERT ON workspace_legal_hold.case_scope_completions FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.validate_scope_completion();
ALTER TABLE workspace_legal_hold.case_scope_completions ENABLE ALWAYS TRIGGER validate_completion;
CREATE FUNCTION workspace_legal_hold.order_scope_intent() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 INSERT INTO workspace_legal_hold.case_scope_intent_order(intent_id) VALUES(NEW.id);
 RETURN NEW;
END $$;
CREATE TRIGGER scope_order AFTER INSERT ON workspace_legal_hold.mutation_intents FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.order_scope_intent();
ALTER TABLE workspace_legal_hold.mutation_intents ENABLE ALWAYS TRIGGER scope_order;
CREATE FUNCTION workspace_legal_hold.acquire_scoped_file() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE f jsonb;
BEGIN
 PERFORM workspace_legal_hold.case_barrier();
 IF NOT EXISTS(SELECT 1 FROM workspace_legal_hold.effective_case_scopes s WHERE (s.case_id,s.kind,s.target_id,s.activation)=(NEW.case_id,NEW.scope_kind,NEW.scope_target,NEW.activation)) THEN
  RAISE EXCEPTION 'legal hold membership requires an active scope generation' USING ERRCODE='23514';
 END IF;
 IF EXISTS(SELECT 1 FROM workspace_legal_hold.case_scope_members m WHERE (m.case_id,m.scope_kind,m.scope_target,m.activation,m.kind,m.entity_id)=(NEW.case_id,NEW.scope_kind,NEW.scope_target,NEW.activation,NEW.kind,NEW.entity_id)) THEN RETURN NEW; END IF;
 IF NEW.kind='fileinfo' THEN
  PERFORM workspace_legal_hold.lock_case_entity('fileinfo',NEW.entity_id);
  IF NOT EXISTS(SELECT 1 FROM workspace_legal_hold.active_case_scope_members m WHERE m.case_id=NEW.case_id AND m.kind='fileinfo' AND m.entity_id=NEW.entity_id) AND EXISTS(SELECT 1 FROM workspace_legal_hold.file_reservations WHERE file_id=NEW.entity_id) THEN
   PERFORM workspace_legal_hold.check_file_mutations();
   SELECT to_jsonb(src) INTO f FROM {{fileinfo}} src WHERE id=NEW.entity_id;
   IF FOUND THEN PERFORM workspace_legal_hold.reserve_file_metadata(NEW.entity_id,f,false,true); END IF;
  END IF;
  NEW.version_floor:=COALESCE((SELECT max(id) FROM workspace_legal_hold.file_versions WHERE file_id=NEW.entity_id),0);
  NEW.intent_floor:=COALESCE((SELECT max(sequence)+1 FROM workspace_legal_hold.case_scope_intent_order),0);
 ELSE NEW.version_floor:=0; NEW.intent_floor:=0;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER acquire_scoped_file BEFORE INSERT ON workspace_legal_hold.case_scope_members FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.acquire_scoped_file();
ALTER TABLE workspace_legal_hold.case_scope_members ENABLE ALWAYS TRIGGER acquire_scoped_file;
`
