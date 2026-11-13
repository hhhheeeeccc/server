package legal_hold

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

func scopeChange(caseID string, add, remove []CaseScope) CaseScopeChangeRequest {
	return CaseScopeChangeRequest{CaseID: caseID, RequestKey: model.NewId(), Reason: "تعديل نطاق الحفظ مع إبقاء الأدلة السابقة", Add: add, Remove: remove}
}

func TestCaseScopeChangeSnapshotsAdditionAndFreezesRemovedEntities(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseScopeJournal(context.Background(), m.journal))
	post := th.CreatePost(t)
	former := th.CreateUser(t)
	_, err := c.fence.db.Exec(`UPDATE posts SET userid=$2 WHERE id=$1`, post.Id, former.Id)
	require.NoError(t, err)
	post.UserId = former.Id
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	other := th.CreatePublicChannel(t)
	user := th.CreateUser(t)
	newID := model.NewId()
	_, err = c.fence.db.Exec(`INSERT INTO posts(id,userid,channelid,message,props,fileids) VALUES($1,$2,$3,'existing newly scoped post','{}','[]')`, newID, user.Id, other.Id)
	require.NoError(t, err)
	input := scopeChange(status.ID, []CaseScope{{"channels", other.Id}}, []CaseScope{{"channels", th.BasicChannel.Id}})
	changed, err := m.ChangeScope(rctx, input)
	require.NoError(t, err)
	require.Equal(t, "complete", changed.State)
	require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", newID), "existing newly scoped post")
	before := caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id)
	_, err = c.fence.db.Exec(`UPDATE posts SET message='excluded after scope removal' WHERE id=$1`, post.Id)
	require.NoError(t, err)
	_, err = th.Store.TemporaryPost().Save(th.Context, &model.TemporaryPost{ID: post.Id, Type: model.PostTypeBurnOnRead, Message: "excluded temporary after removal"})
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`UPDATE users SET nickname='excluded former author' WHERE id=$1`, post.UserId)
	require.NoError(t, err)
	require.Equal(t, before, caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id))
	require.Empty(t, caseMessageVersions(t, c.fence.db, status.ID, "temporaryposts", post.Id))
	_, err = c.fence.db.Exec(`UPDATE posts SET message='included new scope edit' WHERE id=$1`, newID)
	require.NoError(t, err)
	require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", newID), "included new scope edit")
	details, err := m.Get(rctx, status.ID)
	require.NoError(t, err)
	require.Equal(t, []CaseScope{{"channels", other.Id}}, details.Scopes)
	require.Empty(t, details.PendingScopeChangeID)
	exporter, _ := exportFixture(t, m, 16<<20)
	result, err := exporter.Export(rctx, status.ID)
	require.NoError(t, err)
	file, _, err := exporter.Open(rctx, result.ID)
	require.NoError(t, err)
	entries := readExportEntries(t, file)
	require.NoError(t, file.Close())
	require.Contains(t, string(entries["events.jsonl"]), input.Reason)
	require.Contains(t, string(entries["events.jsonl"]), "scope_complete")
	require.NotContains(t, string(entries["versions.jsonl"]), "excluded after scope removal")
	require.NotContains(t, string(entries["versions.jsonl"]), "excluded former author")
	require.Contains(t, string(entries["versions.jsonl"]), "existing newly scoped post")
}

func TestCaseScopeChangeFailureKeepsOldAndAddedProtectionUntilRetry(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseScopeJournal(context.Background(), m.journal))
	other := th.CreatePublicChannel(t)
	status := startChannelCase(t, m, rctx, other.Id)
	post, info := uploadFixtureFile(t, th, []byte("pending added attachment"))
	preview := info.Path + ".missing-preview"
	_, err := c.fence.db.Exec(`UPDATE fileinfo SET previewpath=$2 WHERE id=$1`, info.Id, preview)
	require.NoError(t, err)
	input := scopeChange(status.ID, []CaseScope{{"channels", th.BasicChannel.Id}}, []CaseScope{{"channels", other.Id}})
	pending, err := m.ChangeScope(rctx, input)
	require.Error(t, err)
	require.Equal(t, "preparing", pending.State)
	require.True(t, model.IsValidId(pending.ID))
	details, err := m.Get(rctx, status.ID)
	require.NoError(t, err)
	require.Len(t, details.Scopes, 2)
	require.Equal(t, pending.ID, details.PendingScopeChangeID)
	oldPost := model.NewId()
	_, err = c.fence.db.Exec(`INSERT INTO posts(id,userid,channelid,message,props,fileids) VALUES($1,$2,$3,'old scope remains protected while pending','{}','[]')`, oldPost, th.BasicUser.Id, other.Id)
	require.NoError(t, err)
	require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", oldPost), "old scope remains protected while pending")
	exporter, _ := exportFixture(t, m, 16<<20)
	_, err = m.Release(rctx, exporter, releaseRequest(status.ID))
	require.Error(t, err, "an unfinished scope change cannot be bypassed by case release")
	_, err = c.fence.db.Exec(`UPDATE posts SET message='captured during failed addition' WHERE id=$1`, post.Id)
	require.NoError(t, err)
	require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id), "captured during failed addition")
	_, err = c.fence.db.Exec(`DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.Error(t, err)
	_, err = m.ChangeScope(rctx, scopeChange(status.ID, nil, []CaseScope{{"channels", other.Id}}))
	require.ErrorIs(t, err, ErrCaseScopePending)
	_, err = c.source.WriteFile(bytes.NewReader([]byte("repaired preview evidence")), preview)
	require.NoError(t, err)
	require.NoError(t, InstallCaseScopeJournal(context.Background(), m.journal))
	successor, err := NewCaseManager(th.App, m.journal)
	require.NoError(t, err)
	done, err := successor.ChangeScope(rctx, input)
	require.NoError(t, err)
	require.Equal(t, pending.ID, done.ID)
	require.Equal(t, "complete", done.State)
	details, err = m.Get(rctx, status.ID)
	require.NoError(t, err)
	require.Equal(t, []CaseScope{{"channels", th.BasicChannel.Id}}, details.Scopes)
}

func TestCaseScopeChangeValidationAndNativeDeletionAfterRemoval(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	keep := th.CreatePublicChannel(t)
	input := scopeChange(model.NewId(), []CaseScope{{"channels", keep.Id}}, nil)
	_, err := m.ChangeScope(rctx, input)
	require.ErrorContains(t, err, "scope journal")
	require.NoError(t, InstallCaseScopeJournal(context.Background(), m.journal))
	post, info := uploadFixtureFile(t, th, []byte("retained after actual native deletion"))
	status := startChannelCase(t, m, rctx, post.ChannelId)
	valid := scopeChange(status.ID, []CaseScope{{"channels", keep.Id}}, []CaseScope{{"channels", post.ChannelId}})
	for name, alter := range map[string]func(*CaseScopeChangeRequest){
		"unknown-case":   func(r *CaseScopeChangeRequest) { r.CaseID = model.NewId() },
		"invalid-key":    func(r *CaseScopeChangeRequest) { r.RequestKey = "bad" },
		"empty-reason":   func(r *CaseScopeChangeRequest) { r.Reason = " " },
		"long-reason":    func(r *CaseScopeChangeRequest) { r.Reason = strings.Repeat("ع", 2049) },
		"no-changes":     func(r *CaseScopeChangeRequest) { r.Add, r.Remove = nil, nil },
		"duplicate":      func(r *CaseScopeChangeRequest) { r.Add = append(r.Add, r.Add...) },
		"both-actions":   func(r *CaseScopeChangeRequest) { r.Add = r.Remove },
		"unknown-kind":   func(r *CaseScopeChangeRequest) { r.Add = []CaseScope{{"teams", keep.Id}} },
		"missing-target": func(r *CaseScopeChangeRequest) { r.Add = []CaseScope{{"channels", model.NewId()}} },
		"already-held":   func(r *CaseScopeChangeRequest) { r.Add, r.Remove = r.Remove, nil },
		"absent-removal": func(r *CaseScopeChangeRequest) { r.Add, r.Remove = nil, r.Add },
		"last-scope":     func(r *CaseScopeChangeRequest) { r.Add = nil },
	} {
		t.Run(name, func(t *testing.T) {
			bad := valid
			bad.Add = append([]CaseScope(nil), valid.Add...)
			bad.Remove = append([]CaseScope(nil), valid.Remove...)
			alter(&bad)
			_, err := m.ChangeScope(rctx, bad)
			require.Error(t, err)
			var count int
			require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_scope_requests`).Scan(&count))
			require.Zero(t, count, "rejected changes cannot leave a partial request")
		})
	}
	done, err := m.ChangeScope(rctx, valid)
	require.NoError(t, err)
	frozen := lifecycleVersionCount(t, c.fence.db, status.ID)
	require.Nil(t, th.App.PermanentDeletePost(th.Context, post.Id, post.UserId))
	_, err = th.Store.FileInfo().GetFromMaster(info.Id)
	require.Error(t, err)
	present, err := c.source.FileExists(info.Path)
	require.NoError(t, err)
	require.False(t, present)
	require.Equal(t, frozen, lifecycleVersionCount(t, c.fence.db, status.ID))
	replayed, err := m.ChangeScope(rctx, valid)
	require.NoError(t, err)
	require.Equal(t, done, replayed)
	exporter, _ := exportFixture(t, m, 16<<20)
	released, err := m.Release(rctx, exporter, releaseRequest(status.ID))
	require.NoError(t, err)
	file, _, err := exporter.Open(rctx, released.ExportID)
	require.NoError(t, err)
	entries := readExportEntries(t, file)
	require.NoError(t, file.Close())
	found := false
	for _, data := range entries {
		found = found || bytes.Equal(data, []byte("retained after actual native deletion"))
	}
	require.True(t, found)
	_, err = m.ChangeScope(rctx, scopeChange(status.ID, []CaseScope{{"channels", keep.Id}}, nil))
	require.Error(t, err, "released cases cannot change scope")
	replayed, err = m.ChangeScope(rctx, valid)
	require.NoError(t, err)
	require.Equal(t, done, replayed, "completed replay stays a read after release")
}

func TestCaseScopeChangeRevocationDuringSealingRetainsPreparation(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseScopeJournal(context.Background(), m.journal))
	keep := th.CreatePublicChannel(t)
	status := startChannelCase(t, m, rctx, keep.Id)
	_, info := uploadFixtureFile(t, th, []byte("do not release without current permission"))
	input := scopeChange(status.ID, []CaseScope{{"channels", th.BasicChannel.Id}}, []CaseScope{{"channels", keep.Id}})
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
		status CaseScopeChangeStatus
		err    error
	}
	done := make(chan outcome, 1)
	go func() { s, err := m.ChangeScope(rctx, input); done <- outcome{s, err} }()
	select {
	case <-blocked.started:
	case <-time.After(10 * time.Second):
		t.Fatal("scope sealing never reached the source reader")
	}
	th.Server.SetLicense(nil)
	close(blocked.resume)
	var result outcome
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("scope sealing did not finish")
	}
	c.source = source
	require.ErrorIs(t, result.err, ErrCaseForbidden)
	require.Equal(t, "preparing", result.status.State)
	_, err := c.fence.LatestFileEvidence(context.Background(), info.Id)
	require.ErrorIs(t, err, ErrPreservationPending)
	th.Server.SetLicense(model.NewTestLicense("compliance"))
	details, err := m.Get(rctx, status.ID)
	require.NoError(t, err)
	require.Len(t, details.Scopes, 2)
	recovered, err := m.ChangeScope(rctx, input)
	require.NoError(t, err)
	require.Equal(t, result.status.ID, recovered.ID)
	require.Equal(t, "complete", recovered.State)
}

func TestCaseScopeChangeAnotherAdminResumesCreatorRequest(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseScopeJournal(context.Background(), m.journal))
	keep := th.CreatePublicChannel(t)
	status := startChannelCase(t, m, rctx, keep.Id)
	_, info := uploadFixtureFile(t, th, []byte("resumable by another administrator"))
	preview := info.Path + ".preview"
	_, err := c.fence.db.Exec(`UPDATE fileinfo SET previewpath=$2 WHERE id=$1`, info.Id, preview)
	require.NoError(t, err)
	input := scopeChange(status.ID, []CaseScope{{"channels", th.BasicChannel.Id}}, nil)
	pending, err := m.ChangeScope(rctx, input)
	require.Error(t, err)
	_, err = m.ResumeScopeChange(th.Context.WithSession(&model.Session{UserId: th.BasicUser.Id}), pending.ID)
	require.ErrorIs(t, err, ErrCaseForbidden)
	admin := th.CreateUser(t)
	_, appErr := th.App.UpdateUserRoles(th.Context, admin.Id, model.SystemAdminRoleId+" "+model.SystemUserRoleId, false)
	require.Nil(t, appErr)
	otherContext := th.Context.WithSession(&model.Session{UserId: admin.Id})
	_, err = c.source.WriteFile(bytes.NewReader([]byte("preview restored")), preview)
	require.NoError(t, err)
	done, err := m.ResumeScopeChange(otherContext, pending.ID)
	require.NoError(t, err)
	require.Equal(t, "complete", done.State)
	require.Equal(t, pending.ID, done.ID)
	var original, completed string
	require.NoError(t, c.fence.db.QueryRow(`SELECT r.actor_id,c.actor_id FROM workspace_legal_hold.case_scope_requests r JOIN workspace_legal_hold.case_scope_completions c ON c.request_id=r.id WHERE r.id=$1`, done.ID).Scan(&original, &completed))
	require.Equal(t, rctx.Session().UserId, original)
	require.Equal(t, admin.Id, completed)
	replayed, err := m.ChangeScope(rctx, input)
	require.NoError(t, err)
	require.Equal(t, done, replayed)
}

func TestCaseScopeChangeRejectsStaleNativeTransactions(t *testing.T) {
	for _, isolation := range []sql.IsolationLevel{sql.LevelRepeatableRead, sql.LevelSerializable} {
		t.Run(isolation.String(), func(t *testing.T) {
			th, c, m, rctx, _ := caseFixture(t)
			require.NoError(t, InstallCaseScopeJournal(context.Background(), m.journal))
			keep := th.CreatePublicChannel(t)
			status := startChannelCase(t, m, rctx, keep.Id)
			writer, err := c.fence.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: isolation})
			require.NoError(t, err)
			defer writer.Rollback()
			var old string
			require.NoError(t, writer.QueryRow(`SELECT message FROM posts WHERE id=$1`, th.BasicPost.Id).Scan(&old))
			_, err = m.ChangeScope(rctx, scopeChange(status.ID, []CaseScope{{"channels", th.BasicChannel.Id}}, []CaseScope{{"channels", keep.Id}}))
			require.NoError(t, err)
			_, err = writer.Exec(`UPDATE posts SET message='stale writer after scope change' WHERE id=$1`, th.BasicPost.Id)
			var failure *pq.Error
			require.ErrorAs(t, err, &failure)
			require.Equal(t, pq.ErrorCode("40001"), failure.Code)
			require.NoError(t, writer.Rollback())
			_, err = c.fence.db.Exec(`UPDATE posts SET message='fresh writer after scope change' WHERE id=$1`, th.BasicPost.Id)
			require.NoError(t, err)
			require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", th.BasicPost.Id), old)
			require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", th.BasicPost.Id), "fresh writer after scope change")
		})
	}
}

func TestCaseScopeChangeOverlapsAndNewActivationExcludeGapEvidence(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	require.NoError(t, InstallCaseScopeJournal(context.Background(), m.journal))
	data := bytes.Repeat([]byte("A"), 32)
	post, info := uploadFixtureFile(t, th, data)
	keep := th.CreatePublicChannel(t)
	status, err := m.StartCapture(rctx, CaseRequest{Title: "تداخل النطاقات", Scopes: []CaseScope{{"channels", post.ChannelId}, {"users", post.UserId}, {"channels", keep.Id}}})
	require.NoError(t, err)
	_, err = m.ChangeScope(rctx, scopeChange(status.ID, nil, []CaseScope{{"channels", post.ChannelId}}))
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`UPDATE posts SET message='still held by user scope' WHERE id=$1`, post.Id)
	require.NoError(t, err)
	require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id), "still held by user scope")
	_, err = m.ChangeScope(rctx, scopeChange(status.ID, nil, []CaseScope{{"users", post.UserId}}))
	require.NoError(t, err)
	// Another case keeps recording this same physical file while this case has
	// no relevant scope. Its file generations and observations must not leak.
	other := startChannelCase(t, m, rctx, post.ChannelId)
	gap := bytes.Repeat([]byte("B"), 32)
	_, err = th.Server.FileBackend().WriteFile(bytes.NewReader(gap), info.Path)
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`UPDATE posts SET message='outside first case scope' WHERE id=$1`, post.Id)
	require.NoError(t, err)
	// Restore the current bytes before readding. Reacquisition must import only
	// the current generation, never the interval held by the other case alone.
	current := bytes.Repeat([]byte("C"), 32)
	_, err = th.Server.FileBackend().WriteFile(bytes.NewReader(current), info.Path)
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`UPDATE posts SET message='current at readdition' WHERE id=$1`, post.Id)
	require.NoError(t, err)
	_, err = m.ChangeScope(rctx, scopeChange(status.ID, []CaseScope{{"channels", post.ChannelId}}, nil))
	require.NoError(t, err)
	exporter, _ := exportFixture(t, m, 16<<20)
	for _, id := range []string{status.ID, other.ID} {
		result, err := exporter.Export(rctx, id)
		require.NoError(t, err)
		file, _, err := exporter.Open(rctx, result.ID)
		require.NoError(t, err)
		entries := readExportEntries(t, file)
		require.NoError(t, file.Close())
		foundGap, foundCurrent := false, false
		for name, content := range entries {
			if strings.HasPrefix(name, "objects/") {
				foundGap = foundGap || bytes.Equal(content, gap)
				foundCurrent = foundCurrent || bytes.Equal(content, current)
			}
		}
		require.True(t, foundCurrent)
		if id == status.ID {
			require.False(t, foundGap, "another case's unheld-interval bytes must be absent")
			require.NotContains(t, string(entries["versions.jsonl"]), "outside first case scope")
		} else {
			require.True(t, foundGap, "the other case must actually contain the excluded bytes")
		}
	}
	require.Contains(t, caseMessageVersions(t, c.fence.db, other.ID, "posts", post.Id), "outside first case scope")
}

func TestCaseScopeChangeKeysPermissionsAndImmutableUpgrade(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	post := th.CreatePost(t)
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	before := lifecycleVersionCount(t, c.fence.db, status.ID)
	require.NoError(t, InstallCaseScopeJournal(context.Background(), m.journal))
	require.Equal(t, before, lifecycleVersionCount(t, c.fence.db, status.ID), "upgrade cannot invent evidence")
	other := th.CreatePublicChannel(t)
	input := scopeChange(status.ID, []CaseScope{{"channels", other.Id}, {"users", th.BasicUser.Id}}, nil)
	results, failures := make([]CaseScopeChangeStatus, 3), make([]error, 3)
	var group sync.WaitGroup
	for i := range results {
		group.Add(1)
		go func(index int) { defer group.Done(); results[index], failures[index] = m.ChangeScope(rctx, input) }(i)
	}
	group.Wait()
	for i, failure := range failures {
		require.NoError(t, failure)
		require.Equal(t, results[0], results[i])
	}
	input.Add = []CaseScope{input.Add[1], input.Add[0]}
	input.Reason = "  " + input.Reason + "  "
	replayed, err := m.ChangeScope(rctx, input)
	require.NoError(t, err)
	require.Equal(t, results[0], replayed)
	require.Equal(t, "users", input.Add[0].Kind)
	conflict := input
	conflict.Reason = "different reason"
	_, err = m.ChangeScope(rctx, conflict)
	require.ErrorIs(t, err, ErrCaseScopeKeyConflict)
	for _, ctx := range []request.CTX{th.Context, th.Context.WithSession(&model.Session{UserId: th.BasicUser.Id})} {
		_, err = m.ChangeScope(ctx, input)
		require.ErrorIs(t, err, ErrCaseForbidden)
	}
	th.Server.SetLicense(nil)
	_, err = m.ChangeScope(rctx, input)
	require.ErrorIs(t, err, ErrCaseForbidden)
	th.Server.SetLicense(model.NewTestLicense("compliance"))
	_, err = c.fence.db.Exec(`UPDATE posts SET message='still preserved after upgrade' WHERE id=$1`, post.Id)
	require.NoError(t, err)
	require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id), "still preserved after upgrade")
	for _, table := range []string{"case_scope_installation", "case_scope_requests", "case_scope_actions", "case_scope_completions", "case_scope_members", "case_scope_intent_order"} {
		_, err = c.fence.db.Exec("DELETE FROM workspace_legal_hold." + table)
		require.Error(t, err, table)
		_, err = c.fence.db.Exec("TRUNCATE workspace_legal_hold." + table + " CASCADE")
		require.Error(t, err, table)
	}
	var payload []byte
	require.NoError(t, c.fence.db.QueryRow(`SELECT payload FROM workspace_legal_hold.case_scope_requests WHERE id=$1`, replayed.ID).Scan(&payload))
	var recorded map[string]any
	require.NoError(t, json.Unmarshal(payload, &recorded))
	require.Equal(t, strings.TrimSpace(input.Reason), recorded["reason"])
	_, err = c.fence.db.Exec(`ALTER TABLE workspace_legal_hold.case_scope_actions DISABLE TRIGGER validate_action`)
	require.NoError(t, err)
	require.Error(t, InstallCaseScopeJournal(context.Background(), m.journal), "reopen must reject incomplete guards")
	_, err = c.fence.db.Exec(`ALTER TABLE workspace_legal_hold.case_scope_actions ENABLE ALWAYS TRIGGER validate_action`)
	require.NoError(t, err)
}
