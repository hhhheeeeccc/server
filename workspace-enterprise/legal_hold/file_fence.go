package legal_hold

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
)

var ErrPreservationPending = errors.New("legal hold file preservation is pending")

// FileFence is an internal, unregistered metadata preservation building block.
// It is not a case activation API. FileCoordinator supplies the physical lease
// and path journal; case authorization and message preservation still precede
// production use. Direct callers must coordinate physical writes while sealing.
type FileFence struct {
	db        *sql.DB
	fileTable string
}

type PreservedFileObject struct {
	Role string `json:"role"`
	Path string `json:"path"`
	BlobRef
}

type FileEvidence struct {
	VersionID int64
	FileID    string
	Metadata  json.RawMessage
	Objects   []PreservedFileObject
}

// InstallFileFence explicitly installs the journal and native SQL triggers.
// There is no production call site. It resolves the same unquoted FileInfo as
// native queries and rejects reuse against a different table. DDL is atomic.
func InstallFileFence(ctx context.Context, db *sql.DB) (*FileFence, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='25s'; SET LOCAL synchronous_commit=on`); err != nil {
		return nil, err
	}
	var oid int64
	var schema, table string
	err = tx.QueryRowContext(ctx, `SELECT c.oid,n.nspname,c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.oid=to_regclass('fileinfo') AND c.relkind='r'`).Scan(&oid, &schema, &table)
	if err != nil {
		return nil, fmt.Errorf("resolve legal hold FileInfo table: %w", err)
	}
	qualified := pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(table)
	// Serializes installers and excludes writers while attaching both triggers.
	if _, err = tx.ExecContext(ctx, "LOCK TABLE "+qualified+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return nil, err
	}
	installedVersion := 1
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT to_regnamespace('workspace_legal_hold') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		var version int
		var installedOID int64
		if err = tx.QueryRowContext(ctx, `SELECT version,source_oid FROM workspace_legal_hold.installation ORDER BY version DESC LIMIT 1`).Scan(&version, &installedOID); err != nil {
			return nil, fmt.Errorf("incompatible legal hold schema: %w", err)
		}
		if (version != 1 && version != 2) || installedOID != oid {
			return nil, errors.New("legal hold schema version or FileInfo identity differs")
		}
		installedVersion = version
		var intact bool
		err = tx.QueryRowContext(ctx, `WITH expected(relation,name) AS (
			VALUES($1::oid,'workspace_legal_hold_file_row'),($1::oid,'workspace_legal_hold_file_truncate'),
			('workspace_legal_hold.control'::regclass::oid,'control_rows'),
			('workspace_legal_hold.control'::regclass::oid,'control_truncate'),
			('workspace_legal_hold.file_receipts'::regclass::oid,'validate_receipt')
			UNION ALL SELECT to_regclass('workspace_legal_hold.'||r)::oid,t
			FROM unnest(ARRAY['installation','file_reservations','file_versions','file_receipts']) r
			CROSS JOIN unnest(ARRAY['immutable_rows','immutable_truncate']) t)
			SELECT count(*)=13 AND bool_and(t.oid IS NOT NULL AND t.tgenabled='A')
			FROM expected e LEFT JOIN pg_trigger t ON t.tgrelid=e.relation AND t.tgname=e.name`, oid).Scan(&intact)
		if err != nil || !intact {
			return nil, errors.New("legal hold schema or native triggers are incomplete")
		}
	} else {
		if _, err = tx.ExecContext(ctx, fileFenceSchema); err != nil {
			return nil, fmt.Errorf("install legal hold file journal: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.installation VALUES(1,$1)`, oid); err != nil {
			return nil, err
		}
		for _, statement := range []string{
			"CREATE TRIGGER workspace_legal_hold_file_row BEFORE INSERT OR UPDATE OR DELETE ON " + qualified + " FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.guard_file_row()",
			"CREATE TRIGGER workspace_legal_hold_file_truncate BEFORE TRUNCATE ON " + qualified + " FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.guard_file_truncate()",
			"ALTER TABLE " + qualified + " ENABLE ALWAYS TRIGGER workspace_legal_hold_file_row",
			"ALTER TABLE " + qualified + " ENABLE ALWAYS TRIGGER workspace_legal_hold_file_truncate",
		} {
			if _, err = tx.ExecContext(ctx, statement); err != nil {
				return nil, err
			}
		}
	}
	if installedVersion == 1 {
		if _, err = tx.ExecContext(ctx, fileMutationSchema+fileNativeGuardsSQL); err != nil {
			return nil, fmt.Errorf("upgrade legal hold file mutation journal: %w", err)
		}
	}
	if err = validateFileMutationSchema(ctx, tx); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &FileFence{db: db, fileTable: qualified}, nil
}

func (f *FileFence) begin(ctx context.Context) (*sql.Tx, error) {
	return f.beginControl(ctx, false)
}

func (f *FileFence) beginControl(ctx context.Context, allowMutation bool) (*sql.Tx, error) {
	tx, err := f.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='30s'; SET LOCAL synchronous_commit=on`); err != nil {
		tx.Rollback()
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE workspace_legal_hold.control SET revision=revision+1 WHERE singleton`)
	if err == nil {
		var count int64
		count, err = result.RowsAffected()
		if err == nil && count != 1 {
			err = errors.New("legal hold coordination row missing")
		}
	}
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	if !allowMutation {
		if _, err := tx.ExecContext(ctx, `SELECT workspace_legal_hold.check_file_mutations()`); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	return tx, nil
}

// ReserveFile commits a pending metadata version before any archive work. A
// failed copy therefore cannot turn a required file back into an unheld one.
// An existing reservation is retained. There is deliberately no release API.
func (f *FileFence) ReserveFile(ctx context.Context, fileID string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := f.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = f.reserveFileTx(ctx, tx, fileID); err != nil {
		return err
	}
	return tx.Commit()
}

func (f *FileFence) reserveFileTx(ctx context.Context, tx *sql.Tx, fileID string) error {
	return f.reserveFileOriginTx(ctx, tx, fileID, true)
}

func (f *FileFence) reserveFileOriginTx(ctx context.Context, tx *sql.Tx, fileID string, independent bool) error {
	var metadata []byte
	// Do not take a FileInfo row lock after the control lock: a native writer
	// already owns that row before entering its trigger. The barrier excludes
	// its mutation while this READ COMMITTED query reads the existing preimage.
	if err := tx.QueryRowContext(ctx, "SELECT to_jsonb(f) FROM "+f.fileTable+" f WHERE id=$1", fileID).Scan(&metadata); err != nil {
		return err
	}
	installed, err := lifecycleInstalled(ctx, tx)
	if err != nil {
		return err
	}
	if installed {
		_, err = tx.ExecContext(ctx, `SELECT workspace_legal_hold.reserve_file_metadata($1,$2::jsonb,$3)`, fileID, string(metadata), independent)
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.file_reservations(file_id) VALUES($1) ON CONFLICT DO NOTHING`, fileID)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil || inserted == 0 {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.file_versions(file_id,metadata) VALUES($1,$2::jsonb)`, fileID, string(metadata))
	return err
}

func readFileEvidence(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, fileID string) (FileEvidence, error) {
	evidence := FileEvidence{FileID: fileID}
	var metadata, manifest []byte
	err := query.QueryRowContext(ctx, `SELECT v.id,v.metadata,r.manifest FROM workspace_legal_hold.file_versions v
		LEFT JOIN workspace_legal_hold.file_receipts r ON r.version_id=v.id
		WHERE v.file_id=$1 ORDER BY v.id DESC LIMIT 1`, fileID).Scan(&evidence.VersionID, &metadata, &manifest)
	if err != nil {
		return evidence, err
	}
	evidence.Metadata = json.RawMessage(metadata)
	if manifest == nil {
		return evidence, ErrPreservationPending
	}
	err = json.Unmarshal(manifest, &evidence.Objects)
	return evidence, err
}

// LatestFileEvidence returns the immutable latest metadata version and its
// committed manifest. A pending version returns its metadata with a sentinel.
func (f *FileFence) LatestFileEvidence(ctx context.Context, fileID string) (FileEvidence, error) {
	return readFileEvidence(ctx, f.db, fileID)
}

// PreservePendingFile seals the latest pending metadata version with actual
// vault copies and commits their references before returning success. Reopening
// an already sealed version verifies its archive bytes, including after native
// removal. It does not assert that a live path has never been overwritten since
// sealing; FileCoordinator separately journals physical path generations.
func (f *FileFence) PreservePendingFile(ctx context.Context, fileID string, backend filestore.FileBackend, vault *LocalVault) (FileEvidence, error) {
	if backend == nil || vault == nil {
		return FileEvidence{}, errors.New("legal hold source and archive are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tx, err := f.begin(ctx)
	if err != nil {
		return FileEvidence{}, err
	}
	defer tx.Rollback()
	evidence, err := preservePendingFileTx(ctx, tx, fileID, backend, vault)
	if err != nil {
		return evidence, err
	}
	return evidence, tx.Commit()
}

func preservePendingFileTx(ctx context.Context, tx *sql.Tx, fileID string, backend filestore.FileBackend, vault *LocalVault) (FileEvidence, error) {
	evidence, err := readFileEvidence(ctx, tx, fileID)
	if err != nil && !errors.Is(err, ErrPreservationPending) {
		return evidence, err
	}
	if err == nil {
		for _, object := range evidence.Objects {
			file, err := vault.OpenVerified(object.BlobRef)
			if err != nil {
				return evidence, err
			}
			if err = file.Close(); err != nil {
				return evidence, err
			}
		}
		return evidence, nil
	}
	var metadata struct {
		Path          string `json:"path"`
		ThumbnailPath string `json:"thumbnailpath"`
		PreviewPath   string `json:"previewpath"`
		Size          *int64 `json:"size"`
	}
	if err = json.Unmarshal(evidence.Metadata, &metadata); err != nil {
		return evidence, err
	}
	if metadata.Path == "" || metadata.Size == nil || *metadata.Size < 0 {
		return evidence, errors.New("legal hold file metadata has no valid original path and size")
	}
	for _, object := range []PreservedFileObject{
		{Role: "original", Path: metadata.Path},
		{Role: "thumbnail", Path: metadata.ThumbnailPath},
		{Role: "preview", Path: metadata.PreviewPath},
	} {
		if object.Path == "" {
			continue
		}
		size, err := backend.FileSize(object.Path)
		if err != nil {
			return evidence, err
		}
		if object.Role == "original" && size != *metadata.Size {
			return evidence, errors.New("legal hold original size differs from metadata")
		}
		reader, err := backend.Reader(object.Path)
		if err != nil {
			return evidence, err
		}
		stop := context.AfterFunc(ctx, func() { _ = reader.Close() })
		ref, copyErr := vault.Put(ctx, reader, size)
		stop()
		if err = errors.Join(copyErr, reader.Close(), ctx.Err()); err != nil {
			return evidence, err
		}
		object.BlobRef = ref
		evidence.Objects = append(evidence.Objects, object)
	}
	manifest, err := json.Marshal(evidence.Objects)
	if err != nil {
		return evidence, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.file_receipts(version_id,manifest) VALUES($1,$2::jsonb)`, evidence.VersionID, string(manifest)); err != nil {
		return evidence, err
	}
	return evidence, nil
}

const fileFenceSchema = `
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

` + fileNativeGuardsSQL

const fileNativeGuardsSQL = `CREATE OR REPLACE FUNCTION workspace_legal_hold.guard_file_row() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE latest workspace_legal_hold.file_versions%ROWTYPE;
BEGIN
 -- Row locks, rather than advisory locks alone, also detect a stale snapshot
 -- under REPEATABLE READ/SERIALIZABLE when reservation increments revision.
 PERFORM revision FROM workspace_legal_hold.control WHERE singleton FOR SHARE;
 IF NOT FOUND THEN RAISE EXCEPTION 'legal hold coordination row missing' USING ERRCODE='55000'; END IF;
 PERFORM workspace_legal_hold.check_file_mutations();
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

CREATE OR REPLACE FUNCTION workspace_legal_hold.guard_file_truncate() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 PERFORM revision FROM workspace_legal_hold.control WHERE singleton FOR SHARE;
 IF NOT FOUND THEN RAISE EXCEPTION 'legal hold coordination row missing' USING ERRCODE='55000'; END IF;
 PERFORM workspace_legal_hold.check_file_mutations();
 IF EXISTS(SELECT 1 FROM workspace_legal_hold.file_reservations) THEN
  RAISE EXCEPTION 'legal hold prevents FileInfo truncate' USING ERRCODE='55000';
 END IF;
 RETURN NULL;
END $$;
`
