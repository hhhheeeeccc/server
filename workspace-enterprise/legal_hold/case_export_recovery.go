package legal_hold

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

var ErrExportKeyConflict = errors.New("legal hold export request key belongs to a different case")

// Request commits an idempotent request without starting work. Keys are scoped
// to the authenticated actor. A lost response is recovered using the same key;
// a different case with that key is rejected, never silently substituted.
func (e *CaseExporter) Request(rctx request.CTX, caseID, key string) (CaseExportResult, error) {
	var result CaseExportResult
	actor, err := e.manager.authorize(rctx)
	if err != nil {
		return result, err
	}
	if !model.IsValidId(caseID) || !model.IsValidId(key) {
		return result, errors.New("legal hold case ID and request key must be valid IDs")
	}
	ctx, cancel := context.WithTimeout(rctx.Context(), 10*time.Second)
	defer cancel()
	if released, releaseErr := caseReleased(ctx, e.manager.journal.coordinator.fence.db, caseID); releaseErr != nil {
		return result, releaseErr
	} else if released {
		err = e.manager.journal.coordinator.fence.db.QueryRowContext(ctx, `SELECT r.id,r.case_id FROM workspace_legal_hold.case_export_keys k
		 JOIN workspace_legal_hold.case_exports r ON r.id=k.export_id WHERE k.actor_id=$1 AND k.request_key=$2`, actor, key).Scan(&result.ID, &result.CaseID)
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrCaseReleased
		} else if err == nil && result.CaseID != caseID {
			err = ErrExportKeyConflict
		}
		return result, err
	}
	result.ID, result.CaseID = model.NewId(), caseID
	existing := errors.New("existing legal hold export request key")
	err = e.auditTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT workspace_legal_hold.case_barrier()`); err != nil {
			return err
		}
		if released, err := caseReleased(ctx, tx, caseID); err != nil {
			return err
		} else if released {
			return ErrCaseReleased
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_exports(id,case_id,actor_id) VALUES($1,$2,$3)`, result.ID, caseID, actor); err != nil {
			return err
		}
		insert, err := tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_export_keys(actor_id,request_key,export_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, actor, key, result.ID)
		if err != nil {
			return err
		}
		count, err := insert.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return existing
		}
		return nil
	})
	if errors.Is(err, existing) {
		err = e.manager.journal.coordinator.fence.db.QueryRowContext(ctx, `SELECT r.id,r.case_id FROM workspace_legal_hold.case_export_keys k
		 JOIN workspace_legal_hold.case_exports r ON r.id=k.export_id WHERE k.actor_id=$1 AND k.request_key=$2`, actor, key).Scan(&result.ID, &result.CaseID)
		if err == nil && result.CaseID != caseID {
			err = ErrExportKeyConflict
		}
	}
	return result, err
}

func (e *CaseExporter) ExportWithKey(rctx request.CTX, caseID, key string) (CaseExportResult, error) {
	result, err := e.Request(rctx, caseID, key)
	if err != nil {
		return result, err
	}
	completed, err := e.Resume(rctx, result.ID)
	if completed.ID == "" {
		completed = result
	}
	if err != nil {
		// Cancellation can fall between the committed request and the
		// attempt insert. Retain a failed request outcome in that interval
		// too, without replacing any committed completion or earlier failure.
		cleanup, stop := context.WithTimeout(context.WithoutCancel(rctx.Context()), 5*time.Second)
		defer stop()
		err = errors.Join(err, e.audit(cleanup, `INSERT INTO workspace_legal_hold.case_export_outcomes(export_id,state) VALUES($1,'failed') ON CONFLICT DO NOTHING`, result.ID))
	}
	return completed, err
}

// Resume retries the same request. A committed snapshot is restored exactly;
// source changes after its cutoff cannot enter the retry. Requests that died
// before committing any snapshot establish their cutoff on the next attempt.
// Recovery is explicit; no production scheduler is registered here.
func (e *CaseExporter) Resume(rctx request.CTX, exportID string) (CaseExportResult, error) {
	actor, err := e.manager.authorize(rctx)
	if err != nil {
		return CaseExportResult{}, err
	}
	if !model.IsValidId(exportID) {
		return CaseExportResult{}, errors.New("invalid legal hold export ID")
	}
	ctx, cancel := context.WithTimeout(rctx.Context(), 10*time.Minute)
	defer cancel()
	return e.run(ctx, exportID, actor, func() error {
		_, err := e.manager.authorize(rctx)
		return err
	})
}

func (e *CaseExporter) run(ctx context.Context, exportID, actor string, authorize func() error) (result CaseExportResult, err error) {
	result.ID = exportID
	var originalActor string
	err = e.manager.journal.coordinator.fence.db.QueryRowContext(ctx, `SELECT case_id,actor_id FROM workspace_legal_hold.case_exports WHERE id=$1`, exportID).Scan(&result.CaseID, &originalActor)
	if err != nil {
		return result, err
	}
	// One working attempt per shared export store, across cooperating
	// processes. Keep the physical lease until all actual I/O returns.
	lease, err := (&LocalVault{root: e.root}).mutationLease(ctx)
	if err != nil {
		return result, err
	}
	defer lease.Close()
	if err = e.checkStore(ctx); err != nil {
		return result, err
	}
	if complete, completeErr := e.completed(ctx, exportID); completeErr == nil {
		_, err = e.verifyPublished(ctx, complete, nil)
		if err == nil {
			err = authorize()
		}
		if err == nil {
			err = e.root.RemoveAll("staging/" + exportID)
		}
		return complete, err
	} else if !errors.Is(completeErr, sql.ErrNoRows) {
		return result, completeErr
	}
	attempt := model.NewId()
	if err = e.audit(ctx, `INSERT INTO workspace_legal_hold.case_export_attempts(id,export_id,actor_id) VALUES($1,$2,$3)`, attempt, exportID, actor); err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer stop()
			err = errors.Join(err, e.recordOutcome(cleanup, result, attempt, false))
		}
	}()
	manifest, snapshotErr := e.readSnapshot(ctx, result, originalActor)
	if snapshotErr != nil && !errors.Is(snapshotErr, sql.ErrNoRows) {
		return result, snapshotErr
	}
	if _, statErr := e.root.Lstat("packages/" + exportID + ".zip"); statErr == nil {
		if snapshotErr != nil {
			return result, errors.New("legal hold published package has no durable snapshot; administrative review required")
		}
		result, err = e.verifyPublished(ctx, result, &manifest)
		if err != nil {
			return result, err
		}
		if err = authorize(); err != nil {
			return result, err
		}
		if err = e.syncPackages(); err != nil {
			return result, err
		}
		if err = e.recordOutcome(ctx, result, attempt, true); err != nil {
			return result, err
		}
		return result, e.root.RemoveAll("staging/" + exportID)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return result, statErr
	}
	work := "staging/" + exportID
	if err = e.root.RemoveAll(work); err != nil {
		return result, err
	}
	if err = e.root.Mkdir(work, 0o700); err != nil {
		return result, err
	}
	defer e.root.RemoveAll(work)
	budget := &exportBudget{ctx, e.limit}
	if errors.Is(snapshotErr, sql.ErrNoRows) {
		manifest, err = e.snapshotMode(ctx, work, result, originalActor, budget, true)
	} else {
		err = e.restoreSnapshot(ctx, work, manifest, budget)
	}
	if err != nil {
		return result, err
	}
	result.BlobRef, result.ManifestSHA256, err = e.build(ctx, work, manifest, budget)
	if err != nil {
		return result, err
	}
	if err = authorize(); err != nil {
		return result, err
	}
	if err = e.publish(ctx, work, result.ID); err != nil {
		return result, err
	}
	return result, e.recordOutcome(ctx, result, attempt, true)
}

func (e *CaseExporter) bindStore(ctx context.Context) error {
	id, err := (&LocalVault{root: e.root}).archiveIdentity()
	if err != nil {
		return err
	}
	if err = e.audit(ctx, `INSERT INTO workspace_legal_hold.case_export_store_binding VALUES(true,$1) ON CONFLICT DO NOTHING`, id); err != nil {
		return err
	}
	e.storeID = id
	return e.checkStore(ctx)
}

func (e *CaseExporter) checkStore(ctx context.Context) error {
	var bound string
	err := e.manager.journal.coordinator.fence.db.QueryRowContext(ctx, `SELECT store_id FROM workspace_legal_hold.case_export_store_binding WHERE singleton`).Scan(&bound)
	if err == nil && (e.storeID == "" || bound != e.storeID) {
		err = errors.New("legal hold export store differs from the durable binding")
	}
	return err
}

func (e *CaseExporter) completed(ctx context.Context, id string) (CaseExportResult, error) {
	result := CaseExportResult{ID: id}
	if err := e.checkStore(ctx); err != nil {
		return result, err
	}
	err := e.manager.journal.coordinator.fence.db.QueryRowContext(ctx, `WITH outcomes AS (
	 SELECT export_id,sha256,size,manifest_sha256,0 AS priority FROM workspace_legal_hold.case_export_attempt_outcomes WHERE state='complete'
	 UNION ALL SELECT export_id,sha256,size,manifest_sha256,1 FROM workspace_legal_hold.case_export_outcomes WHERE state='complete')
	 SELECT r.case_id,o.sha256,o.size,o.manifest_sha256 FROM workspace_legal_hold.case_exports r JOIN outcomes o ON o.export_id=r.id
	 WHERE r.id=$1 ORDER BY o.priority LIMIT 1`, id).Scan(&result.CaseID, &result.SHA256, &result.Size, &result.ManifestSHA256)
	return result, err
}

func (e *CaseExporter) recordOutcome(ctx context.Context, result CaseExportResult, attempt string, success bool) error {
	return e.auditTx(ctx, func(tx *sql.Tx) error {
		state := "failed"
		var digest, manifest any
		var size any
		if success {
			state, digest, manifest, size = "complete", result.SHA256, result.ManifestSHA256, result.Size
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_export_outcomes(export_id,state,sha256,size,manifest_sha256) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, result.ID, state, digest, size, manifest); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_export_attempt_outcomes(attempt_id,export_id,state,sha256,size,manifest_sha256) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(attempt_id) DO NOTHING`, attempt, result.ID, state, digest, size, manifest)
		return err
	})
}

func (e *CaseExporter) persistSnapshot(ctx context.Context, tx *sql.Tx, work string, manifest CaseExportManifest, budget *exportBudget) error {
	for _, dataset := range manifest.Datasets {
		if dataset.Size > budget.remaining {
			return ErrExportLimit
		}
		budget.remaining -= dataset.Size
		file, err := e.root.Open(work + "/" + dataset.Path)
		if err != nil {
			return err
		}
		ref, putErr := e.manager.journal.coordinator.vault.Put(ctx, file, dataset.Size)
		if err = errors.Join(putErr, file.Close()); err != nil {
			return err
		}
		if ref != dataset.BlobRef {
			return ErrIntegrity
		}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_export_snapshots(export_id,manifest) VALUES($1,$2::jsonb)`, manifest.ExportID, string(encoded))
	return err
}

func (e *CaseExporter) readSnapshot(ctx context.Context, result CaseExportResult, actor string) (CaseExportManifest, error) {
	var manifest CaseExportManifest
	var encoded []byte
	err := e.manager.journal.coordinator.fence.db.QueryRowContext(ctx, `SELECT manifest FROM workspace_legal_hold.case_export_snapshots WHERE export_id=$1`, result.ID).Scan(&encoded)
	if err != nil {
		return manifest, err
	}
	if err = json.Unmarshal(encoded, &manifest); err != nil {
		return manifest, err
	}
	if !validExportIDs(manifest) || manifest.Version != 1 || manifest.ExportID != result.ID || manifest.CaseID != result.CaseID || manifest.ActorID != actor || manifest.ArchiveID != e.manager.journal.coordinator.archive || len(manifest.Datasets) != len(exportDatasets) {
		return manifest, ErrIntegrity
	}
	for index, dataset := range manifest.Datasets {
		if dataset.Path != exportDatasets[index].name || dataset.Rows < 0 {
			return manifest, ErrIntegrity
		}
		if _, err = blobPath(dataset.BlobRef); err != nil {
			return manifest, err
		}
	}
	return manifest, nil
}

func (e *CaseExporter) restoreSnapshot(ctx context.Context, work string, manifest CaseExportManifest, budget *exportBudget) error {
	for _, dataset := range manifest.Datasets {
		name, err := blobPath(dataset.BlobRef)
		if err != nil {
			return err
		}
		src, err := e.manager.journal.coordinator.vault.root.Open(name)
		if err != nil {
			return err
		}
		dst, err := e.root.OpenFile(work+"/"+dataset.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return errors.Join(err, src.Close())
		}
		hash := sha256.New()
		n, copyErr := io.CopyBuffer(exportWriter{budget, io.MultiWriter(dst, hash)}, vaultContextReader{ctx, io.LimitReader(src, dataset.Size+1)}, make([]byte, 64*1024))
		if err = errors.Join(copyErr, src.Close(), dst.Close()); err != nil {
			return err
		}
		if n != dataset.Size || hex.EncodeToString(hash.Sum(nil)) != dataset.SHA256 {
			return ErrIntegrity
		}
	}
	return nil
}

func (e *CaseExporter) publish(ctx context.Context, work, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.root.Link(work+"/package.zip", "packages/"+id+".zip"); err != nil {
		return err
	}
	return e.syncPackages()
}

func (e *CaseExporter) syncPackages() error {
	dir, err := e.root.Open("packages")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (e *CaseExporter) verifyPublished(ctx context.Context, expected CaseExportResult, snapshot *CaseExportManifest) (CaseExportResult, error) {
	file, err := e.root.Open("packages/" + expected.ID + ".zip")
	if err != nil {
		return expected, err
	}
	defer file.Close()
	ref, err := hashExportFile(ctx, file)
	if err != nil {
		return expected, err
	}
	if expected.SHA256 != "" && expected.BlobRef != ref {
		return expected, ErrIntegrity
	}
	manifest, err := VerifyCaseExport(ctx, file, ref.Size)
	if err != nil {
		return expected, err
	}
	if manifest.ExportID != expected.ID || manifest.CaseID != expected.CaseID || manifest.ArchiveID != e.manager.journal.coordinator.archive {
		return expected, ErrIntegrity
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return expected, err
	}
	if snapshot != nil {
		frozen, err := json.Marshal(snapshot)
		if err != nil || !bytes.Equal(frozen, encoded) {
			return expected, ErrIntegrity
		}
	}
	manifestHash := sha256.Sum256(encoded)
	checksum := hex.EncodeToString(manifestHash[:])
	if expected.ManifestSHA256 != "" && expected.ManifestSHA256 != checksum {
		return expected, ErrIntegrity
	}
	expected.BlobRef, expected.ManifestSHA256 = ref, checksum
	return expected, nil
}
