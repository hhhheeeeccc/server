package legal_hold

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/case_export_v1.sql
var legacyExportSchema []byte

func TestCaseExportRecoveryRequestKeysSerializeAndRejectDifferentCase(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	exporter, _ := exportFixture(t, m, 16<<20)
	for _, caller := range []request.CTX{nil, th.Context.WithSession(&model.Session{UserId: th.BasicUser.Id, Roles: "system_admin"})} {
		_, err := exporter.Resume(caller, model.NewId())
		require.ErrorIs(t, err, ErrCaseForbidden)
	}
	th.Server.SetLicense(nil)
	_, err := exporter.Resume(rctx, model.NewId())
	require.ErrorIs(t, err, ErrCaseForbidden)
	th.Server.SetLicense(model.NewTestLicense("compliance"))
	key := model.NewId()
	var group sync.WaitGroup
	results, failures := make([]CaseExportResult, 4), make([]error, 4)
	for i := range results {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			results[index], failures[index] = exporter.Request(rctx, status.ID, key)
		}(i)
	}
	group.Wait()
	for index, err := range failures {
		require.NoError(t, err)
		require.Equal(t, results[0], results[index])
	}
	other := startChannelCase(t, m, rctx, th.BasicChannel2.Id)
	_, err = exporter.Request(rctx, other.ID, key)
	require.ErrorIs(t, err, ErrExportKeyConflict)
	var requests, keys int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_exports`).Scan(&requests))
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_export_keys`).Scan(&keys))
	require.Equal(t, 1, requests)
	require.Equal(t, 1, keys)
	result, err := exporter.ExportWithKey(rctx, status.ID, key)
	require.NoError(t, err)
	repeated, err := exporter.ExportWithKey(rctx, status.ID, key)
	require.NoError(t, err)
	require.Equal(t, result, repeated)
	var attempts int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_export_attempts`).Scan(&attempts))
	require.Equal(t, 1, attempts, "replaying a confirmed request must not create another work attempt")
}

func TestCaseExportRecoveryRetriesFailedBuildWithSameDurableSnapshot(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	uploadFixtureFile(t, th, bytes.Repeat([]byte("fixed-cutoff attachment\x00"), 60000))
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	exporter, directory := exportFixture(t, m, 128<<10)
	key := model.NewId()
	failed, err := exporter.ExportWithKey(rctx, status.ID, key)
	require.ErrorIs(t, err, ErrExportLimit)
	var frozen []byte
	require.NoError(t, c.fence.db.QueryRow(`SELECT manifest FROM workspace_legal_hold.case_export_snapshots WHERE export_id=$1`, failed.ID).Scan(&frozen))
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "after the committed export cutoff"})
	require.NoError(t, err)
	other, err := NewCaseExporter(m, directory, 16<<20)
	require.NoError(t, err)
	defer other.Close()
	result, err := other.ExportWithKey(rctx, status.ID, key)
	require.NoError(t, err)
	require.Equal(t, failed.ID, result.ID)
	file, _, err := other.Open(rctx, result.ID)
	require.NoError(t, err)
	entries := readExportEntries(t, file)
	require.NoError(t, file.Close())
	require.NotContains(t, string(entries["versions.jsonl"]), post.Id)
	var manifest CaseExportManifest
	require.NoError(t, json.Unmarshal(frozen, &manifest))
	var actual CaseExportManifest
	require.NoError(t, json.Unmarshal(entries["manifest.json"], &actual))
	require.Equal(t, manifest, actual)
	var legacy string
	require.NoError(t, c.fence.db.QueryRow(`SELECT state FROM workspace_legal_hold.case_export_outcomes WHERE export_id=$1`, result.ID).Scan(&legacy))
	require.Equal(t, "failed", legacy, "the first failure remains immutable history")
	var states string
	require.NoError(t, c.fence.db.QueryRow(`SELECT string_agg(state,',' ORDER BY state) FROM workspace_legal_hold.case_export_attempt_outcomes WHERE export_id=$1`, result.ID).Scan(&states))
	require.Equal(t, "complete,failed", states)
}

func TestCaseExportRecoveryConcurrentResumesPublishOnlyOneConfirmedArtifact(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	uploadFixtureFile(t, th, bytes.Repeat([]byte("concurrent retry\x00"), 10000))
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	exporter, directory := exportFixture(t, m, 16<<20)
	request, err := exporter.Request(rctx, status.ID, model.NewId())
	require.NoError(t, err)
	other, err := NewCaseExporter(m, directory, 16<<20)
	require.NoError(t, err)
	defer other.Close()
	var group sync.WaitGroup
	results, failures := make([]CaseExportResult, 2), make([]error, 2)
	for index, instance := range []*CaseExporter{exporter, other} {
		group.Add(1)
		go func(index int, instance *CaseExporter) {
			defer group.Done()
			results[index], failures[index] = instance.Resume(rctx, request.ID)
		}(index, instance)
	}
	group.Wait()
	for _, err := range failures {
		require.NoError(t, err)
	}
	require.Equal(t, results[0], results[1])
	var attempts, complete int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_export_attempts WHERE export_id=$1`, request.ID).Scan(&attempts))
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_export_attempt_outcomes WHERE export_id=$1 AND state='complete'`, request.ID).Scan(&complete))
	require.Equal(t, 1, attempts)
	require.Equal(t, 1, complete)
	files, err := os.ReadDir(filepath.Join(directory, "packages"))
	require.NoError(t, err)
	require.Len(t, files, 1)
}

func TestCaseExportRecoveryRejectsCorruptFrozenMetadataWithoutTakingNewSnapshot(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	uploadFixtureFile(t, th, bytes.Repeat([]byte("large enough to require resume\x00"), 20000))
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	exporter, _ := exportFixture(t, m, 128<<10)
	result, err := exporter.Export(rctx, status.ID)
	require.ErrorIs(t, err, ErrExportLimit)
	manifest, err := exporter.readSnapshot(context.Background(), result, th.SystemAdminUser.Id)
	require.NoError(t, err)
	var versions CaseExportDataset
	for _, dataset := range manifest.Datasets {
		if dataset.Path == "versions.jsonl" {
			versions = dataset
		}
	}
	require.NotEmpty(t, versions.SHA256)
	name, err := blobPath(versions.BlobRef)
	require.NoError(t, err)
	require.NoError(t, c.vault.root.Chmod(name, 0o600))
	file, err := c.vault.root.OpenFile(name, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteAt([]byte("X"), 0)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	exporter.limit = 16 << 20
	_, err = exporter.Resume(rctx, result.ID)
	require.ErrorIs(t, err, ErrIntegrity)
	var snapshots, successes int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_export_snapshots WHERE export_id=$1`, result.ID).Scan(&snapshots))
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_export_attempt_outcomes WHERE export_id=$1 AND state='complete'`, result.ID).Scan(&successes))
	require.Equal(t, 1, snapshots)
	require.Zero(t, successes)
}

func TestCaseExportRecoveryUpgradesLegacyJournalAndPreservesCompletedReceipt(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	_, err := c.fence.db.Exec(string(legacyExportSchema))
	require.NoError(t, err)
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	root, err := os.OpenRoot(directory)
	require.NoError(t, err)
	defer root.Close()
	legacy := &CaseExporter{manager: m, root: root, limit: 16 << 20}
	for _, name := range []string{"staging", "packages", "staging/legacy"} {
		require.NoError(t, root.MkdirAll(name, 0o700))
	}
	result := CaseExportResult{ID: model.NewId(), CaseID: status.ID}
	require.NoError(t, legacy.audit(context.Background(), `INSERT INTO workspace_legal_hold.case_exports(id,case_id,actor_id) VALUES($1,$2,$3)`, result.ID, status.ID, th.SystemAdminUser.Id))
	budget := &exportBudget{context.Background(), 16 << 20}
	manifest, err := legacy.snapshot(context.Background(), "staging/legacy", result, th.SystemAdminUser.Id, budget)
	require.NoError(t, err)
	result.BlobRef, result.ManifestSHA256, err = legacy.build(context.Background(), "staging/legacy", manifest, budget)
	require.NoError(t, err)
	require.NoError(t, legacy.publish(context.Background(), "staging/legacy", result.ID))
	require.NoError(t, legacy.audit(context.Background(), `INSERT INTO workspace_legal_hold.case_export_outcomes(export_id,state,sha256,size,manifest_sha256) VALUES($1,'complete',$2,$3,$4)`, result.ID, result.SHA256, result.Size, result.ManifestSHA256))
	orphan := CaseExportResult{ID: model.NewId(), CaseID: status.ID}
	require.NoError(t, legacy.audit(context.Background(), `INSERT INTO workspace_legal_hold.case_exports(id,case_id,actor_id) VALUES($1,$2,$3)`, orphan.ID, status.ID, th.SystemAdminUser.Id))
	require.NoError(t, root.Mkdir("staging/orphan", 0o700))
	budget = &exportBudget{context.Background(), 16 << 20}
	orphanManifest, err := legacy.snapshot(context.Background(), "staging/orphan", orphan, th.SystemAdminUser.Id, budget)
	require.NoError(t, err)
	orphan.BlobRef, orphan.ManifestSHA256, err = legacy.build(context.Background(), "staging/orphan", orphanManifest, budget)
	require.NoError(t, err)
	require.NoError(t, legacy.publish(context.Background(), "staging/orphan", orphan.ID))
	require.NoError(t, legacy.audit(context.Background(), `INSERT INTO workspace_legal_hold.case_export_outcomes(export_id,state) VALUES($1,'failed')`, orphan.ID))
	var before, after []byte
	require.NoError(t, c.fence.db.QueryRow(`SELECT to_jsonb(r) FROM workspace_legal_hold.case_export_outcomes r WHERE export_id=$1`, result.ID).Scan(&before))
	require.NoError(t, InstallCaseExportJournal(context.Background(), m.journal))
	exporter, err := NewCaseExporter(m, directory, 16<<20)
	require.NoError(t, err)
	defer exporter.Close()
	resumed, err := exporter.Resume(rctx, result.ID)
	require.NoError(t, err)
	require.Equal(t, result, resumed)
	require.NoError(t, c.fence.db.QueryRow(`SELECT to_jsonb(r) FROM workspace_legal_hold.case_export_outcomes r WHERE export_id=$1`, result.ID).Scan(&after))
	require.Equal(t, before, after)
	var attempts int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_export_attempts`).Scan(&attempts))
	require.Zero(t, attempts)
	_, err = exporter.Resume(rctx, orphan.ID)
	require.ErrorContains(t, err, "no durable snapshot")
}

func TestCaseExportRecoveryProtectsBindingKeysSnapshotsAndAttempts(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	exporter, _ := exportFixture(t, m, 16<<20)
	result, err := exporter.Export(rctx, status.ID)
	require.NoError(t, err)
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	_, err = NewCaseExporter(m, directory, 16<<20)
	require.ErrorContains(t, err, "differs from the durable binding")
	for _, table := range []string{"case_export_recovery_installation", "case_export_store_binding", "case_export_keys", "case_export_snapshots", "case_export_attempts", "case_export_attempt_outcomes"} {
		_, err := c.fence.db.Exec("DELETE FROM workspace_legal_hold." + table)
		require.ErrorContains(t, err, "append only")
		_, err = c.fence.db.Exec("TRUNCATE workspace_legal_hold." + table + " CASCADE")
		require.ErrorContains(t, err, "append only")
	}
	_, err = c.fence.db.Exec(`UPDATE workspace_legal_hold.case_export_snapshots SET manifest='{}' WHERE export_id=$1`, result.ID)
	require.ErrorContains(t, err, "append only")
	_, err = c.fence.db.Exec(`ALTER TABLE workspace_legal_hold.case_export_snapshots DISABLE TRIGGER immutable_rows`)
	require.NoError(t, err)
	require.ErrorContains(t, InstallCaseExportJournal(context.Background(), m.journal), "triggers are incomplete")
}
