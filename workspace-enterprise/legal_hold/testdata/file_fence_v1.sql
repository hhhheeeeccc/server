
CREATE SCHEMA workspace_legal_hold;
CREATE TABLE workspace_legal_hold.installation(version integer PRIMARY KEY CHECK(version=1),source_oid oid NOT NULL);
CREATE TABLE workspace_legal_hold.control(singleton boolean PRIMARY KEY CHECK(singleton),revision bigint NOT NULL);
INSERT INTO workspace_legal_hold.control VALUES(true,0);
CREATE TABLE workspace_legal_hold.file_reservations(file_id varchar(26) PRIMARY KEY,reserved_at timestamptz NOT NULL DEFAULT clock_timestamp());
CREATE TABLE workspace_legal_hold.file_versions(id bigserial PRIMARY KEY,file_id varchar(26) NOT NULL REFERENCES workspace_legal_hold.file_reservations(file_id),metadata jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp());
CREATE INDEX ON workspace_legal_hold.file_versions(file_id,id DESC);
CREATE TABLE workspace_legal_hold.file_receipts(version_id bigint PRIMARY KEY REFERENCES workspace_legal_hold.file_versions(id),manifest jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp());

CREATE FUNCTION workspace_legal_hold.reject_journal_mutation() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN RAISE EXCEPTION 'legal hold journal is append only' USING ERRCODE='55000'; END $$;
DO $$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['installation','file_reservations','file_versions','file_receipts'] LOOP
  EXECUTE format('CREATE TRIGGER immutable_rows BEFORE UPDATE OR DELETE ON workspace_legal_hold.%I FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('CREATE TRIGGER immutable_truncate BEFORE TRUNCATE ON workspace_legal_hold.%I FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_rows',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_truncate',name);
 END LOOP;
END $$;

CREATE FUNCTION workspace_legal_hold.guard_control() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF TG_OP<>'UPDATE' THEN RAISE EXCEPTION 'legal hold coordination row cannot be removed' USING ERRCODE='55000'; END IF;
 IF NEW.singleton IS DISTINCT FROM OLD.singleton OR NEW.revision IS DISTINCT FROM OLD.revision+1 THEN
  RAISE EXCEPTION 'legal hold coordination revision must advance' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER control_rows BEFORE UPDATE OR DELETE ON workspace_legal_hold.control FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.guard_control();
CREATE TRIGGER control_truncate BEFORE TRUNCATE ON workspace_legal_hold.control FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.guard_control();
ALTER TABLE workspace_legal_hold.control ENABLE ALWAYS TRIGGER control_rows;
ALTER TABLE workspace_legal_hold.control ENABLE ALWAYS TRIGGER control_truncate;

CREATE FUNCTION workspace_legal_hold.validate_file_receipt() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE metadata jsonb; part record; object jsonb; expected integer:=0;
BEGIN
 SELECT v.metadata INTO STRICT metadata FROM workspace_legal_hold.file_versions v WHERE v.id=NEW.version_id;
 IF jsonb_typeof(NEW.manifest) IS DISTINCT FROM 'array' OR COALESCE(metadata->>'path','')='' THEN
  RAISE EXCEPTION 'legal hold manifest has no original' USING ERRCODE='23514';
 END IF;
 FOR part IN SELECT * FROM (VALUES ('original',metadata->>'path'),('thumbnail',metadata->>'thumbnailpath'),('preview',metadata->>'previewpath')) AS p(role,path) LOOP
  IF COALESCE(part.path,'')='' THEN CONTINUE; END IF;
  expected:=expected+1;
  IF (SELECT count(*) FROM jsonb_array_elements(NEW.manifest) e WHERE e->>'role'=part.role)<>1 THEN
   RAISE EXCEPTION 'legal hold manifest role missing or duplicated' USING ERRCODE='23514';
  END IF;
  SELECT e INTO object FROM jsonb_array_elements(NEW.manifest) e WHERE e->>'role'=part.role;
  IF object->>'path' IS DISTINCT FROM part.path OR COALESCE(object->>'sha256','') !~ '^[0-9a-f]{64}$'
   OR jsonb_typeof(object->'size') IS DISTINCT FROM 'number' OR COALESCE(object->>'size','') !~ '^[0-9]+$'
   OR (part.role='original' AND (object->>'size')::bigint IS DISTINCT FROM (metadata->>'size')::bigint) THEN
   RAISE EXCEPTION 'legal hold manifest path, digest or size mismatch' USING ERRCODE='23514';
  END IF;
 END LOOP;
 IF jsonb_array_length(NEW.manifest)<>expected THEN
  RAISE EXCEPTION 'legal hold manifest has unexpected objects' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER validate_receipt BEFORE INSERT ON workspace_legal_hold.file_receipts FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.validate_file_receipt();
ALTER TABLE workspace_legal_hold.file_receipts ENABLE ALWAYS TRIGGER validate_receipt;

CREATE FUNCTION workspace_legal_hold.guard_file_row() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE latest workspace_legal_hold.file_versions%ROWTYPE;
BEGIN
 -- Row locks, rather than advisory locks alone, also detect a stale snapshot
 -- under REPEATABLE READ/SERIALIZABLE when reservation increments revision.
 PERFORM revision FROM workspace_legal_hold.control WHERE singleton FOR SHARE;
 IF NOT FOUND THEN RAISE EXCEPTION 'legal hold coordination row missing' USING ERRCODE='55000'; END IF;
 IF TG_OP='INSERT' THEN
  IF EXISTS(SELECT 1 FROM workspace_legal_hold.file_reservations WHERE file_id=NEW.id) THEN
   RAISE EXCEPTION 'legal hold reserved file ID cannot be reused' USING ERRCODE='55000';
  END IF;
  RETURN NEW;
 END IF;
 IF TG_OP='UPDATE' AND NEW.id IS DISTINCT FROM OLD.id
  AND EXISTS(SELECT 1 FROM workspace_legal_hold.file_reservations WHERE file_id=NEW.id) THEN
  RAISE EXCEPTION 'legal hold reserved file ID cannot be reused' USING ERRCODE='55000';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM workspace_legal_hold.file_reservations WHERE file_id=OLD.id) THEN
  IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
 END IF;
 SELECT v.* INTO latest FROM workspace_legal_hold.file_versions v WHERE v.file_id=OLD.id ORDER BY v.id DESC LIMIT 1;
 IF NOT FOUND OR latest.metadata IS DISTINCT FROM to_jsonb(OLD)
  OR NOT EXISTS(SELECT 1 FROM workspace_legal_hold.file_receipts WHERE version_id=latest.id) THEN
  RAISE EXCEPTION 'legal hold file preservation is pending' USING ERRCODE='55000';
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 IF NEW.id IS DISTINCT FROM OLD.id THEN
  RAISE EXCEPTION 'legal hold reserved file ID cannot change' USING ERRCODE='55000';
 END IF;
 IF to_jsonb(NEW) IS DISTINCT FROM to_jsonb(OLD) THEN
  INSERT INTO workspace_legal_hold.file_versions(file_id,metadata) VALUES(OLD.id,to_jsonb(NEW));
 END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION workspace_legal_hold.guard_file_truncate() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 PERFORM revision FROM workspace_legal_hold.control WHERE singleton FOR SHARE;
 IF NOT FOUND THEN RAISE EXCEPTION 'legal hold coordination row missing' USING ERRCODE='55000'; END IF;
 IF EXISTS(SELECT 1 FROM workspace_legal_hold.file_reservations) THEN
  RAISE EXCEPTION 'legal hold prevents FileInfo truncate' USING ERRCODE='55000';
 END IF;
 RETURN NULL;
END $$;
