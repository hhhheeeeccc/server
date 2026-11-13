package legal_hold

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// CaseJournal is an internal case and message journal. Installation is explicit;
// there is no production registration, management route, release or export API.
type CaseJournal struct {
	coordinator *FileCoordinator
	sources     map[string]string
}

var caseSourceNames = []string{"posts", "temporaryposts", "users", "channels", "fileinfo"}

// InstallCaseJournal is intended for the early, quiescent startup hook. It binds
// every source OID and atomically attaches ALWAYS triggers to the native tables.
// It does not activate a case, grant a license or enable any production feature.
func InstallCaseJournal(ctx context.Context, coordinator *FileCoordinator) (*CaseJournal, error) {
	if coordinator == nil {
		return nil, errors.New("legal hold file coordinator is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := InstallFileFence(ctx, coordinator.fence.db); err != nil {
		return nil, err
	}
	lease, err := coordinator.vault.mutationLease(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	if err = coordinator.recoverLocked(ctx); err != nil {
		return nil, err
	}
	tx, err := coordinator.fence.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	sources, identities := map[string]string{}, map[string]int64{}
	for _, name := range caseSourceNames {
		var oid int64
		var schema, table string
		if err = tx.QueryRowContext(ctx, `SELECT c.oid,n.nspname,c.relname FROM pg_class c
			JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.oid=to_regclass($1) AND c.relkind='r'`, name).Scan(&oid, &schema, &table); err != nil {
			return nil, fmt.Errorf("resolve legal hold %s: %w", name, err)
		}
		sources[name] = pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(table)
		identities[name] = oid
	}
	for _, name := range caseSourceNames {
		if _, err = tx.ExecContext(ctx, "LOCK TABLE "+sources[name]+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(identities)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT to_regclass('workspace_legal_hold.case_installation') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		var intact bool
		if err = tx.QueryRowContext(ctx, `SELECT version=1 AND sources=$1::jsonb FROM workspace_legal_hold.case_installation`, string(encoded)).Scan(&intact); err != nil || !intact {
			return nil, errors.New("legal hold case source identity or version differs")
		}
	} else {
		schema := caseSchema
		for _, name := range caseSourceNames {
			schema = strings.ReplaceAll(schema, "{{"+name+"}}", sources[name])
		}
		if _, err = tx.ExecContext(ctx, schema); err != nil {
			return nil, fmt.Errorf("install legal hold case journal: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_installation VALUES(1,$1::jsonb)`, string(encoded)); err != nil {
			return nil, err
		}
		for _, name := range caseSourceNames {
			function := "guard_case_row"
			if name == "fileinfo" {
				function = "guard_case_file_row"
			}
			for _, statement := range []string{
				"CREATE TRIGGER workspace_legal_hold_case_row BEFORE INSERT OR UPDATE OR DELETE ON " + sources[name] + " FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold." + function + "('" + name + "')",
				"CREATE TRIGGER workspace_legal_hold_case_truncate BEFORE TRUNCATE ON " + sources[name] + " FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.guard_case_truncate()",
				"ALTER TABLE " + sources[name] + " ENABLE ALWAYS TRIGGER workspace_legal_hold_case_row",
				"ALTER TABLE " + sources[name] + " ENABLE ALWAYS TRIGGER workspace_legal_hold_case_truncate",
			} {
				if _, err = tx.ExecContext(ctx, statement); err != nil {
					return nil, err
				}
			}
			if name != "fileinfo" {
				if _, err = tx.ExecContext(ctx, "CREATE TRIGGER workspace_legal_hold_case_after AFTER INSERT OR UPDATE ON "+sources[name]+" FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.guard_case_row('"+name+"'); ALTER TABLE "+sources[name]+" ENABLE ALWAYS TRIGGER workspace_legal_hold_case_after"); err != nil {
					return nil, err
				}
			}
		}
		if _, err = tx.ExecContext(ctx, "CREATE TRIGGER workspace_legal_hold_case_file_after AFTER INSERT OR UPDATE ON "+sources["fileinfo"]+" FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.capture_case_file_after(); ALTER TABLE "+sources["fileinfo"]+" ENABLE ALWAYS TRIGGER workspace_legal_hold_case_file_after"); err != nil {
			return nil, err
		}
	}
	if err = validateCaseJournal(ctx, tx, identities); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &CaseJournal{coordinator: coordinator, sources: sources}, nil
}

func validateCaseJournal(ctx context.Context, tx *sql.Tx, identities map[string]int64) error {
	var intact bool
	err := tx.QueryRowContext(ctx, `WITH expected(relation,name) AS (
	 SELECT to_regclass('workspace_legal_hold.'||r)::oid,t
	 FROM unnest(ARRAY['case_installation','cases','case_scopes','case_events','case_members','case_versions']) r
	 CROSS JOIN unnest(ARRAY['immutable_rows','immutable_truncate']) t
	 UNION ALL SELECT unnest($1::oid[]), 'workspace_legal_hold_case_row'
	 UNION ALL SELECT unnest($1::oid[]), 'workspace_legal_hold_case_truncate'
	 UNION ALL SELECT unnest($3::oid[]), 'workspace_legal_hold_case_after'
	 UNION ALL VALUES($2::oid,'workspace_legal_hold_case_file_after'),
	 ('workspace_legal_hold.case_scopes'::regclass::oid,'validate_scope'),
	 ('workspace_legal_hold.case_events'::regclass::oid,'validate_event'),
	 ('workspace_legal_hold.case_entity_barriers'::regclass::oid,'barrier_rows'),
	 ('workspace_legal_hold.case_entity_barriers'::regclass::oid,'barrier_truncate'))
	 SELECT count(*)=31 AND bool_and(t.oid IS NOT NULL AND t.tgenabled='A')
	 FROM expected e LEFT JOIN pg_trigger t ON t.tgrelid=e.relation AND t.tgname=e.name`,
		pq.Array([]int64{identities["posts"], identities["temporaryposts"], identities["users"], identities["channels"], identities["fileinfo"]}), identities["fileinfo"],
		pq.Array([]int64{identities["posts"], identities["temporaryposts"], identities["users"], identities["channels"]})).Scan(&intact)
	if err != nil || !intact {
		return errors.New("legal hold case journal or native triggers are incomplete")
	}
	return nil
}

const caseSchema = `
CREATE TABLE workspace_legal_hold.case_installation(version integer PRIMARY KEY CHECK(version=1),sources jsonb NOT NULL);
CREATE TABLE workspace_legal_hold.cases(id varchar(26) PRIMARY KEY,title text NOT NULL CHECK(length(title) BETWEEN 1 AND 256),created_by varchar(26) NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp());
CREATE TABLE workspace_legal_hold.case_scopes(case_id varchar(26) REFERENCES workspace_legal_hold.cases(id),kind text CHECK(kind IN('users','channels')),target_id varchar(26) NOT NULL,PRIMARY KEY(case_id,kind,target_id));
CREATE INDEX ON workspace_legal_hold.case_scopes(kind,target_id,case_id);
CREATE TABLE workspace_legal_hold.case_events(id bigserial PRIMARY KEY,case_id varchar(26) NOT NULL REFERENCES workspace_legal_hold.cases(id),state text NOT NULL CHECK(state IN('preparing','active')),actor_id varchar(26) NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),UNIQUE(case_id,state));
CREATE TABLE workspace_legal_hold.case_members(case_id varchar(26) REFERENCES workspace_legal_hold.cases(id),kind text CHECK(kind IN('posts','temporaryposts','users','channels','fileinfo')),entity_id varchar(26) NOT NULL,PRIMARY KEY(case_id,kind,entity_id));
CREATE INDEX ON workspace_legal_hold.case_members(kind,entity_id,case_id);
CREATE TABLE workspace_legal_hold.case_versions(id bigserial PRIMARY KEY,case_id varchar(26) NOT NULL,kind text NOT NULL,entity_id varchar(26) NOT NULL,data jsonb NOT NULL,operation text NOT NULL CHECK(operation IN('snapshot','insert','update','delete')),created_at timestamptz NOT NULL DEFAULT clock_timestamp(),FOREIGN KEY(case_id,kind,entity_id) REFERENCES workspace_legal_hold.case_members(case_id,kind,entity_id));
CREATE INDEX ON workspace_legal_hold.case_versions(case_id,kind,entity_id,id DESC);
CREATE TABLE workspace_legal_hold.case_entity_barriers(kind text NOT NULL CHECK(kind IN('posts','users','channels','fileinfo')),entity_id varchar(26) NOT NULL,revision bigint NOT NULL CHECK(revision>0),PRIMARY KEY(kind,entity_id));
DO $$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['case_installation','cases','case_scopes','case_events','case_members','case_versions'] LOOP
  EXECUTE format('CREATE TRIGGER immutable_rows BEFORE UPDATE OR DELETE ON workspace_legal_hold.%I FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('CREATE TRIGGER immutable_truncate BEFORE TRUNCATE ON workspace_legal_hold.%I FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_rows',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_truncate',name);
 END LOOP;
END $$;

CREATE FUNCTION workspace_legal_hold.case_barrier() RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 PERFORM revision FROM workspace_legal_hold.control WHERE singleton FOR SHARE;
 IF NOT FOUND THEN RAISE EXCEPTION 'legal hold coordination row missing' USING ERRCODE='55000'; END IF;
END $$;
CREATE FUNCTION workspace_legal_hold.guard_case_entity_barrier() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF TG_OP='UPDATE' THEN
  IF (NEW.kind,NEW.entity_id) IS NOT DISTINCT FROM (OLD.kind,OLD.entity_id) AND NEW.revision=OLD.revision+1 THEN RETURN NEW; END IF;
 END IF;
 RAISE EXCEPTION 'legal hold entity coordination revision must advance and cannot be removed' USING ERRCODE='55000';
END $$;
CREATE TRIGGER barrier_rows BEFORE UPDATE OR DELETE ON workspace_legal_hold.case_entity_barriers FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.guard_case_entity_barrier();
CREATE TRIGGER barrier_truncate BEFORE TRUNCATE ON workspace_legal_hold.case_entity_barriers FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.guard_case_entity_barrier();
ALTER TABLE workspace_legal_hold.case_entity_barriers ENABLE ALWAYS TRIGGER barrier_rows;
ALTER TABLE workspace_legal_hold.case_entity_barriers ENABLE ALWAYS TRIGGER barrier_truncate;
CREATE FUNCTION workspace_legal_hold.lock_case_entity(k text,i text) RETURNS void LANGUAGE plpgsql VOLATILE SET search_path=pg_catalog AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM workspace_legal_hold.cases) THEN RETURN; END IF;
 -- A row update both serializes READ COMMITTED and rejects a stale higher
 -- isolation snapshot. Advisory waiting alone cannot refresh that snapshot.
 INSERT INTO workspace_legal_hold.case_entity_barriers VALUES(k,i,1)
 ON CONFLICT(kind,entity_id) DO UPDATE SET revision=workspace_legal_hold.case_entity_barriers.revision+1;
END
$$;
CREATE FUNCTION workspace_legal_hold.validate_case_scope() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM workspace_legal_hold.case_events WHERE case_id=NEW.case_id) THEN
  RAISE EXCEPTION 'legal hold case scopes are fixed before capture starts' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER validate_scope BEFORE INSERT ON workspace_legal_hold.case_scopes FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.validate_case_scope();
ALTER TABLE workspace_legal_hold.case_scopes ENABLE ALWAYS TRIGGER validate_scope;
CREATE FUNCTION workspace_legal_hold.validate_case_event() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM workspace_legal_hold.case_scopes WHERE case_id=NEW.case_id)
  OR (NEW.state='active' AND NOT EXISTS(SELECT 1 FROM workspace_legal_hold.case_events WHERE case_id=NEW.case_id AND state='preparing')) THEN
  RAISE EXCEPTION 'legal hold case transition has no prepared scope' USING ERRCODE='55000';
 END IF;
 IF NEW.state='active' THEN
  PERFORM workspace_legal_hold.check_file_mutations();
  IF EXISTS(SELECT 1 FROM workspace_legal_hold.case_members m WHERE m.case_id=NEW.case_id AND m.kind='fileinfo'
   AND NOT EXISTS(SELECT 1 FROM workspace_legal_hold.file_versions v JOIN workspace_legal_hold.file_receipts r ON r.version_id=v.id
    WHERE v.file_id=m.entity_id AND v.id=(SELECT max(id) FROM workspace_legal_hold.file_versions WHERE file_id=m.entity_id))) THEN
   RAISE EXCEPTION 'legal hold case attachments are not sealed' USING ERRCODE='55000';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER validate_event BEFORE INSERT ON workspace_legal_hold.case_events FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.validate_case_event();
ALTER TABLE workspace_legal_hold.case_events ENABLE ALWAYS TRIGGER validate_event;

CREATE FUNCTION workspace_legal_hold.post_cases(p jsonb) RETURNS TABLE(case_id varchar(26)) LANGUAGE sql VOLATILE SET search_path=pg_catalog AS $$
 SELECT s.case_id FROM workspace_legal_hold.case_scopes s
 WHERE (s.kind='users' AND s.target_id=p->>'userid') OR (s.kind='channels' AND s.target_id=p->>'channelid')
 UNION SELECT m.case_id FROM workspace_legal_hold.case_members m WHERE m.kind='posts' AND m.entity_id IN(p->>'id',NULLIF(p->>'originalid',''))
$$;
CREATE FUNCTION workspace_legal_hold.file_cases(f jsonb) RETURNS TABLE(case_id varchar(26)) LANGUAGE sql VOLATILE SET search_path=pg_catalog AS $$
 SELECT s.case_id FROM workspace_legal_hold.case_scopes s
 WHERE (s.kind='users' AND s.target_id=f->>'creatorid') OR (s.kind='channels' AND s.target_id=f->>'channelid')
 UNION SELECT m.case_id FROM workspace_legal_hold.case_members m
 WHERE (m.kind='fileinfo' AND m.entity_id=f->>'id') OR (m.kind='posts' AND m.entity_id=f->>'postid')
$$;
CREATE FUNCTION workspace_legal_hold.case_file_ids(p jsonb) RETURNS SETOF text LANGUAGE plpgsql IMMUTABLE SET search_path=pg_catalog AS $$
DECLARE ids jsonb;
BEGIN
 ids:=p->'fileids';
 IF ids IS NULL OR ids='null'::jsonb OR ids='""'::jsonb THEN RETURN; END IF;
 IF jsonb_typeof(ids)='string' THEN ids:=(ids#>>'{}')::jsonb; END IF;
 IF jsonb_typeof(ids) IS DISTINCT FROM 'array' THEN RAISE EXCEPTION 'legal hold invalid attachment list' USING ERRCODE='23514'; END IF;
 RETURN QUERY SELECT jsonb_array_elements_text(ids);
END $$;
CREATE FUNCTION workspace_legal_hold.capture_case_object(c text,k text,i text,p jsonb,op text) RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE previous jsonb;
BEGIN
 IF k='users' THEN
  SELECT jsonb_object_agg(key,value) INTO p FROM jsonb_each(p) WHERE key=ANY(ARRAY['id','createat','updateat','deleteat','username','email','emailverified','nickname','firstname','lastname','roles','position','locale','remoteid','timezone','authservice']);
 END IF;
 INSERT INTO workspace_legal_hold.case_members VALUES(c,k,i) ON CONFLICT DO NOTHING;
 SELECT data INTO previous FROM workspace_legal_hold.case_versions WHERE case_id=c AND kind=k AND entity_id=i ORDER BY id DESC LIMIT 1;
 IF previous IS DISTINCT FROM p OR op='delete' THEN
  INSERT INTO workspace_legal_hold.case_versions(case_id,kind,entity_id,data,operation) VALUES(c,k,i,p,op);
 END IF;
END $$;
CREATE FUNCTION workspace_legal_hold.capture_case_file(c text,f jsonb,reserve boolean) RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE inserted integer;
BEGIN
 PERFORM workspace_legal_hold.capture_case_object(c,'fileinfo',f->>'id',f,'snapshot');
 IF reserve THEN
  INSERT INTO workspace_legal_hold.file_reservations(file_id) VALUES(f->>'id') ON CONFLICT DO NOTHING;
  GET DIAGNOSTICS inserted=ROW_COUNT;
  IF inserted=1 THEN INSERT INTO workspace_legal_hold.file_versions(file_id,metadata) VALUES(f->>'id',f); END IF;
 END IF;
END $$;
CREATE FUNCTION workspace_legal_hold.capture_case_post(c text,p jsonb,op text) RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE row jsonb; file_id text;
BEGIN
 IF EXISTS(SELECT 1 FROM workspace_legal_hold.case_file_ids(p)) OR EXISTS(SELECT 1 FROM {{fileinfo}} WHERE postid=p->>'id') THEN
  -- The physical intent may have selected this file while it was still
  -- unheld. Do not add a held reference while that writer is running.
  PERFORM workspace_legal_hold.check_file_mutations();
 END IF;
 PERFORM workspace_legal_hold.capture_case_object(c,'posts',p->>'id',p,op);
 PERFORM workspace_legal_hold.lock_case_entity('users',p->>'userid');
 SELECT to_jsonb(u) INTO row FROM {{users}} u WHERE id=p->>'userid';
 IF FOUND THEN PERFORM workspace_legal_hold.capture_case_object(c,'users',p->>'userid',row,'snapshot'); END IF;
 PERFORM workspace_legal_hold.lock_case_entity('channels',p->>'channelid');
 SELECT to_jsonb(ch) INTO row FROM {{channels}} ch WHERE id=p->>'channelid';
 IF FOUND THEN PERFORM workspace_legal_hold.capture_case_object(c,'channels',p->>'channelid',row,'snapshot'); END IF;
 SELECT to_jsonb(t) INTO row FROM {{temporaryposts}} t WHERE postid=p->>'id';
 IF FOUND THEN PERFORM workspace_legal_hold.capture_case_temporary(c,row,'snapshot'); END IF;
 FOR file_id IN SELECT workspace_legal_hold.case_file_ids(p) UNION SELECT f.id FROM {{fileinfo}} f WHERE postid=p->>'id' LOOP
  PERFORM workspace_legal_hold.lock_case_entity('fileinfo',file_id);
  INSERT INTO workspace_legal_hold.case_members VALUES(c,'fileinfo',file_id) ON CONFLICT DO NOTHING;
  SELECT to_jsonb(f) INTO row FROM {{fileinfo}} f WHERE id=file_id;
  IF FOUND THEN PERFORM workspace_legal_hold.capture_case_file(c,row,false); END IF;
 END LOOP;
END $$;
CREATE FUNCTION workspace_legal_hold.capture_case_temporary(c text,p jsonb,op text) RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE row jsonb; file_id text;
BEGIN
 IF EXISTS(SELECT 1 FROM workspace_legal_hold.case_file_ids(p)) THEN
  PERFORM workspace_legal_hold.check_file_mutations();
 END IF;
 PERFORM workspace_legal_hold.capture_case_object(c,'temporaryposts',p->>'postid',p,op);
 FOR file_id IN SELECT workspace_legal_hold.case_file_ids(p) LOOP
  PERFORM workspace_legal_hold.lock_case_entity('fileinfo',file_id);
  INSERT INTO workspace_legal_hold.case_members VALUES(c,'fileinfo',file_id) ON CONFLICT DO NOTHING;
  SELECT to_jsonb(f) INTO row FROM {{fileinfo}} f WHERE id=file_id;
  IF FOUND THEN PERFORM workspace_legal_hold.capture_case_file(c,row,false); END IF;
 END LOOP;
END $$;
CREATE FUNCTION workspace_legal_hold.guard_case_row() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE p jsonb; old_p jsonb; c text; k text:=TG_ARGV[0]; i text;
BEGIN
 PERFORM workspace_legal_hold.case_barrier();
 IF TG_OP<>'INSERT' THEN old_p:=to_jsonb(OLD); END IF;
 IF TG_OP<>'DELETE' THEN p:=to_jsonb(NEW); END IF;
 i:=COALESCE(old_p->>'id',p->>'id',old_p->>'postid',p->>'postid');
 PERFORM workspace_legal_hold.lock_case_entity(CASE WHEN k='temporaryposts' THEN 'posts' ELSE k END,i);
 IF TG_WHEN='BEFORE' AND TG_OP='UPDATE' AND COALESCE(p->>'id',p->>'postid') IS DISTINCT FROM i AND EXISTS(SELECT 1 FROM workspace_legal_hold.case_members WHERE kind=k AND entity_id IN(i,COALESCE(p->>'id',p->>'postid'))) THEN
  RAISE EXCEPTION 'legal hold observed entity ID cannot change' USING ERRCODE='55000';
 END IF;
 IF TG_WHEN='BEFORE' AND TG_OP='INSERT' AND k<>'temporaryposts' AND EXISTS(SELECT 1 FROM workspace_legal_hold.case_members WHERE kind=k AND entity_id=i) THEN
  RAISE EXCEPTION 'legal hold observed entity ID cannot be reused' USING ERRCODE='55000';
 END IF;
 IF TG_WHEN='AFTER' THEN old_p:=NULL; END IF;
 IF k='posts' THEN
  FOR c IN SELECT case_id FROM workspace_legal_hold.post_cases(COALESCE(old_p,p)) UNION SELECT case_id FROM workspace_legal_hold.post_cases(p) LOOP
   IF old_p IS NOT NULL THEN PERFORM workspace_legal_hold.capture_case_post(c,old_p,CASE WHEN TG_OP='DELETE' THEN 'delete' ELSE 'snapshot' END); END IF;
   IF p IS NOT NULL AND TG_WHEN='AFTER' THEN PERFORM workspace_legal_hold.capture_case_post(c,p,lower(TG_OP)); END IF;
  END LOOP;
 ELSIF k='temporaryposts' THEN
  FOR c IN SELECT case_id FROM workspace_legal_hold.case_members WHERE kind IN('posts','temporaryposts') AND entity_id=i
   UNION SELECT pc.case_id FROM {{posts}} src CROSS JOIN LATERAL workspace_legal_hold.post_cases(to_jsonb(src)) pc WHERE src.id=i LOOP
   IF old_p IS NOT NULL THEN PERFORM workspace_legal_hold.capture_case_temporary(c,old_p,CASE WHEN TG_OP='DELETE' THEN 'delete' ELSE 'snapshot' END); END IF;
   IF p IS NOT NULL AND TG_WHEN='AFTER' THEN PERFORM workspace_legal_hold.capture_case_temporary(c,p,lower(TG_OP)); END IF;
  END LOOP;
 ELSE
  FOR c IN SELECT case_id FROM workspace_legal_hold.case_scopes WHERE kind=k AND target_id=i
   UNION SELECT case_id FROM workspace_legal_hold.case_members WHERE kind=k AND entity_id=i LOOP
   IF old_p IS NOT NULL THEN PERFORM workspace_legal_hold.capture_case_object(c,k,i,old_p,CASE WHEN TG_OP='DELETE' THEN 'delete' ELSE 'snapshot' END); END IF;
   IF p IS NOT NULL AND TG_WHEN='AFTER' THEN PERFORM workspace_legal_hold.capture_case_object(c,k,i,p,lower(TG_OP)); END IF;
  END LOOP;
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END $$;
CREATE FUNCTION workspace_legal_hold.guard_case_file_row() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE c text;
BEGIN
 PERFORM workspace_legal_hold.case_barrier();
 PERFORM workspace_legal_hold.lock_case_entity('fileinfo',CASE WHEN TG_OP='INSERT' THEN NEW.id ELSE OLD.id END);
 IF TG_OP='INSERT' THEN RETURN NEW; END IF;
 IF TG_OP='UPDATE' AND NEW.id IS DISTINCT FROM OLD.id AND EXISTS(SELECT 1 FROM workspace_legal_hold.case_members WHERE kind='fileinfo' AND entity_id IN(OLD.id,NEW.id)) THEN
  RAISE EXCEPTION 'legal hold observed file ID cannot change' USING ERRCODE='55000';
 END IF;
 FOR c IN SELECT case_id FROM workspace_legal_hold.file_cases(to_jsonb(OLD)) LOOP
  -- An initial attachment changes PostId/ChannelId after Posts is saved. Do
  -- not reserve its old, unattached metadata before that safe transition.
  -- Changing byte identity or deleting metadata requires a sealed preimage.
  PERFORM workspace_legal_hold.capture_case_file(c,to_jsonb(OLD),TG_OP='DELETE' OR
   (to_jsonb(OLD)->'path',to_jsonb(OLD)->'size') IS DISTINCT FROM (to_jsonb(NEW)->'path',to_jsonb(NEW)->'size') OR
   (COALESCE(OLD.previewpath,'')<>'' AND OLD.previewpath IS DISTINCT FROM NEW.previewpath) OR
   (COALESCE(OLD.thumbnailpath,'')<>'' AND OLD.thumbnailpath IS DISTINCT FROM NEW.thumbnailpath));
 END LOOP;
 IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END $$;
CREATE FUNCTION workspace_legal_hold.capture_case_file_after() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE c text;
BEGIN
 FOR c IN SELECT case_id FROM workspace_legal_hold.file_cases(to_jsonb(NEW)) LOOP
  -- New uploads may still be producing their derivatives. Record membership
  -- immediately; the physical coordinator reserves/seals before destructive
  -- changes, and the BEFORE trigger refuses unsealed metadata destruction.
  PERFORM workspace_legal_hold.capture_case_file(c,to_jsonb(NEW),false);
 END LOOP;
 RETURN NEW;
END $$;
CREATE FUNCTION workspace_legal_hold.guard_case_truncate() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 PERFORM workspace_legal_hold.case_barrier();
 IF EXISTS(SELECT 1 FROM workspace_legal_hold.cases) THEN RAISE EXCEPTION 'legal hold case prevents source truncate' USING ERRCODE='55000'; END IF;
 RETURN NULL;
END $$;
CREATE FUNCTION workspace_legal_hold.discover_case_files(paths jsonb,recursive boolean) RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE f jsonb; c text;
BEGIN
 FOR f IN SELECT to_jsonb(src) FROM {{fileinfo}} src WHERE EXISTS(
  SELECT 1 FROM jsonb_array_elements_text(paths) targets(target_path),
  unnest(ARRAY[src.path,src.previewpath,src.thumbnailpath]) parts(object_path)
  WHERE object_path<>'' AND (object_path=target_path OR recursive AND (target_path='.' OR starts_with(object_path,target_path||'/')))) LOOP
  FOR c IN SELECT case_id FROM workspace_legal_hold.file_cases(f) LOOP
   PERFORM workspace_legal_hold.capture_case_file(c,f,true);
  END LOOP;
 END LOOP;
END $$;
`
