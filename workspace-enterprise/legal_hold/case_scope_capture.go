package legal_hold

// Provenance is per scope activation, not per case alone. Removing a user must
// retain protection supplied by a channel (or another case), without extending
// capture for that user indefinitely through historical case_members.
const caseScopeCapture = `
CREATE FUNCTION workspace_legal_hold.object_scope_origins(c text,k text,i text) RETURNS TABLE(kind text,target_id text,activation bigint) LANGUAGE sql VOLATILE SET search_path=pg_catalog AS $$
 SELECT s.kind,s.target_id::text,s.activation FROM workspace_legal_hold.effective_case_scopes s WHERE s.case_id=c AND s.kind=k AND s.target_id=i
 UNION SELECT m.scope_kind,m.scope_target::text,m.activation FROM workspace_legal_hold.active_case_scope_members m WHERE m.case_id=c AND m.kind=k AND m.entity_id=i
$$;
CREATE FUNCTION workspace_legal_hold.post_scope_origins(c text,p jsonb) RETURNS TABLE(kind text,target_id text,activation bigint) LANGUAGE sql VOLATILE SET search_path=pg_catalog AS $$
 SELECT s.kind,s.target_id::text,s.activation FROM workspace_legal_hold.effective_case_scopes s WHERE s.case_id=c
 AND ((s.kind='users' AND s.target_id=p->>'userid') OR (s.kind='channels' AND s.target_id=p->>'channelid'))
 UNION SELECT m.scope_kind,m.scope_target::text,m.activation FROM workspace_legal_hold.active_case_scope_members m
 WHERE m.case_id=c AND m.kind='posts' AND m.entity_id IN(p->>'id',NULLIF(p->>'originalid',''))
$$;
CREATE FUNCTION workspace_legal_hold.file_scope_origins(c text,f jsonb) RETURNS TABLE(kind text,target_id text,activation bigint) LANGUAGE sql VOLATILE SET search_path=pg_catalog AS $$
 SELECT s.kind,s.target_id::text,s.activation FROM workspace_legal_hold.effective_case_scopes s WHERE s.case_id=c
 AND ((s.kind='users' AND s.target_id=f->>'creatorid') OR (s.kind='channels' AND s.target_id=f->>'channelid'))
 UNION SELECT m.scope_kind,m.scope_target::text,m.activation FROM workspace_legal_hold.active_case_scope_members m
 WHERE m.case_id=c AND ((m.kind='fileinfo' AND m.entity_id=f->>'id') OR (m.kind='posts' AND m.entity_id=f->>'postid'))
$$;
CREATE FUNCTION workspace_legal_hold.link_scope_object(c text,sk text,si text,a bigint,k text,i text,p jsonb,op text) RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NOT workspace_legal_hold.case_is_open(c) THEN RETURN; END IF;
 INSERT INTO workspace_legal_hold.case_scope_members(case_id,scope_kind,scope_target,activation,kind,entity_id) VALUES(c,sk,si,a,k,i) ON CONFLICT DO NOTHING;
 PERFORM workspace_legal_hold.record_case_object(c,k,i,p,op);
END $$;
CREATE OR REPLACE FUNCTION workspace_legal_hold.capture_case_object(c text,k text,i text,p jsonb,op text) RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE s record;
BEGIN
 IF NOT workspace_legal_hold.case_is_open(c) THEN RETURN; END IF;
 FOR s IN SELECT * FROM workspace_legal_hold.object_scope_origins(c,k,i) LOOP
  PERFORM workspace_legal_hold.link_scope_object(c,s.kind,s.target_id,s.activation,k,i,p,op);
 END LOOP;
END $$;
CREATE OR REPLACE FUNCTION workspace_legal_hold.capture_case_file(c text,f jsonb,reserve boolean) RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE s record; matched boolean:=false;
BEGIN
 IF NOT workspace_legal_hold.case_is_open(c) THEN RETURN; END IF;
 FOR s IN SELECT * FROM workspace_legal_hold.file_scope_origins(c,f) LOOP
  PERFORM workspace_legal_hold.link_scope_object(c,s.kind,s.target_id,s.activation,'fileinfo',f->>'id',f,'snapshot');
  matched:=true;
 END LOOP;
 IF matched AND reserve THEN PERFORM workspace_legal_hold.reserve_file_metadata(f->>'id',f,false); END IF;
END $$;
CREATE FUNCTION workspace_legal_hold.link_scope_temporary(c text,sk text,si text,a bigint,p jsonb,op text) RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE row jsonb; file_id text;
BEGIN
 IF EXISTS(SELECT 1 FROM workspace_legal_hold.case_file_ids(p)) THEN PERFORM workspace_legal_hold.check_file_mutations(); END IF;
 PERFORM workspace_legal_hold.link_scope_object(c,sk,si,a,'temporaryposts',p->>'postid',p,op);
 FOR file_id IN SELECT workspace_legal_hold.case_file_ids(p) LOOP
  PERFORM workspace_legal_hold.lock_case_entity('fileinfo',file_id);
  INSERT INTO workspace_legal_hold.case_scope_members(case_id,scope_kind,scope_target,activation,kind,entity_id) VALUES(c,sk,si,a,'fileinfo',file_id) ON CONFLICT DO NOTHING;
  INSERT INTO workspace_legal_hold.case_members VALUES(c,'fileinfo',file_id) ON CONFLICT DO NOTHING;
  SELECT to_jsonb(f) INTO row FROM {{fileinfo}} f WHERE id=file_id;
  IF FOUND THEN PERFORM workspace_legal_hold.capture_case_file(c,row,false); END IF;
 END LOOP;
END $$;
CREATE OR REPLACE FUNCTION workspace_legal_hold.capture_case_temporary(c text,p jsonb,op text) RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE s record;
BEGIN
 IF NOT workspace_legal_hold.case_is_open(c) THEN RETURN; END IF;
 FOR s IN
  SELECT * FROM workspace_legal_hold.object_scope_origins(c,'temporaryposts',p->>'postid')
  UNION SELECT m.scope_kind,m.scope_target::text,m.activation FROM workspace_legal_hold.active_case_scope_members m WHERE m.case_id=c AND m.kind='posts' AND m.entity_id=p->>'postid'
  UNION SELECT o.* FROM {{posts}} src CROSS JOIN LATERAL workspace_legal_hold.post_scope_origins(c,to_jsonb(src)) o WHERE src.id=p->>'postid'
 LOOP
  PERFORM workspace_legal_hold.link_scope_temporary(c,s.kind,s.target_id,s.activation,p,op);
 END LOOP;
END $$;
CREATE OR REPLACE FUNCTION workspace_legal_hold.capture_case_post(c text,p jsonb,op text) RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE s record; row jsonb; file_id text;
BEGIN
 IF NOT workspace_legal_hold.case_is_open(c) THEN RETURN; END IF;
 FOR s IN SELECT * FROM workspace_legal_hold.post_scope_origins(c,p) LOOP
  IF EXISTS(SELECT 1 FROM workspace_legal_hold.case_file_ids(p)) OR EXISTS(SELECT 1 FROM {{fileinfo}} WHERE postid=p->>'id') THEN
   PERFORM workspace_legal_hold.check_file_mutations();
  END IF;
  PERFORM workspace_legal_hold.link_scope_object(c,s.kind,s.target_id,s.activation,'posts',p->>'id',p,op);
  PERFORM workspace_legal_hold.lock_case_entity('users',p->>'userid');
  SELECT to_jsonb(u) INTO row FROM {{users}} u WHERE id=p->>'userid';
  IF FOUND THEN PERFORM workspace_legal_hold.link_scope_object(c,s.kind,s.target_id,s.activation,'users',p->>'userid',row,'snapshot'); END IF;
  PERFORM workspace_legal_hold.lock_case_entity('channels',p->>'channelid');
  SELECT to_jsonb(ch) INTO row FROM {{channels}} ch WHERE id=p->>'channelid';
  IF FOUND THEN PERFORM workspace_legal_hold.link_scope_object(c,s.kind,s.target_id,s.activation,'channels',p->>'channelid',row,'snapshot'); END IF;
  SELECT to_jsonb(t) INTO row FROM {{temporaryposts}} t WHERE postid=p->>'id';
  IF FOUND THEN PERFORM workspace_legal_hold.link_scope_temporary(c,s.kind,s.target_id,s.activation,row,'snapshot'); END IF;
  FOR file_id IN SELECT workspace_legal_hold.case_file_ids(p) UNION SELECT f.id FROM {{fileinfo}} f WHERE postid=p->>'id' LOOP
   PERFORM workspace_legal_hold.lock_case_entity('fileinfo',file_id);
   INSERT INTO workspace_legal_hold.case_scope_members(case_id,scope_kind,scope_target,activation,kind,entity_id) VALUES(c,s.kind,s.target_id,s.activation,'fileinfo',file_id) ON CONFLICT DO NOTHING;
   INSERT INTO workspace_legal_hold.case_members VALUES(c,'fileinfo',file_id) ON CONFLICT DO NOTHING;
   SELECT to_jsonb(f) INTO row FROM {{fileinfo}} f WHERE id=file_id;
   IF FOUND THEN PERFORM workspace_legal_hold.capture_case_file(c,row,false); END IF;
  END LOOP;
 END LOOP;
END $$;
CREATE OR REPLACE FUNCTION workspace_legal_hold.post_cases(p jsonb) RETURNS TABLE(case_id varchar(26)) LANGUAGE sql VOLATILE SET search_path=pg_catalog AS $$
 SELECT s.case_id FROM workspace_legal_hold.effective_case_scopes s WHERE (s.kind='users' AND s.target_id=p->>'userid') OR (s.kind='channels' AND s.target_id=p->>'channelid')
 UNION SELECT m.case_id FROM workspace_legal_hold.active_case_scope_members m WHERE m.kind='posts' AND m.entity_id IN(p->>'id',NULLIF(p->>'originalid',''))
$$;
CREATE OR REPLACE FUNCTION workspace_legal_hold.file_cases(f jsonb) RETURNS TABLE(case_id varchar(26)) LANGUAGE sql VOLATILE SET search_path=pg_catalog AS $$
 SELECT s.case_id FROM workspace_legal_hold.effective_case_scopes s WHERE (s.kind='users' AND s.target_id=f->>'creatorid') OR (s.kind='channels' AND s.target_id=f->>'channelid')
 UNION SELECT m.case_id FROM workspace_legal_hold.active_case_scope_members m WHERE (m.kind='fileinfo' AND m.entity_id=f->>'id') OR (m.kind='posts' AND m.entity_id=f->>'postid')
$$;
`

// Backfill only links justified by historical case evidence. No new source
// snapshot is fabricated, and no independent legacy reservation is removed.
const caseScopeBackfill = `
INSERT INTO workspace_legal_hold.case_scope_members(case_id,scope_kind,scope_target,activation,kind,entity_id)
 SELECT s.case_id,s.kind,s.target_id,0,m.kind,m.entity_id FROM workspace_legal_hold.case_scopes s
 JOIN workspace_legal_hold.case_members m ON m.case_id=s.case_id AND m.kind=s.kind AND m.entity_id=s.target_id ON CONFLICT DO NOTHING;
INSERT INTO workspace_legal_hold.case_scope_members(case_id,scope_kind,scope_target,activation,kind,entity_id)
 SELECT DISTINCT s.case_id,s.kind,s.target_id,0,'posts',v.entity_id FROM workspace_legal_hold.case_scopes s
 JOIN workspace_legal_hold.case_versions v ON v.case_id=s.case_id AND v.kind='posts'
 AND ((s.kind='users' AND s.target_id=v.data->>'userid') OR (s.kind='channels' AND s.target_id=v.data->>'channelid')) ON CONFLICT DO NOTHING;
-- History rows can refer to the original post even if their own scope fields
-- changed. Reach a fixed point over preserved, rather than current, rows.
DO $$ DECLARE n integer; BEGIN LOOP
 INSERT INTO workspace_legal_hold.case_scope_members(case_id,scope_kind,scope_target,activation,kind,entity_id)
 SELECT DISTINCT m.case_id,m.scope_kind,m.scope_target,0,'posts',v.entity_id FROM workspace_legal_hold.case_scope_members m
 JOIN workspace_legal_hold.case_versions v ON v.case_id=m.case_id AND v.kind='posts' AND v.data->>'originalid'=m.entity_id WHERE m.kind='posts' ON CONFLICT DO NOTHING;
 GET DIAGNOSTICS n=ROW_COUNT; EXIT WHEN n=0;
END LOOP; END $$;
INSERT INTO workspace_legal_hold.case_scope_members(case_id,scope_kind,scope_target,activation,kind,entity_id)
 SELECT DISTINCT m.case_id,m.scope_kind,m.scope_target,0,dep.kind,dep.entity_id FROM workspace_legal_hold.case_scope_members m
 JOIN workspace_legal_hold.case_versions v ON v.case_id=m.case_id AND v.kind='posts' AND v.entity_id=m.entity_id
 CROSS JOIN LATERAL(VALUES('users',v.data->>'userid'),('channels',v.data->>'channelid'),('temporaryposts',v.entity_id)) dep(kind,entity_id)
 JOIN workspace_legal_hold.case_members old ON old.case_id=m.case_id AND old.kind=dep.kind AND old.entity_id=dep.entity_id WHERE m.kind='posts' ON CONFLICT DO NOTHING;
INSERT INTO workspace_legal_hold.case_scope_members(case_id,scope_kind,scope_target,activation,kind,entity_id)
 SELECT DISTINCT s.case_id,s.kind,s.target_id,0,'fileinfo',v.entity_id FROM workspace_legal_hold.case_scopes s
 JOIN workspace_legal_hold.case_versions v ON v.case_id=s.case_id AND v.kind='fileinfo'
 AND ((s.kind='users' AND s.target_id=v.data->>'creatorid') OR (s.kind='channels' AND s.target_id=v.data->>'channelid'))
 UNION SELECT DISTINCT m.case_id,m.scope_kind,m.scope_target,0,'fileinfo',f.entity_id FROM workspace_legal_hold.case_scope_members m
 JOIN workspace_legal_hold.case_versions f ON f.case_id=m.case_id AND f.kind='fileinfo' AND f.data->>'postid'=m.entity_id WHERE m.kind='posts'
 UNION SELECT DISTINCT m.case_id,m.scope_kind,m.scope_target,0,'fileinfo',old.entity_id FROM workspace_legal_hold.case_scope_members m
 JOIN workspace_legal_hold.case_versions v ON v.case_id=m.case_id AND v.kind=m.kind AND v.entity_id=m.entity_id
 CROSS JOIN LATERAL workspace_legal_hold.case_file_ids(v.data) f(file_id)
 JOIN workspace_legal_hold.case_members old ON old.case_id=m.case_id AND old.kind='fileinfo' AND old.entity_id=f.file_id WHERE m.kind IN('posts','temporaryposts') ON CONFLICT DO NOTHING;
-- Refuse an ambiguous upgrade rather than dropping protection or assigning
-- every member to every scope. The old schema and its guards remain on rollback.
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM workspace_legal_hold.case_members m WHERE NOT EXISTS(SELECT 1 FROM workspace_legal_hold.case_scope_members s WHERE (s.case_id,s.kind,s.entity_id)=(m.case_id,m.kind,m.entity_id))) THEN
  RAISE EXCEPTION 'legal hold legacy membership has no provable scope origin' USING ERRCODE='55000';
 END IF;
END $$;
`
