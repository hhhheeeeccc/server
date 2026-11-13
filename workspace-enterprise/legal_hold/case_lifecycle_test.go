package legal_hold

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func releaseRequest(id string) CaseReleaseRequest {
	return CaseReleaseRequest{CaseID: id, RequestKey: model.NewId(), Reason: "انتهاء سبب الحفظ مع إبقاء الأدلة"}
}

func lifecycleVersionCount(t *testing.T, db *sql.DB, id string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_versions WHERE case_id=$1`, id).Scan(&count))
	return count
}

func TestCaseLifecycleReleaseStopsFutureCaptureAndKeepsFinalEvidenceAfterNativeDeletion(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseLifecycle(context.Background(), m.journal))
	data := bytes.Repeat([]byte("held before release\x00"), 10000)
	post, info := uploadFixtureFile(t, th, data)
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	exporter, _ := exportFixture(t, m, 16<<20)
	input := releaseRequest(status.ID)
	released, err := m.Release(rctx, exporter, input)
	require.NoError(t, err)
	require.Equal(t, "released", released.State)
	count := lifecycleVersionCount(t, c.fence.db, status.ID)
	post.Message = "after release, excluded"
	_, _, err = th.Client.UpdatePost(context.Background(), post.Id, post)
	require.NoError(t, err)
	_, err = th.Store.TemporaryPost().Save(th.Context, &model.TemporaryPost{ID: post.Id, Type: model.PostTypeBurnOnRead, Message: "temporary after release"})
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`UPDATE users SET nickname='identity after release' WHERE id=$1`, post.UserId)
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`UPDATE channels SET header='channel after release' WHERE id=$1`, post.ChannelId)
	require.NoError(t, err)
	require.Nil(t, th.App.PermanentDeletePost(th.Context, post.Id, post.UserId))
	_, err = th.Store.FileInfo().GetFromMaster(info.Id)
	require.Error(t, err, "native source metadata was actually removed")
	present, err := c.source.FileExists(info.Path)
	require.NoError(t, err)
	require.False(t, present)
	require.Equal(t, count, lifecycleVersionCount(t, c.fence.db, status.ID))
	file, _, err := exporter.Open(rctx, released.ExportID)
	require.NoError(t, err)
	entries := readExportEntries(t, file)
	require.NoError(t, file.Close())
	require.NotContains(t, string(entries["versions.jsonl"]), post.Message)
	var manifest CaseExportManifest
	require.NoError(t, json.Unmarshal(entries["manifest.json"], &manifest))
	require.NotNil(t, manifest.Release)
	require.Equal(t, input.Reason, manifest.Release.Reason)
	require.Equal(t, input.RequestKey, manifest.Release.RequestKey)
	found := false
	for _, content := range entries {
		found = found || bytes.Equal(content, data)
	}
	require.True(t, found)
	_, err = m.ResumeCapture(rctx, status.ID)
	require.ErrorIs(t, err, ErrCaseReleased)
	_, err = exporter.Export(rctx, status.ID)
	require.ErrorIs(t, err, ErrCaseReleased)
	repeated, err := m.Release(rctx, exporter, input)
	require.NoError(t, err)
	require.Equal(t, released, repeated)
	input.Reason = "different release assertion"
	_, err = m.Release(rctx, exporter, input)
	require.ErrorIs(t, err, ErrCaseReleased)
	t.Log("case released only after a durable final snapshot; actual native deletion succeeds and the frozen export remains verified without future capture")
}

func TestCaseLifecycleOverlappingCaseAndIndependentReservationsRemainProtected(t *testing.T) {
	for _, otherHold := range []string{"case", "independent"} {
		t.Run(otherHold, func(t *testing.T) {
			th, c, m, rctx, _ := caseFixture(t)
			require.NoError(t, InstallCaseLifecycle(context.Background(), m.journal))
			post, info := uploadFixtureFile(t, th, []byte("still held elsewhere"))
			first := startChannelCase(t, m, rctx, th.BasicChannel.Id)
			var second CaseStatus
			if otherHold == "case" {
				second = startChannelCase(t, m, rctx, th.BasicChannel.Id)
			} else {
				_, err := c.ReserveFile(context.Background(), info.Id)
				require.NoError(t, err)
			}
			exporter, _ := exportFixture(t, m, 16<<20)
			_, err := m.Release(rctx, exporter, releaseRequest(first.ID))
			require.NoError(t, err)
			frozen := lifecycleVersionCount(t, c.fence.db, first.ID)
			_, err = th.Server.FileBackend().WriteFile(bytes.NewReader([]byte("a later physical generation")), info.Path)
			require.NoError(t, err)
			requireMutationBytes(t, c, info.Id, "after", info.Path, []byte("a later physical generation"))
			if otherHold == "case" {
				post.Message = "second case still captures"
				_, _, err = th.Client.UpdatePost(context.Background(), post.Id, post)
				require.NoError(t, err)
				require.Contains(t, caseMessageVersions(t, c.fence.db, second.ID, "posts", post.Id), post.Message)
			}
			require.Equal(t, frozen, lifecycleVersionCount(t, c.fence.db, first.ID))
		})
	}
}

func TestCaseLifecycleReactivationCapturesCurrentFileMetadataAfterUnheldChanges(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseLifecycle(context.Background(), m.journal))
	post, info := uploadFixtureFile(t, th, []byte("old held bytes"))
	first := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	exporter, _ := exportFixture(t, m, 16<<20)
	closed, err := m.Release(rctx, exporter, releaseRequest(first.ID))
	require.NoError(t, err)
	newBytes := []byte("new, longer unheld generation")
	_, err = th.Server.FileBackend().WriteFile(bytes.NewReader(newBytes), info.Path)
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`UPDATE fileinfo SET size=$1 WHERE id=$2`, len(newBytes), info.Id)
	require.NoError(t, err)
	var count int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.mutation_files WHERE file_id=$1`, info.Id).Scan(&count))
	require.Zero(t, count, "unheld physical generations must not continue to join the old case")
	second := startChannelCase(t, m, rctx, post.ChannelId)
	current, err := c.fence.LatestFileEvidence(context.Background(), info.Id)
	require.NoError(t, err)
	require.Equal(t, newBytes, readVaultBlob(t, c.vault, current.Objects[0].BlobRef))
	final, err := exporter.Export(rctx, second.ID)
	require.NoError(t, err)
	file, _, err := exporter.Open(rctx, final.ID)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	file, _, err = exporter.Open(rctx, closed.ExportID)
	require.NoError(t, err)
	entries := readExportEntries(t, file)
	require.NoError(t, file.Close())
	for _, content := range entries {
		require.NotEqual(t, newBytes, content)
	}
}

func TestCaseLifecycleReleaseFailureDoesNotDropProtectionAndZIPFailureCanResume(t *testing.T) {
	for _, fault := range []string{"missing-object", "snapshot-limit", "build-limit"} {
		t.Run(fault, func(t *testing.T) {
			th, c, m, rctx, _ := caseFixture(t)
			require.NoError(t, InstallCaseLifecycle(context.Background(), m.journal))
			post, info := uploadFixtureFile(t, th, bytes.Repeat([]byte("large final evidence\x00"), 30000))
			status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
			exporter, _ := exportFixture(t, m, 16<<20)
			switch fault {
			case "missing-object":
				evidence, err := c.fence.LatestFileEvidence(context.Background(), info.Id)
				require.NoError(t, err)
				name, err := blobPath(evidence.Objects[0].BlobRef)
				require.NoError(t, err)
				require.NoError(t, c.vault.root.Remove(name))
			case "snapshot-limit":
				exporter.limit = 64
			case "build-limit":
				exporter.limit = 128 << 10
			}
			input := releaseRequest(status.ID)
			result, err := m.Release(rctx, exporter, input)
			require.Error(t, err)
			details, err := m.Get(rctx, status.ID)
			require.NoError(t, err)
			if fault == "build-limit" {
				require.Equal(t, "released", result.State)
				require.Equal(t, "released", details.State)
				exporter.limit = 16 << 20
				resumed, err := m.Release(rctx, exporter, input)
				require.NoError(t, err)
				require.Equal(t, result, resumed)
				file, _, err := exporter.Open(rctx, result.ExportID)
				require.NoError(t, err)
				require.NoError(t, file.Close())
			} else {
				require.Equal(t, "active", details.State)
				post.Message = "capture continues after failed release"
				_, _, err = th.Client.UpdatePost(context.Background(), post.Id, post)
				require.NoError(t, err)
				require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id), post.Message)
			}
		})
	}
}

func TestCaseLifecyclePreparingCaseCannotBeReleasedBeforeAttachmentsArePreserved(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseLifecycle(context.Background(), m.journal))
	post, info := uploadFixtureFile(t, th, []byte("pending release evidence"))
	preview := info.Path + ".preview"
	_, err := c.fence.db.Exec(`UPDATE fileinfo SET previewpath=$2 WHERE id=$1`, info.Id, preview)
	require.NoError(t, err)
	status, err := m.StartCapture(rctx, CaseRequest{Title: "قضية قيد الحفظ", Scopes: []CaseScope{{Kind: "channels", TargetID: post.ChannelId}}})
	require.Error(t, err)
	require.Equal(t, "preparing", status.State)
	exporter, _ := exportFixture(t, m, 16<<20)
	input := releaseRequest(status.ID)
	_, err = m.Release(rctx, exporter, input)
	require.ErrorContains(t, err, "requires an active case")
	details, err := m.Get(rctx, status.ID)
	require.NoError(t, err)
	require.Equal(t, "preparing", details.State)
	require.Nil(t, details.Release)
	_, err = c.fence.db.Exec(`DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.Error(t, err, "a rejected release must retain pending file protection")
	post.Message = "captured while the release is rejected"
	_, _, err = th.Client.UpdatePost(context.Background(), post.Id, post)
	require.NoError(t, err)
	require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id), post.Message)
	_, err = th.Server.FileBackend().WriteFile(bytes.NewReader([]byte("preserved preview")), preview)
	require.NoError(t, err)
	_, err = m.ResumeCapture(rctx, status.ID)
	require.NoError(t, err)
	released, err := m.Release(rctx, exporter, input)
	require.NoError(t, err)
	require.Equal(t, "released", released.State)
	file, _, err := exporter.Open(rctx, released.ExportID)
	require.NoError(t, err)
	entries := readExportEntries(t, file)
	require.NoError(t, file.Close())
	require.Contains(t, string(entries["versions.jsonl"]), post.Message)
}

func TestCaseLifecycleReleaseCannotReuseAnOrdinaryExportSnapshot(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseLifecycle(context.Background(), m.journal))
	post, _ := uploadFixtureFile(t, th, []byte("ordinary export is not a release"))
	status := startChannelCase(t, m, rctx, post.ChannelId)
	exporter, _ := exportFixture(t, m, 16<<20)
	input := releaseRequest(status.ID)
	ordinary, err := exporter.ExportWithKey(rctx, status.ID, input.RequestKey)
	require.NoError(t, err)
	_, err = m.Release(rctx, exporter, input)
	require.ErrorContains(t, err, "already used for an ordinary export")
	details, err := m.Get(rctx, status.ID)
	require.NoError(t, err)
	require.Equal(t, "active", details.State)
	post.Message = "captured after the ordinary export"
	_, _, err = th.Client.UpdatePost(context.Background(), post.Id, post)
	require.NoError(t, err)
	require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id), post.Message)
	input.RequestKey = model.NewId()
	released, err := m.Release(rctx, exporter, input)
	require.NoError(t, err)
	require.NotEqual(t, ordinary.ID, released.ExportID)
	file, _, err := exporter.Open(rctx, released.ExportID)
	require.NoError(t, err)
	entries := readExportEntries(t, file)
	require.NoError(t, file.Close())
	require.Contains(t, string(entries["versions.jsonl"]), post.Message)
	file, _, err = exporter.Open(rctx, ordinary.ID)
	require.NoError(t, err)
	entries = readExportEntries(t, file)
	require.NoError(t, file.Close())
	require.NotContains(t, string(entries["versions.jsonl"]), post.Message)
}

func TestCaseLifecycleAdministrativeViewsPermissionsAndReleaseAudit(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseLifecycle(context.Background(), m.journal))
	first := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	second := startChannelCase(t, m, rctx, th.BasicChannel2.Id)
	exporter, _ := exportFixture(t, m, 16<<20)
	input := releaseRequest(first.ID)
	for _, caller := range []request.CTX{nil, th.Context.WithSession(&model.Session{UserId: th.BasicUser.Id, Roles: "system_admin"})} {
		_, err := m.Get(caller, first.ID)
		require.ErrorIs(t, err, ErrCaseForbidden)
		_, err = m.List(caller, "", 10)
		require.ErrorIs(t, err, ErrCaseForbidden)
		_, err = m.Release(caller, exporter, input)
		require.ErrorIs(t, err, ErrCaseForbidden)
	}
	th.Server.SetLicense(nil)
	_, err := m.Release(rctx, exporter, input)
	require.ErrorIs(t, err, ErrCaseForbidden)
	th.Server.SetLicense(model.NewTestLicense("compliance"))
	for _, bad := range []CaseReleaseRequest{{CaseID: first.ID, RequestKey: input.RequestKey}, {CaseID: "bad", RequestKey: input.RequestKey, Reason: "reason"}} {
		_, err := m.Release(rctx, exporter, bad)
		require.Error(t, err)
	}
	result, err := m.Release(rctx, exporter, input)
	require.NoError(t, err)
	details, err := m.Get(rctx, first.ID)
	require.NoError(t, err)
	require.Equal(t, "released", details.State)
	require.Equal(t, result.ExportID, details.FinalExportID)
	require.Equal(t, input.RequestKey, details.Release.RequestKey)
	require.Equal(t, th.SystemAdminUser.Id, details.Release.ActorID)
	require.Len(t, details.Scopes, 1)
	page, err := m.List(rctx, "", 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, first.ID, page[0].ID)
	require.Equal(t, "released", page[0].State)
	page, err = m.List(rctx, page[0].ID, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, second.ID, page[0].ID)
	_, err = m.List(rctx, "", 101)
	require.Error(t, err)
	for _, table := range []string{"case_lifecycle_installation", "case_releases"} {
		_, err := c.fence.db.Exec("DELETE FROM workspace_legal_hold." + table)
		require.ErrorContains(t, err, "append only")
		_, err = c.fence.db.Exec("TRUNCATE workspace_legal_hold." + table + " CASCADE")
		require.ErrorContains(t, err, "append only")
	}
	_, err = c.fence.db.Exec(`INSERT INTO workspace_legal_hold.case_releases VALUES($1,$2,$3,$4,'forged',clock_timestamp())`, second.ID, result.ExportID, th.SystemAdminUser.Id, model.NewId())
	require.Error(t, err)
	require.NoError(t, InstallCaseLifecycle(context.Background(), m.journal))
	_, err = c.fence.db.Exec(`ALTER TABLE workspace_legal_hold.case_releases DISABLE TRIGGER immutable_rows`)
	require.NoError(t, err)
	require.ErrorContains(t, InstallCaseLifecycle(context.Background(), m.journal), "triggers are incomplete")
}

func TestCaseLifecycleUpgradeKeepsUnknownLegacyReservationsIndependent(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	_, info := uploadFixtureFile(t, th, []byte("legacy held bytes"))
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	var before, after []byte
	require.NoError(t, c.fence.db.QueryRow(`SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM workspace_legal_hold.file_versions v`).Scan(&before))
	require.NoError(t, InstallCaseLifecycle(context.Background(), m.journal))
	require.NoError(t, c.fence.db.QueryRow(`SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM workspace_legal_hold.file_versions v`).Scan(&after))
	require.Equal(t, before, after)
	var origin string
	require.NoError(t, c.fence.db.QueryRow(`SELECT origin FROM workspace_legal_hold.independent_file_holds WHERE file_id=$1`, info.Id).Scan(&origin))
	require.Equal(t, "legacy", origin)
	exporter, _ := exportFixture(t, m, 16<<20)
	_, err := m.Release(rctx, exporter, releaseRequest(status.ID))
	require.NoError(t, err)
	_, err = th.Server.FileBackend().WriteFile(bytes.NewReader([]byte("legacy future bytes")), info.Path)
	require.NoError(t, err)
	requireMutationBytes(t, c, info.Id, "after", info.Path, []byte("legacy future bytes"))
	_, err = c.fence.db.Exec(`DELETE FROM workspace_legal_hold.independent_file_holds`)
	require.ErrorContains(t, err, "append only")
}

func TestCaseLifecycleReleaseSerializesOldNativeTransactionsAtEveryIsolation(t *testing.T) {
	for _, level := range []sql.IsolationLevel{sql.LevelReadCommitted, sql.LevelRepeatableRead, sql.LevelSerializable} {
		t.Run(level.String(), func(t *testing.T) {
			th, c, m, rctx, _ := caseFixture(t)
			require.NoError(t, InstallCaseLifecycle(context.Background(), m.journal))
			status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
			exporter, _ := exportFixture(t, m, 16<<20)
			tx, err := c.fence.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: level})
			require.NoError(t, err)
			defer tx.Rollback()
			var old string
			require.NoError(t, tx.QueryRow(`SELECT message FROM posts WHERE id=$1`, th.BasicPost.Id).Scan(&old))
			_, err = m.Release(rctx, exporter, releaseRequest(status.ID))
			require.NoError(t, err)
			_, err = tx.Exec(`UPDATE posts SET message='old transaction after release' WHERE id=$1`, th.BasicPost.Id)
			if level == sql.LevelReadCommitted {
				require.NoError(t, err)
				require.NoError(t, tx.Commit())
			} else {
				var pg *pq.Error
				require.ErrorAs(t, err, &pg)
				require.Equal(t, pq.ErrorCode("40001"), pg.Code)
			}
			require.NotContains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", th.BasicPost.Id), "old transaction after release")
		})
	}
}

func TestCaseLifecycleReleaseWaitsForNativeWriterBeforeFinalSnapshot(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseLifecycle(context.Background(), m.journal))
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	exporter, _ := exportFixture(t, m, 16<<20)
	tx, err := c.fence.db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE posts SET message='writer wins before release' WHERE id=$1`, th.BasicPost.Id)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(rctx.Context(), 10*time.Second)
	defer cancel()
	response := make(chan error, 1)
	go func() {
		_, err := m.Release(rctx.WithContext(ctx), exporter, releaseRequest(status.ID))
		response <- err
	}()
	assert.Eventually(t, func() bool {
		var blocked bool
		err := c.fence.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%control SET revision%')`).Scan(&blocked)
		return err == nil && blocked
	}, 2*time.Second, 10*time.Millisecond)
	require.NoError(t, tx.Commit())
	select {
	case err = <-response:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	details, err := m.Get(rctx, status.ID)
	require.NoError(t, err)
	file, _, err := exporter.Open(rctx, details.FinalExportID)
	require.NoError(t, err)
	entries := readExportEntries(t, file)
	require.NoError(t, file.Close())
	require.Contains(t, string(entries["versions.jsonl"]), "writer wins before release")
}

func TestCaseLifecycleNewHoldRecapturesSameMetadataAfterUnheldByteChange(t *testing.T) {
	for _, hold := range []string{"case", "independent"} {
		t.Run(hold, func(t *testing.T) {
			th, c, m, rctx, _ := caseFixture(t)
			require.NoError(t, InstallCaseLifecycle(context.Background(), m.journal))
			beforeBytes, currentBytes := []byte("before"), []byte("after!")
			post, info := uploadFixtureFile(t, th, beforeBytes)
			first := startChannelCase(t, m, rctx, post.ChannelId)
			exporter, _ := exportFixture(t, m, 16<<20)
			firstRelease, err := m.Release(rctx, exporter, releaseRequest(first.ID))
			require.NoError(t, err)
			previous, err := c.fence.LatestFileEvidence(context.Background(), info.Id)
			require.NoError(t, err)
			_, err = th.Server.FileBackend().WriteFile(bytes.NewReader(currentBytes), info.Path)
			require.NoError(t, err)
			var liveMetadata []byte
			require.NoError(t, c.fence.db.QueryRow(`SELECT to_jsonb(f) FROM fileinfo f WHERE id=$1`, info.Id).Scan(&liveMetadata))
			require.JSONEq(t, string(previous.Metadata), string(liveMetadata), "the new acquisition must not rely on a metadata difference")
			if hold == "case" {
				startChannelCase(t, m, rctx, post.ChannelId)
			} else {
				_, err = c.ReserveFile(context.Background(), info.Id)
				require.NoError(t, err)
			}
			current, err := c.fence.LatestFileEvidence(context.Background(), info.Id)
			require.NoError(t, err)
			require.Equal(t, currentBytes, readVaultBlob(t, c.vault, current.Objects[0].BlobRef))
			require.Greater(t, current.VersionID, previous.VersionID)
			file, _, err := exporter.Open(rctx, firstRelease.ExportID)
			require.NoError(t, err)
			entries := readExportEntries(t, file)
			require.NoError(t, file.Close())
			found := false
			for _, content := range entries {
				found = found || bytes.Equal(content, beforeBytes)
				require.NotEqual(t, currentBytes, content)
			}
			require.True(t, found)
		})
	}
}
