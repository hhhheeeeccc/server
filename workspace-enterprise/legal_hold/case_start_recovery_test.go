package legal_hold

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func caseStartCounts(t *testing.T, db *sql.DB) (int, int) {
	t.Helper()
	var cases, keys int
	require.NoError(t, db.QueryRow(`SELECT (SELECT count(*) FROM workspace_legal_hold.cases),(SELECT count(*) FROM workspace_legal_hold.case_start_keys)`).Scan(&cases, &keys))
	return cases, keys
}

func TestCaseStartRecoveryConcurrentRequestsCanonicalScopeAndConflict(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseStartJournal(context.Background(), m.journal))
	input := CaseRequest{Title: "  قضية واحدة  ", Scopes: []CaseScope{{Kind: "users", TargetID: th.BasicUser.Id}, {Kind: "channels", TargetID: th.BasicChannel.Id}}}
	key := model.NewId()
	results, failures := make([]CaseStatus, 4), make([]error, 4)
	var group sync.WaitGroup
	for i := range results {
		other, err := NewCaseManager(th.App, m.journal)
		require.NoError(t, err)
		group.Add(1)
		go func(index int) {
			defer group.Done()
			results[index], failures[index] = other.StartCaptureWithKey(rctx, input, key)
		}(i)
	}
	group.Wait()
	for i, err := range failures {
		require.NoError(t, err)
		require.Equal(t, results[0], results[i])
	}
	cases, keys := caseStartCounts(t, c.fence.db)
	require.Equal(t, 1, cases)
	require.Equal(t, 1, keys)
	input.Title = "قضية واحدة"
	input.Scopes[0], input.Scopes[1] = input.Scopes[1], input.Scopes[0]
	replayed, err := m.StartCaptureWithKey(rctx, input, key)
	require.NoError(t, err)
	require.Equal(t, results[0], replayed)
	require.Equal(t, "channels", input.Scopes[0].Kind, "canonicalization must not reorder the caller's slice")
	for _, different := range []CaseRequest{{Title: "different title", Scopes: input.Scopes}, {Title: input.Title, Scopes: input.Scopes[:1]}} {
		_, err = m.StartCaptureWithKey(rctx, different, key)
		require.ErrorIs(t, err, ErrCaseStartKeyConflict)
	}
	different, err := m.StartCaptureWithKey(rctx, input, model.NewId())
	require.NoError(t, err)
	require.NotEqual(t, replayed.ID, different.ID)
}

func TestCaseStartRecoveryResumesPreparationWithSameIDAndOriginalScope(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseStartJournal(context.Background(), m.journal))
	post, info := uploadFixtureFile(t, th, []byte("original pending bytes"))
	preview := info.Path + ".preview"
	_, err := c.fence.db.Exec(`UPDATE fileinfo SET previewpath=$2 WHERE id=$1`, info.Id, preview)
	require.NoError(t, err)
	input := CaseRequest{Title: "بدء قابل للاستئناف", Scopes: []CaseScope{{Kind: "channels", TargetID: post.ChannelId}}}
	key := model.NewId()
	pending, err := m.StartCaptureWithKey(rctx, input, key)
	require.Error(t, err)
	require.Equal(t, "preparing", pending.State)
	post.Message = "captured after the first committed preparation"
	_, _, err = th.Client.UpdatePost(context.Background(), post.Id, post)
	require.NoError(t, err)
	require.Contains(t, caseMessageVersions(t, c.fence.db, pending.ID, "posts", post.Id), post.Message)
	_, err = c.fence.db.Exec(`DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.Error(t, err)
	_, err = th.Server.FileBackend().WriteFile(bytes.NewReader([]byte("preview evidence")), preview)
	require.NoError(t, err)
	require.NoError(t, InstallCaseStartJournal(context.Background(), m.journal))
	other, err := NewCaseManager(th.App, m.journal)
	require.NoError(t, err)
	resumed, err := other.StartCaptureWithKey(rctx, input, key)
	require.NoError(t, err)
	require.Equal(t, pending.ID, resumed.ID)
	require.Equal(t, "active", resumed.State)
	cases, keys := caseStartCounts(t, c.fence.db)
	require.Equal(t, 1, cases)
	require.Equal(t, 1, keys)
	evidence, err := c.fence.LatestFileEvidence(context.Background(), info.Id)
	require.NoError(t, err)
	require.Len(t, evidence.Objects, 2)
	require.Equal(t, []byte("original pending bytes"), readVaultBlob(t, c.vault, evidence.Objects[0].BlobRef))
}

func TestCaseStartRecoveryReleasedCaseReplaysAfterActualNativeChannelDeletion(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseStartJournal(context.Background(), m.journal))
	data := []byte("released case survives channel removal")
	post, info := uploadFixtureFile(t, th, data)
	input := CaseRequest{Title: "قضية لا تعاد بعد التحرير", Scopes: []CaseScope{{Kind: "channels", TargetID: th.BasicChannel.Id}}}
	key := model.NewId()
	started, err := m.StartCaptureWithKey(rctx, input, key)
	require.NoError(t, err)
	exporter, _ := exportFixture(t, m, 16<<20)
	released, err := m.Release(rctx, exporter, releaseRequest(started.ID))
	require.NoError(t, err)
	frozen := lifecycleVersionCount(t, c.fence.db, started.ID)
	// PermanentDeleteChannel removes posts but does not delete FileInfo or
	// attachment bytes. Exercise their actual native deletion path as well.
	require.Nil(t, th.App.PermanentDeletePost(th.Context, post.Id, th.BasicUser.Id))
	require.Nil(t, th.App.PermanentDeleteChannel(th.Context, th.BasicChannel))
	var exists bool
	require.NoError(t, c.fence.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM channels WHERE id=$1)`, th.BasicChannel.Id).Scan(&exists))
	require.False(t, exists)
	_, err = th.Store.FileInfo().GetFromMaster(info.Id)
	require.Error(t, err)
	replayed, err := m.StartCaptureWithKey(rctx, input, key)
	require.NoError(t, err)
	require.Equal(t, started.ID, replayed.ID)
	require.Equal(t, "released", replayed.State)
	require.Equal(t, frozen, lifecycleVersionCount(t, c.fence.db, started.ID))
	cases, keys := caseStartCounts(t, c.fence.db)
	require.Equal(t, 1, cases)
	require.Equal(t, 1, keys)
	file, _, err := exporter.Open(rctx, released.ExportID)
	require.NoError(t, err)
	entries := readExportEntries(t, file)
	require.NoError(t, file.Close())
	found := false
	for _, content := range entries {
		found = found || bytes.Equal(content, data)
	}
	require.True(t, found, "the final export retains attachment bytes after native source deletion")
}

func TestCaseStartRecoveryLicenseRevokedDuringBytePreservationDoesNotActivate(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseStartJournal(context.Background(), m.journal))
	_, info := uploadFixtureFile(t, th, []byte("held while license changes"))
	input := CaseRequest{Title: "سحب الترخيص أثناء الحفظ", Scopes: []CaseScope{{Kind: "channels", TargetID: th.BasicChannel.Id}}}
	key := model.NewId()
	source := c.source
	blocked := &fenceBlockedSource{FileBackend: source, started: make(chan struct{}), resume: make(chan struct{})}
	c.source = blocked
	defer func() { c.source = source }()
	defer func() {
		select {
		case <-blocked.resume:
		default:
			close(blocked.resume)
		}
	}()
	type outcome struct {
		status CaseStatus
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		status, err := m.StartCaptureWithKey(rctx, input, key)
		done <- outcome{status, err}
	}()
	select {
	case <-blocked.started:
	case <-time.After(10 * time.Second):
		t.Fatal("preservation did not reach the actual source reader")
	}
	th.Server.SetLicense(nil)
	close(blocked.resume)
	var result outcome
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("case start did not finish after the source reader closed")
	}
	c.source = source
	require.ErrorIs(t, result.err, ErrCaseForbidden)
	require.Equal(t, "preparing", result.status.State)
	var active int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_events WHERE state='active'`).Scan(&active))
	require.Zero(t, active)
	cases, keys := caseStartCounts(t, c.fence.db)
	require.Equal(t, 1, cases)
	require.Equal(t, 1, keys, "the committed preparation must retain its request identity")
	_, err := c.fence.LatestFileEvidence(context.Background(), info.Id)
	require.ErrorIs(t, err, ErrPreservationPending)
	_, err = c.fence.db.Exec(`DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.Error(t, err, "revoking administration must not remove preservation protection")
	th.Server.SetLicense(model.NewTestLicense("compliance"))
	recovered, err := m.StartCaptureWithKey(rctx, input, key)
	require.NoError(t, err)
	require.Equal(t, result.status.ID, recovered.ID)
	require.Equal(t, "active", recovered.State)
}

func TestCaseStartRecoveryRequiresInstallationCurrentLicenseAndRealAdmin(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	input := CaseRequest{Title: "صلاحيات البدء", Scopes: []CaseScope{{Kind: "channels", TargetID: th.BasicChannel.Id}}}
	key := model.NewId()
	_, err := m.StartCaptureWithKey(rctx, input, key)
	require.ErrorContains(t, err, "start journal")
	require.NoError(t, InstallCaseStartJournal(context.Background(), m.journal))
	for _, caller := range []request.CTX{nil, th.Context.WithSession(&model.Session{UserId: th.BasicUser.Id, Roles: "system_admin"})} {
		_, err = m.StartCaptureWithKey(caller, input, key)
		require.ErrorIs(t, err, ErrCaseForbidden)
	}
	for _, badKey := range []string{"", "invalid"} {
		_, err = m.StartCaptureWithKey(rctx, input, badKey)
		require.Error(t, err)
	}
	for _, bad := range []CaseRequest{{Title: "", Scopes: input.Scopes}, {Title: strings.Repeat("ع", 257), Scopes: input.Scopes}, {Title: "bad scope", Scopes: append(append([]CaseScope(nil), input.Scopes...), input.Scopes...)}, {Title: "unknown", Scopes: []CaseScope{{Kind: "channels", TargetID: model.NewId()}}}} {
		_, err = m.StartCaptureWithKey(rctx, bad, key)
		require.Error(t, err)
	}
	cases, keys := caseStartCounts(t, c.fence.db)
	require.Zero(t, cases)
	require.Zero(t, keys)
	started, err := m.StartCaptureWithKey(rctx, input, key)
	require.NoError(t, err)
	th.Server.SetLicense(nil)
	_, err = m.StartCaptureWithKey(rctx, input, key)
	require.ErrorIs(t, err, ErrCaseForbidden)
	th.Server.SetLicense(model.NewTestLicense("compliance"))
	other := th.CreateUser(t)
	_, appErr := th.App.UpdateUserRoles(th.Context, other.Id, "system_user system_admin", false)
	require.Nil(t, appErr)
	otherContext := th.Context.WithSession(&model.Session{UserId: other.Id})
	separate, err := m.StartCaptureWithKey(otherContext, input, key)
	require.NoError(t, err)
	require.NotEqual(t, started.ID, separate.ID, "keys belong to the authenticated administrator")
	_, appErr = th.App.UpdateUserRoles(th.Context, other.Id, "system_user", false)
	require.Nil(t, appErr)
	_, err = m.StartCaptureWithKey(otherContext, input, key)
	require.ErrorIs(t, err, ErrCaseForbidden)
}

func TestCaseStartRecoveryUpgradePreservesExistingEvidenceAndProtectsBindings(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	uploadFixtureFile(t, th, []byte("legacy start evidence"))
	legacy := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	var before, after []byte
	query := `SELECT jsonb_build_object('cases',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM workspace_legal_hold.cases c),'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM workspace_legal_hold.case_versions v),'receipts',(SELECT jsonb_agg(to_jsonb(r) ORDER BY version_id) FROM workspace_legal_hold.file_receipts r))`
	require.NoError(t, c.fence.db.QueryRow(query).Scan(&before))
	require.NoError(t, InstallCaseStartJournal(context.Background(), m.journal))
	require.NoError(t, c.fence.db.QueryRow(query).Scan(&after))
	require.Equal(t, before, after)
	_, keys := caseStartCounts(t, c.fence.db)
	require.Zero(t, keys, "an upgrade must not invent a lost request identity")
	input := CaseRequest{Title: "new keyed case", Scopes: []CaseScope{{Kind: "channels", TargetID: th.BasicChannel.Id}}}
	started, err := m.StartCaptureWithKey(rctx, input, model.NewId())
	require.NoError(t, err)
	require.NotEqual(t, legacy.ID, started.ID)
	for _, table := range []string{"case_start_installation", "case_start_keys"} {
		_, err = c.fence.db.Exec("DELETE FROM workspace_legal_hold." + table)
		require.ErrorContains(t, err, "append only")
		_, err = c.fence.db.Exec("TRUNCATE workspace_legal_hold." + table + " CASCADE")
		require.ErrorContains(t, err, "append only")
	}
	_, err = c.fence.db.Exec(`INSERT INTO workspace_legal_hold.case_start_keys VALUES($1,$2,$3,'{}',clock_timestamp())`, th.SystemAdminUser.Id, model.NewId(), legacy.ID)
	require.ErrorContains(t, err, "does not match")
	require.NoError(t, InstallCaseStartJournal(context.Background(), m.journal))
	_, err = c.fence.db.Exec(`ALTER TABLE workspace_legal_hold.case_start_keys DISABLE TRIGGER validate_start_key`)
	require.NoError(t, err)
	require.ErrorContains(t, InstallCaseStartJournal(context.Background(), m.journal), "triggers are incomplete")
	_, err = m.StartCaptureWithKey(rctx, input, model.NewId())
	require.ErrorContains(t, err, "triggers are incomplete")
}

func TestCaseStartRecoveryCancelledOrRevokedWhileWaitingDoesNotCommitBinding(t *testing.T) {
	for _, fault := range []string{"cancelled", "license-revoked"} {
		t.Run(fault, func(t *testing.T) {
			th, c, m, rctx, _ := caseFixture(t)
			require.NoError(t, InstallCaseStartJournal(context.Background(), m.journal))
			tx, err := c.fence.db.Begin()
			require.NoError(t, err)
			defer tx.Rollback()
			_, err = tx.Exec(`UPDATE posts SET message='native writer before keyed start' WHERE id=$1`, th.BasicPost.Id)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(rctx.Context(), 10*time.Second)
			defer cancel()
			input := CaseRequest{Title: "طلب ينتظر الكتابة", Scopes: []CaseScope{{Kind: "channels", TargetID: th.BasicChannel.Id}}}
			key := model.NewId()
			result := make(chan error, 1)
			go func() {
				_, err := m.StartCaptureWithKey(rctx.WithContext(ctx), input, key)
				result <- err
			}()
			require.Eventually(t, func() bool {
				var blocked bool
				err := c.fence.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%control SET revision%')`).Scan(&blocked)
				return err == nil && blocked
			}, 2*time.Second, 10*time.Millisecond)
			if fault == "cancelled" {
				cancel()
			} else {
				th.Server.SetLicense(nil)
			}
			require.NoError(t, tx.Commit())
			select {
			case err = <-result:
				if fault == "license-revoked" {
					require.ErrorIs(t, err, ErrCaseForbidden)
				} else {
					require.Error(t, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("case start did not release its wait")
			}
			cases, keys := caseStartCounts(t, c.fence.db)
			require.Zero(t, cases)
			require.Zero(t, keys)
			th.Server.SetLicense(model.NewTestLicense("compliance"))
			started, err := m.StartCaptureWithKey(rctx, input, key)
			require.NoError(t, err)
			assert.Contains(t, caseMessageVersions(t, c.fence.db, started.ID, "posts", th.BasicPost.Id), "native writer before keyed start")
		})
	}
}
