package legal_hold

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

var ErrExportLimit = errors.New("legal hold export working space limit exceeded")

// CaseExporter creates private, verified artifacts. No HTTP routes, background
// worker or production installation are registered by this package.
type CaseExporter struct {
	manager *CaseManager
	root    *os.Root
	limit   int64
	storeID string
}

type CaseExportResult struct {
	ID     string `json:"id"`
	CaseID string `json:"case_id"`
	BlobRef
	ManifestSHA256 string `json:"manifest_sha256"`
}

type CaseExportDataset struct {
	Path string `json:"path"`
	Rows int64  `json:"rows"`
	BlobRef
}

type CaseExportManifest struct {
	Version    int                 `json:"version"`
	ExportID   string              `json:"export_id"`
	CaseID     string              `json:"case_id"`
	ActorID    string              `json:"actor_id"`
	ArchiveID  string              `json:"archive_id"`
	CapturedAt time.Time           `json:"captured_at"`
	Datasets   []CaseExportDataset `json:"datasets"`
	Release    *CaseReleaseInfo    `json:"release,omitempty"`
}

// NewCaseExporter requires an explicitly installed export journal and an
// existing private directory separate from the vault and live file stores.
// maxWorkingBytes bounds spool, new durable metadata copies and ZIP writes
// per attempt, not only the final artifact. Cooperating processes serialize
// work through the bound shared directory; artifact retention needs an
// operator policy before registering an HTTP endpoint.
func NewCaseExporter(manager *CaseManager, directory string, maxWorkingBytes int64) (*CaseExporter, error) {
	if manager == nil || maxWorkingBytes <= 0 {
		return nil, errors.New("legal hold export requires a manager and positive working space limit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := validateCaseExportJournal(ctx, manager.journal.coordinator.fence.db); err != nil {
		return nil, err
	}
	destination, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	protected := []string{manager.journal.coordinator.vault.root.Name()}
	settings := manager.app.Config().FileSettings
	if model.SafeDereference(settings.DriverName) == "local" {
		protected = append(protected, model.SafeDereference(settings.Directory))
	}
	if model.SafeDereference(settings.DedicatedExportStore) && model.SafeDereference(settings.ExportDriverName) == "local" {
		protected = append(protected, model.SafeDereference(settings.ExportDirectory))
	}
	for _, name := range protected {
		path, err := filepath.EvalSymlinks(name)
		if err != nil {
			return nil, err
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if containsDirectory(path, destination) || containsDirectory(destination, path) {
			return nil, errors.New("legal hold export directory must be separate from archive and live storage")
		}
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = root.Close()
		}
	}()
	for _, name := range []string{".", "staging", "packages"} {
		if name != "." {
			if err = root.MkdirAll(name, 0o700); err != nil {
				return nil, err
			}
		}
		info, err := root.Stat(name)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("legal hold export directories must have private permissions")
		}
	}
	for _, name := range []string{"staging", "packages", "."} {
		dir, err := root.Open(name)
		if err != nil {
			return nil, err
		}
		if err = errors.Join(dir.Sync(), dir.Close()); err != nil {
			return nil, err
		}
	}
	exporter := &CaseExporter{manager: manager, root: root, limit: maxWorkingBytes}
	lease, err := (&LocalVault{root: root}).mutationLease(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	if err = exporter.bindStore(ctx); err != nil {
		return nil, err
	}
	ok = true
	return exporter, nil
}

func (e *CaseExporter) Close() error { return e.root.Close() }

// Export seals all required attachments and snapshots the append-only SQL
// evidence behind the same physical and SQL barriers used by native writers.
// It releases those barriers before building and verifying the ZIP. Later
// writes belong to a later export and cannot change this snapshot.
func (e *CaseExporter) Export(rctx request.CTX, caseID string) (result CaseExportResult, err error) {
	return e.ExportWithKey(rctx, caseID, model.NewId())
}

func (e *CaseExporter) audit(ctx context.Context, query string, args ...any) error {
	return e.auditTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, query, args...)
		return err
	})
}

func (e *CaseExporter) auditTx(ctx context.Context, apply func(*sql.Tx) error) (err error) {
	defer func() {
		// A cancelled transaction may report ErrTxDone after database/sql's
		// automatic rollback. Keep the context cause without masking SQL errors
		// or turning an acknowledged commit into a reported failure.
		if err != nil && ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
	}()
	tx, err := e.manager.journal.coordinator.fence.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on; SET LOCAL statement_timeout='5s'; SET LOCAL lock_timeout='5s'`); err != nil {
		return err
	}
	if err = apply(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Open rechecks the current license/administrator and the entire artifact
// against its durable receipt. Caller owns Close; never serve files by path.
func (e *CaseExporter) Open(rctx request.CTX, exportID string) (*os.File, CaseExportResult, error) {
	var result CaseExportResult
	if _, err := e.manager.authorize(rctx); err != nil {
		return nil, result, err
	}
	if !model.IsValidId(exportID) {
		return nil, result, errors.New("invalid legal hold export ID")
	}
	ctx, cancel := context.WithTimeout(rctx.Context(), 10*time.Minute)
	defer cancel()
	result.ID = exportID
	result, err := e.completed(ctx, exportID)
	if err != nil {
		return nil, result, err
	}
	file, err := e.root.Open("packages/" + exportID + ".zip")
	if err != nil {
		return nil, result, err
	}
	ref, err := hashExportFile(ctx, file)
	if err == nil && ref != result.BlobRef {
		err = ErrIntegrity
	}
	if err == nil {
		var manifest CaseExportManifest
		manifest, err = VerifyCaseExport(ctx, file, ref.Size)
		if err == nil && (manifest.ExportID != exportID || manifest.CaseID != result.CaseID) {
			err = ErrIntegrity
		}
	}
	if err == nil {
		_, err = e.manager.authorize(rctx)
	}
	if err != nil {
		return nil, result, errors.Join(err, file.Close())
	}
	return file, result, nil
}

type exportBudget struct {
	ctx       context.Context
	remaining int64
}

type exportWriter struct {
	budget *exportBudget
	dst    io.Writer
}

func (w exportWriter) Write(p []byte) (int, error) {
	if err := w.budget.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.budget.remaining {
		return 0, ErrExportLimit
	}
	n, err := w.dst.Write(p)
	w.budget.remaining -= int64(n)
	return n, err
}

func (e *CaseExporter) snapshot(ctx context.Context, work string, result CaseExportResult, actor string, budget *exportBudget) (CaseExportManifest, error) {
	return e.snapshotMode(ctx, work, result, actor, budget, false)
}

func (e *CaseExporter) snapshotMode(ctx context.Context, work string, result CaseExportResult, actor string, budget *exportBudget, durable bool) (CaseExportManifest, error) {
	c := e.manager.journal.coordinator
	manifest := CaseExportManifest{Version: 1, ExportID: result.ID, CaseID: result.CaseID, ActorID: actor, ArchiveID: c.archive}
	lease, err := c.vault.mutationLease(ctx)
	if err != nil {
		return manifest, err
	}
	defer lease.Close()
	if err = c.recoverLocked(ctx); err != nil {
		return manifest, err
	}
	tx, err := c.fence.begin(ctx)
	if err != nil {
		return manifest, err
	}
	defer tx.Rollback()
	manifest, err = e.snapshotTx(ctx, tx, work, result, actor, budget, durable, nil)
	if err != nil {
		return manifest, err
	}
	return manifest, tx.Commit()
}

func (e *CaseExporter) snapshotTx(ctx context.Context, tx *sql.Tx, work string, result CaseExportResult, actor string, budget *exportBudget, durable bool, release *CaseReleaseInfo) (CaseExportManifest, error) {
	manifest := CaseExportManifest{Version: 1, ExportID: result.ID, CaseID: result.CaseID, ActorID: actor, ArchiveID: e.manager.journal.coordinator.archive, Release: release}
	if released, err := caseReleased(ctx, tx, result.CaseID); err != nil {
		return manifest, err
	} else if released {
		return manifest, ErrCaseReleased
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM workspace_legal_hold.case_events WHERE case_id=$1 ORDER BY id DESC LIMIT 1`, result.CaseID).Scan(&state); err != nil {
		return manifest, err
	}
	if state != "active" {
		return manifest, ErrCaseNotActive
	}
	if err := e.manager.sealCaseFilesTx(ctx, tx, result.CaseID); err != nil {
		return manifest, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&manifest.CapturedAt); err != nil {
		return manifest, err
	}
	if release != nil {
		release.ReleasedAt = manifest.CapturedAt
	}
	datasets, err := scopeExportDatasets(ctx, tx)
	if err != nil {
		return manifest, err
	}
	for _, dataset := range datasets {
		entry, err := e.spool(ctx, tx, work, dataset, result.CaseID, budget)
		if err != nil {
			return manifest, err
		}
		manifest.Datasets = append(manifest.Datasets, entry)
	}
	if durable {
		if err := e.persistSnapshot(ctx, tx, work, manifest, budget); err != nil {
			return manifest, err
		}
	}
	return manifest, nil
}

type exportDatasetQuery struct{ name, query string }

// Observation selection includes only the case's historical file paths and
// the endpoints of its nonrecursive mutations. A directory deletion involving
// files in several cases must not export another case's attachment bytes.
const exportFileCTE = `WITH files AS (SELECT entity_id AS file_id FROM workspace_legal_hold.case_members WHERE case_id=$1 AND kind='fileinfo'),
 versions AS (SELECT v.* FROM workspace_legal_hold.file_versions v JOIN files f USING(file_id)),
 links AS (SELECT m.* FROM workspace_legal_hold.mutation_files m JOIN files f USING(file_id)),
 intents AS (SELECT i.* FROM workspace_legal_hold.mutation_intents i WHERE EXISTS(SELECT 1 FROM links l WHERE l.intent_id=i.id)),
 paths AS (SELECT DISTINCT p.path FROM versions v CROSS JOIN LATERAL
  (VALUES(v.metadata->>'path'),(v.metadata->>'previewpath'),(v.metadata->>'thumbnailpath')) p(path) WHERE COALESCE(p.path,'')<>''),
 observations AS (SELECT o.* FROM workspace_legal_hold.mutation_observations o JOIN intents i ON i.id=o.intent_id
  WHERE EXISTS(SELECT 1 FROM paths p WHERE p.path=o.path)
  OR (NOT i.recursive AND EXISTS(SELECT 1 FROM jsonb_array_elements_text(i.paths) p(path) WHERE p.path=o.path))),
 receipts AS (SELECT r.* FROM workspace_legal_hold.file_receipts r JOIN versions v ON v.id=r.version_id)
`

var exportDatasets = []exportDatasetQuery{
	{"case.jsonl", `SELECT to_jsonb(r) FROM workspace_legal_hold.cases r WHERE id=$1`},
	{"scopes.jsonl", `SELECT to_jsonb(r) FROM workspace_legal_hold.case_scopes r WHERE case_id=$1 ORDER BY kind,target_id`},
	{"events.jsonl", `SELECT to_jsonb(r) FROM workspace_legal_hold.case_events r WHERE case_id=$1 ORDER BY id`},
	{"members.jsonl", `SELECT to_jsonb(r) FROM workspace_legal_hold.case_members r WHERE case_id=$1 ORDER BY kind,entity_id`},
	{"versions.jsonl", `SELECT to_jsonb(r) FROM workspace_legal_hold.case_versions r WHERE case_id=$1 ORDER BY id`},
	{"file_versions.jsonl", exportFileCTE + `SELECT to_jsonb(v) FROM versions v ORDER BY id`},
	{"file_receipts.jsonl", exportFileCTE + `SELECT to_jsonb(r) FROM receipts r ORDER BY version_id`},
	{"mutation_files.jsonl", exportFileCTE + `SELECT to_jsonb(l) FROM links l ORDER BY intent_id,file_id`},
	{"mutation_intents.jsonl", exportFileCTE + `SELECT to_jsonb(i)-'observed_paths' FROM intents i ORDER BY id`},
	{"mutation_observations.jsonl", exportFileCTE + `SELECT to_jsonb(o) FROM observations o ORDER BY intent_id,phase,path`},
	{"mutation_completions.jsonl", exportFileCTE + `SELECT to_jsonb(c) FROM workspace_legal_hold.mutation_completions c JOIN intents i ON i.id=c.intent_id ORDER BY intent_id`},
	{"objects.jsonl", exportFileCTE + `SELECT jsonb_build_object('sha256',sha256,'size',size) FROM (
  SELECT obj->>'sha256' AS sha256,(obj->>'size')::bigint AS size FROM receipts r CROSS JOIN LATERAL jsonb_array_elements(r.manifest) obj
  UNION SELECT sha256,size FROM observations WHERE present) refs ORDER BY sha256,size`},
}

func (e *CaseExporter) spool(ctx context.Context, tx *sql.Tx, work string, dataset exportDatasetQuery, caseID string, budget *exportBudget) (CaseExportDataset, error) {
	entry := CaseExportDataset{Path: dataset.name}
	file, err := e.root.OpenFile(work+"/"+dataset.name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return entry, err
	}
	defer file.Close()
	hash := sha256.New()
	writer := exportWriter{budget, io.MultiWriter(file, hash)}
	rows, err := tx.QueryContext(ctx, dataset.query, caseID)
	if err != nil {
		return entry, err
	}
	defer rows.Close()
	for rows.Next() {
		var row []byte
		if err = rows.Scan(&row); err != nil {
			return entry, err
		}
		n, err := writer.Write(append(row, '\n'))
		if err != nil {
			return entry, err
		}
		entry.Rows++
		entry.Size += int64(n)
	}
	entry.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return entry, errors.Join(rows.Err(), rows.Close(), file.Close())
}

func (e *CaseExporter) build(ctx context.Context, work string, manifest CaseExportManifest, budget *exportBudget) (BlobRef, string, error) {
	file, err := e.root.OpenFile(work+"/package.zip", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return BlobRef{}, "", err
	}
	defer file.Close()
	zw := zip.NewWriter(exportWriter{budget, file})
	add := func(name string, src io.Reader) error {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		header.SetMode(0o600)
		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		_, err = io.CopyBuffer(writer, vaultContextReader{ctx, src}, make([]byte, 64*1024))
		return err
	}
	for _, dataset := range manifest.Datasets {
		src, err := e.root.Open(work + "/" + dataset.Path)
		if err != nil {
			return BlobRef{}, "", err
		}
		if err = errors.Join(add(dataset.Path, src), src.Close()); err != nil {
			return BlobRef{}, "", err
		}
	}
	index, err := e.root.Open(work + "/objects.jsonl")
	if err != nil {
		return BlobRef{}, "", err
	}
	defer index.Close()
	decoder := json.NewDecoder(vaultContextReader{ctx, index})
	for {
		var ref BlobRef
		if err = decoder.Decode(&ref); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return BlobRef{}, "", err
		}
		// Verify while writing and again in the completed ZIP. A concurrent
		// filesystem mutation cannot pass merely because an earlier read did.
		name, err := blobPath(ref)
		if err != nil {
			return BlobRef{}, "", err
		}
		src, err := e.manager.journal.coordinator.vault.root.Open(name)
		if err != nil {
			return BlobRef{}, "", err
		}
		hash := sha256.New()
		reader := io.TeeReader(io.LimitReader(src, ref.Size+1), hash)
		err = errors.Join(add(name, reader), src.Close())
		if err == nil && hex.EncodeToString(hash.Sum(nil)) != ref.SHA256 {
			err = ErrIntegrity
		}
		if err != nil {
			return BlobRef{}, "", err
		}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return BlobRef{}, "", err
	}
	if err = add("manifest.json", bytes.NewReader(encoded)); err != nil {
		return BlobRef{}, "", err
	}
	if err = errors.Join(zw.Close(), file.Chmod(0o400), file.Sync()); err != nil {
		return BlobRef{}, "", err
	}
	ref, err := hashExportFile(ctx, file)
	if err != nil {
		return BlobRef{}, "", err
	}
	if _, err = VerifyCaseExport(ctx, file, ref.Size); err != nil {
		return BlobRef{}, "", fmt.Errorf("verify completed legal hold export: %w", err)
	}
	manifestHash := sha256.Sum256(encoded)
	return ref, hex.EncodeToString(manifestHash[:]), nil
}

func hashExportFile(ctx context.Context, file *os.File) (BlobRef, error) {
	info, err := file.Stat()
	if err != nil {
		return BlobRef{}, err
	}
	if !info.Mode().IsRegular() {
		return BlobRef{}, ErrIntegrity
	}
	hash := sha256.New()
	n, err := io.CopyBuffer(hash, vaultContextReader{ctx, io.NewSectionReader(file, 0, info.Size())}, make([]byte, 64*1024))
	if err == nil && n != info.Size() {
		err = ErrIntegrity
	}
	return BlobRef{hex.EncodeToString(hash.Sum(nil)), n}, err
}
