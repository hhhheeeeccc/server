package legal_hold

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"image"
	"image/png"
	"path/filepath"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func caseFixture(t *testing.T) (*api4.TestHelper, *FileCoordinator, *CaseManager, request.CTX, string) {
	t.Helper()
	live := t.TempDir()
	vault, archive := openTestVault(t)
	var c *FileCoordinator
	var j *CaseJournal
	key := filepath.Clean(live)
	startupInitializers.Store(key, func(server *app.Server) error {
		fence, err := InstallFileFence(context.Background(), server.Store().GetInternalMasterDB())
		if err != nil {
			return err
		}
		c, err = NewFileCoordinator(context.Background(), fence, vault, server.FileBackend())
		if err != nil {
			return err
		}
		j, err = InstallCaseJournal(context.Background(), c)
		if err != nil {
			return err
		}
		return InstallBackendBeforeStart(server, c)
	})
	t.Cleanup(func() { startupInitializers.Delete(key) })
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.FileSettings.Directory = &live
		cfg.FileSettings.ExtractContent = model.NewPointer(false)
		cfg.EmailSettings.SendEmailNotifications = model.NewPointer(false)
		cfg.EmailSettings.SendPushNotifications = model.NewPointer(false)
	}).InitBasic(t)
	t.Cleanup(func() {
		_, err := th.Store.GetInternalMasterDB().Exec(`DROP SCHEMA workspace_legal_hold CASCADE`)
		require.NoError(t, err)
	})
	require.True(t, th.Server.SetLicense(model.NewTestLicense("compliance")))
	require.NotNil(t, c)
	require.NotNil(t, j)
	m, err := NewCaseManager(th.App, j)
	require.NoError(t, err)
	return th, c, m, th.Context.WithSession(&model.Session{UserId: th.SystemAdminUser.Id}), archive
}

func TestCasesFirstAuthorSnapshotSerializesWithConcurrentIdentityMutation(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	user := th.CreateUser(t)
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	updater, err := c.fence.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer updater.Rollback()
	_, err = updater.ExecContext(ctx, `UPDATE users SET nickname='new concurrent identity' WHERE id=$1`, user.Id)
	require.NoError(t, err)
	conn, err := c.fence.db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	var writerPID int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID))
	result := make(chan error, 1)
	postID := model.NewId()
	go func() {
		_, err := conn.ExecContext(ctx, `INSERT INTO posts(id,userid,channelid,message,props,fileids) VALUES($1,$2,$3,'first message by this author','{}','[]')`, postID, user.Id, th.BasicChannel.Id)
		result <- err
	}()
	assert.Eventually(t, func() bool {
		var blocked bool
		err := c.fence.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1),false)`, writerPID).Scan(&blocked)
		return err == nil && blocked
	}, 2*time.Second, 10*time.Millisecond, "first author snapshot must wait for an uncommitted identity mutation")
	require.NoError(t, updater.Commit())
	select {
	case err = <-result:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var nickname string
	require.NoError(t, c.fence.db.QueryRow(`SELECT data->>'nickname' FROM workspace_legal_hold.case_versions WHERE case_id=$1 AND kind='users' AND entity_id=$2 ORDER BY id DESC LIMIT 1`, status.ID, user.Id).Scan(&nickname))
	require.Equal(t, "new concurrent identity", nickname, "the committed identity must not be lost at first membership")
}

func TestCasesRejectStaleFirstIdentitySnapshotAtHigherIsolation(t *testing.T) {
	for _, isolation := range []sql.IsolationLevel{sql.LevelRepeatableRead, sql.LevelSerializable} {
		t.Run(isolation.String(), func(t *testing.T) {
			th, c, m, rctx, _ := caseFixture(t)
			user := th.CreateUser(t)
			status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			conn, err := c.fence.db.Conn(ctx)
			require.NoError(t, err)
			defer conn.Close()
			var writerPID int
			require.NoError(t, conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID))
			writer, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: isolation})
			require.NoError(t, err)
			defer writer.Rollback()
			var before string
			require.NoError(t, writer.QueryRowContext(ctx, `SELECT nickname FROM users WHERE id=$1`, user.Id).Scan(&before))
			updater, err := c.fence.db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer updater.Rollback()
			_, err = updater.ExecContext(ctx, `UPDATE users SET nickname='identity newer than writer snapshot' WHERE id=$1`, user.Id)
			require.NoError(t, err)
			result := make(chan error, 1)
			postID := model.NewId()
			go func() {
				_, err := writer.ExecContext(ctx, `INSERT INTO posts(id,userid,channelid,message,props,fileids) VALUES($1,$2,$3,'stale first identity','{}','[]')`, postID, user.Id, th.BasicChannel.Id)
				result <- err
			}()
			waitForFenceSQLLock(t, c.fence.db, writerPID)
			require.NoError(t, updater.Commit())
			var writeErr error
			select {
			case writeErr = <-result:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var failure *pq.Error
			require.ErrorAs(t, writeErr, &failure, "advisory waiting cannot refresh a repeatable source snapshot")
			require.Equal(t, pq.ErrorCode("40001"), failure.Code)
			require.NoError(t, writer.Rollback())
			_, err = c.fence.db.ExecContext(ctx, `INSERT INTO posts(id,userid,channelid,message,props,fileids) VALUES($1,$2,$3,'retry with a fresh snapshot','{}','[]')`, postID, user.Id, th.BasicChannel.Id)
			require.NoError(t, err)
			var nickname string
			require.NoError(t, c.fence.db.QueryRowContext(ctx, `SELECT data->>'nickname' FROM workspace_legal_hold.case_versions WHERE case_id=$1 AND kind='users' AND entity_id=$2 ORDER BY id DESC LIMIT 1`, status.ID, user.Id).Scan(&nickname))
			require.Equal(t, "identity newer than writer snapshot", nickname)
		})
	}
}

func startChannelCase(t *testing.T, m *CaseManager, rctx request.CTX, channelID string) CaseStatus {
	t.Helper()
	status, err := m.StartCapture(rctx, CaseRequest{Title: "قضية حفظ محلية", Scopes: []CaseScope{{Kind: "channels", TargetID: channelID}}})
	require.NoError(t, err)
	require.Equal(t, "active", status.State)
	return status
}

func caseMessageVersions(t *testing.T, db *sql.DB, caseID, kind, id string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT data->>'message' FROM workspace_legal_hold.case_versions WHERE case_id=$1 AND kind=$2 AND entity_id=$3 ORDER BY id`, caseID, kind, id)
	require.NoError(t, err)
	defer rows.Close()
	var result []string
	for rows.Next() {
		var text sql.NullString
		require.NoError(t, rows.Scan(&text))
		result = append(result, text.String)
	}
	require.NoError(t, rows.Err())
	return result
}

func TestCasesPreserveNativeEditsHistoryDeletionAndAttachmentBytes(t *testing.T) {
	th, c, m, rctx, archive := caseFixture(t)
	data := bytes.Repeat([]byte("رسالة ومرفق محفوظ\x00"), 30000)
	post, info := uploadFixtureFile(t, th, data)
	original := post.Message
	post.Message = "تحرير قبل بدء القضية"
	updated, _, err := th.Client.UpdatePost(context.Background(), post.Id, post)
	require.NoError(t, err)
	history, err := th.Store.Post().GetEditHistoryForPost(post.Id)
	require.NoError(t, err)
	require.NotEmpty(t, history)
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	for _, previous := range history {
		require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", previous.Id), previous.Message)
	}
	updated.Message = "تحرير بعد بدء القضية"
	updated, _, err = th.Client.UpdatePost(context.Background(), post.Id, updated)
	require.NoError(t, err)
	_, err = th.Client.DeletePost(context.Background(), post.Id)
	require.NoError(t, err)
	require.Nil(t, th.App.PermanentDeletePost(th.Context, post.Id, th.BasicUser.Id))
	var remaining int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM posts WHERE id=$1`, post.Id).Scan(&remaining))
	require.Zero(t, remaining, "the native permanent delete must actually remove the source post")
	_, err = th.Store.FileInfo().GetFromMaster(info.Id)
	require.Error(t, err)
	versions := caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id)
	require.Contains(t, versions, "تحرير قبل بدء القضية")
	require.Contains(t, versions, "تحرير بعد بدء القضية")
	require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", history[0].Id), original)
	var deleted bool
	require.NoError(t, c.fence.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM workspace_legal_hold.case_versions WHERE case_id=$1 AND kind='posts' AND entity_id=$2 AND operation='delete')`, status.ID, post.Id).Scan(&deleted))
	require.True(t, deleted)
	evidence, err := c.fence.LatestFileEvidence(context.Background(), info.Id)
	require.NoError(t, err)
	require.NoError(t, c.vault.Close())
	reopened, err := OpenLocalVault(archive)
	require.NoError(t, err)
	defer reopened.Close()
	require.Equal(t, data, readVaultBlob(t, reopened, evidence.Objects[0].BlobRef))
	t.Log("native edit history, later edits, soft deletion and actual permanent deletion preserved with independent attachment bytes after archive reopen")
}

func TestCasesCaptureFutureImagesAndTemporaryBodiesWithoutBreakingAttachment(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	var imageBytes bytes.Buffer
	require.NoError(t, png.Encode(&imageBytes, image.NewNRGBA(image.Rect(0, 0, 640, 480))))
	upload, _, err := th.Client.UploadFile(context.Background(), imageBytes.Bytes(), th.BasicChannel.Id, "capture.png")
	require.NoError(t, err)
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "صورة جديدة بعد التفعيل", FileIds: model.StringArray{upload.FileInfos[0].Id}})
	require.NoError(t, err)
	info, err := th.Store.FileInfo().GetFromMaster(upload.FileInfos[0].Id)
	require.NoError(t, err)
	require.Equal(t, post.Id, info.PostId, "native attachment update must really succeed, not just post creation")
	require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id), post.Message)
	temporary := &model.TemporaryPost{ID: post.Id, Type: model.PostTypeBurnOnRead, ExpireAt: model.GetMillis() + 60000, Message: "جسم مؤقت أول", FileIDs: post.FileIds}
	_, err = th.Store.TemporaryPost().Save(th.Context, temporary)
	require.NoError(t, err)
	temporary.Message = "جسم مؤقت ثان"
	_, err = th.Store.TemporaryPost().Save(th.Context, temporary)
	require.NoError(t, err, "native temporary-body upsert remains usable")
	_, err = c.fence.db.Exec(`INSERT INTO temporaryposts(postid,type,expireat,message,fileids) VALUES($1,'ignored',0,'ghost body','[]') ON CONFLICT DO NOTHING`, post.Id)
	require.NoError(t, err)
	versions := caseMessageVersions(t, c.fence.db, status.ID, "temporaryposts", post.Id)
	require.Contains(t, versions, "جسم مؤقت أول")
	require.Contains(t, versions, "جسم مؤقت ثان")
	require.NotContains(t, versions, "ghost body", "ignored writes are not actual versions")
	require.Nil(t, th.App.PermanentDeletePost(th.Context, post.Id, th.BasicUser.Id))
	versions = caseMessageVersions(t, c.fence.db, status.ID, "temporaryposts", post.Id)
	require.Contains(t, versions, "جسم مؤقت ثان")
	var gone bool
	require.NoError(t, c.fence.db.QueryRow(`SELECT NOT EXISTS(SELECT 1 FROM temporaryposts WHERE postid=$1)`, post.Id).Scan(&gone))
	require.True(t, gone)
	evidence, err := c.fence.LatestFileEvidence(context.Background(), info.Id)
	require.NoError(t, err)
	require.Equal(t, imageBytes.Bytes(), readVaultBlob(t, c.vault, evidence.Objects[0].BlobRef))
}

func TestCasesGuardFileDuringNativePostAttachmentInterval(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	data := []byte("uploaded outside the channel before native attachment")
	upload, _, err := th.Client.UploadFile(context.Background(), data, th.BasicChannel2.Id, "gap.bin")
	require.NoError(t, err)
	info, err := th.Store.FileInfo().GetFromMaster(upload.FileInfos[0].Id)
	require.NoError(t, err)
	post, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: th.BasicChannel.Id, UserId: th.BasicUser.Id, Message: "stored before attachment", FileIds: model.StringArray{info.Id}})
	require.NoError(t, err)
	require.Empty(t, info.PostId)
	require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id), post.Message)
	_, err = c.fence.db.Exec(`DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.ErrorContains(t, err, "preservation is pending", "direct SQL cannot drop a newly referenced unsealed file")
	require.NoError(t, th.Server.FileBackend().RemoveFile(info.Path))
	evidence, err := c.fence.LatestFileEvidence(context.Background(), info.Id)
	require.NoError(t, err)
	require.Equal(t, data, readVaultBlob(t, c.vault, evidence.Objects[0].BlobRef))
	_, err = c.fence.db.Exec(`DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.NoError(t, err)
	t.Log("case membership guarded bytes after native Post.Save and before FileInfo.AttachToPost")
}

func TestCasesCannotReferenceFileInsideAnUnheldPhysicalDeletion(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	upload, _, err := th.Client.UploadFile(context.Background(), []byte("unheld bytes before removal"), th.BasicChannel2.Id, "unheld.bin")
	require.NoError(t, err)
	info, err := th.Store.FileInfo().GetFromMaster(upload.FileInfos[0].Id)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	entered, resume := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}()
	done := make(chan error, 1)
	go func() {
		done <- c.Apply(ctx, c.source, Mutation{Operation: "remove", Paths: []string{info.Path}}, func() error {
			err := c.source.RemoveFile(info.Path)
			close(entered)
			<-resume
			return err
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Actual deletion has happened inside an open physical intent. A new held
	// post must not claim that file after the deletion selected no held paths.
	post, writeErr := th.Store.Post().Save(th.Context, &model.Post{ChannelId: th.BasicChannel.Id, UserId: th.BasicUser.Id, Message: "must not claim already deleting bytes", FileIds: model.StringArray{info.Id}})
	assert.ErrorContains(t, writeErr, "physical mutation is pending", "a new case reference must not cross an unheld physical intent")
	assert.Nil(t, post)
	close(resume)
	select {
	case err = <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var claims int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_members WHERE case_id=$1 AND kind='fileinfo' AND entity_id=$2`, status.ID, info.Id).Scan(&claims))
	require.Zero(t, claims, "the rejected native write must roll back new case membership")
}

func TestCasesPreparationFailureStaysPendingAndResumesWithOriginalScope(t *testing.T) {
	th, c, m, rctx, archive := caseFixture(t)
	_, info := uploadFixtureFile(t, th, []byte("pending case evidence"))
	// A missing declared derivative makes initial sealing fail. The committed
	// metadata and scope must remain protected instead of claiming activation.
	_, err := c.fence.db.Exec(`UPDATE fileinfo SET previewpath=$2 WHERE id=$1`, info.Id, info.Path+".preview")
	require.NoError(t, err)
	status, err := m.StartCapture(rctx, CaseRequest{Title: "قضية معلقة", Scopes: []CaseScope{{Kind: "channels", TargetID: th.BasicChannel.Id}}})
	require.Error(t, err)
	require.Equal(t, "preparing", status.State)
	require.True(t, model.IsValidId(status.ID))
	stored, err := m.status(context.Background(), status.ID)
	require.NoError(t, err)
	require.Equal(t, status, stored)
	_, err = c.fence.db.Exec(`INSERT INTO workspace_legal_hold.case_events(case_id,state,actor_id) VALUES($1,'active',$2)`, status.ID, th.SystemAdminUser.Id)
	require.ErrorContains(t, err, "attachments are not sealed")
	_, err = c.fence.db.Exec(`DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.Error(t, err)
	// The cooperating writer may create the missing derivative without
	// destroying an existing generation. Completion must then seal the file.
	_, err = th.Server.FileBackend().WriteFile(bytes.NewReader([]byte("preview")), info.Path+".preview")
	require.NoError(t, err)
	require.NoError(t, c.vault.Close())
	reopened, err := OpenLocalVault(archive)
	require.NoError(t, err)
	defer reopened.Close()
	c2, err := NewFileCoordinator(context.Background(), c.fence, reopened, c.source)
	require.NoError(t, err)
	j2, err := InstallCaseJournal(context.Background(), c2)
	require.NoError(t, err)
	m2, err := NewCaseManager(th.App, j2)
	require.NoError(t, err)
	status, err = m2.ResumeCapture(rctx, status.ID)
	require.NoError(t, err)
	require.Equal(t, "active", status.State)
	evidence, err := c.fence.LatestFileEvidence(context.Background(), info.Id)
	require.NoError(t, err)
	require.Len(t, evidence.Objects, 2)
}

func TestCasesRequireLiveComplianceLicenseAndCurrentAdministrator(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	input := CaseRequest{Title: "صلاحيات", Scopes: []CaseScope{{Kind: "users", TargetID: th.BasicUser.Id}}}
	for _, caller := range []request.CTX{nil, th.Context, th.Context.WithSession(&model.Session{UserId: th.BasicUser.Id})} {
		_, err := m.StartCapture(caller, input)
		require.ErrorIs(t, err, ErrCaseForbidden)
	}
	license := model.NewTestLicense("compliance")
	license.Features.Compliance = model.NewPointer(false)
	th.Server.SetLicense(license)
	_, err := m.StartCapture(rctx, input)
	require.ErrorIs(t, err, ErrCaseForbidden)
	th.Server.SetLicense(nil)
	_, err = m.StartCapture(rctx, input)
	require.ErrorIs(t, err, ErrCaseForbidden)
	var count int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.cases`).Scan(&count))
	require.Zero(t, count)
	th.Server.SetLicense(model.NewTestLicense("compliance"))
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "before license removal"})
	require.NoError(t, err)
	status, err := m.StartCapture(rctx, input)
	require.NoError(t, err)
	th.Server.SetLicense(nil)
	_, err = c.fence.db.Exec(`UPDATE posts SET message='preserved after license removal' WHERE id=$1`, post.Id)
	require.NoError(t, err)
	require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id), "preserved after license removal")
	_, err = m.ResumeCapture(rctx, status.ID)
	require.ErrorIs(t, err, ErrCaseForbidden)
}

func TestCasesScopeSnapshotAndNativeWritesSerializeAtAllIsolationLevels(t *testing.T) {
	for _, level := range []sql.IsolationLevel{sql.LevelReadCommitted, sql.LevelRepeatableRead, sql.LevelSerializable} {
		t.Run(level.String(), func(t *testing.T) {
			th, c, m, rctx, _ := caseFixture(t)
			tx, err := c.fence.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: level})
			require.NoError(t, err)
			defer tx.Rollback()
			var old string
			require.NoError(t, tx.QueryRow(`SELECT message FROM posts WHERE id=$1`, th.BasicPost.Id).Scan(&old))
			status := startChannelCase(t, m, rctx, th.BasicPost.ChannelId)
			_, err = tx.Exec(`UPDATE posts SET message='written from a preactivation transaction' WHERE id=$1`, th.BasicPost.Id)
			if level == sql.LevelReadCommitted {
				require.NoError(t, err)
				require.NoError(t, tx.Commit())
				require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", th.BasicPost.Id), "written from a preactivation transaction")
			} else {
				var failure *pq.Error
				require.ErrorAs(t, err, &failure)
				require.Equal(t, pq.ErrorCode("40001"), failure.Code)
			}
			require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", th.BasicPost.Id), old)
		})
	}
}

func TestCasesRejectEvidenceMutationUnsafeScopeAndDisabledTriggers(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	for _, scopes := range [][]CaseScope{
		nil, {{Kind: "all", TargetID: th.BasicUser.Id}}, {{Kind: "users", TargetID: model.NewId()}},
		{{Kind: "users", TargetID: th.BasicUser.Id}, {Kind: "users", TargetID: th.BasicUser.Id}},
	} {
		_, err := m.StartCapture(rctx, CaseRequest{Title: "نطاق غير صالح", Scopes: scopes})
		require.Error(t, err)
	}
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	for _, table := range []string{"case_installation", "cases", "case_scopes", "case_events", "case_members", "case_versions", "case_entity_barriers"} {
		for _, operation := range []string{"DELETE FROM ", "TRUNCATE "} {
			_, err := c.fence.db.Exec(operation + "workspace_legal_hold." + table)
			require.Error(t, err, operation+table)
		}
	}
	_, err := c.fence.db.Exec(`UPDATE workspace_legal_hold.case_entity_barriers SET revision=revision`)
	require.ErrorContains(t, err, "coordination revision must advance")
	var tables, triggers int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='workspace_legal_hold' AND c.relkind='r'`).Scan(&tables))
	require.Equal(t, 18, tables)
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE NOT t.tgisinternal AND t.tgenabled='A' AND (n.nspname='workspace_legal_hold' OR t.tgname LIKE 'workspace_legal_hold_%')`).Scan(&triggers))
	require.Equal(t, 59, triggers)
	for _, table := range caseSourceNames {
		_, err := c.fence.db.Exec("TRUNCATE " + table + " CASCADE")
		require.Error(t, err)
	}
	_, err = c.fence.db.Exec(`INSERT INTO workspace_legal_hold.case_scopes VALUES($1,'users',$2)`, status.ID, th.BasicUser2.Id)
	require.ErrorContains(t, err, "scopes are fixed")
	_, err = c.fence.db.Exec(`ALTER TABLE posts DISABLE TRIGGER workspace_legal_hold_case_after`)
	require.NoError(t, err)
	_, err = InstallCaseJournal(context.Background(), c)
	require.ErrorContains(t, err, "triggers are incomplete")
	_, err = c.fence.db.Exec(`ALTER TABLE posts ENABLE ALWAYS TRIGGER workspace_legal_hold_case_after`)
	require.NoError(t, err)
	_, err = InstallCaseJournal(context.Background(), c)
	require.NoError(t, err)
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "observed identity"})
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`DELETE FROM posts WHERE id=$1`, post.Id)
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`INSERT INTO posts(id,userid,channelid,message) VALUES($1,$2,$3,'reused identity')`, post.Id, th.BasicUser.Id, th.BasicChannel.Id)
	require.ErrorContains(t, err, "ID cannot be reused")
	other, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: th.BasicChannel2.Id, Message: "unheld identity"})
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`UPDATE posts SET id=$2 WHERE id=$1`, other.Id, post.Id)
	require.ErrorContains(t, err, "ID cannot change")
	replica, err := c.fence.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer replica.Rollback()
	_, err = replica.Exec(`SET LOCAL session_replication_role=replica`)
	require.NoError(t, err)
	_, err = replica.Exec(`UPDATE posts SET message='captured even in replica mode' WHERE id=$1`, th.BasicPost.Id)
	require.NoError(t, err)
	require.NoError(t, replica.Commit())
	require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", th.BasicPost.Id), "captured even in replica mode")
}

func TestCasesSnapshotFailureRollsBackActualNativeWriteAndHidesCredentials(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "message for SQL failure proof"})
	require.NoError(t, err)
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	_, err = c.fence.db.Exec(`UPDATE users SET password='never archive password',mfasecret='never archive mfa',authdata='never archive auth',nickname='new display identity with private credentials' WHERE id=$1`, th.BasicUser.Id)
	require.NoError(t, err)
	var payload []byte
	require.NoError(t, c.fence.db.QueryRow(`SELECT data FROM workspace_legal_hold.case_versions WHERE case_id=$1 AND kind='users' AND entity_id=$2 ORDER BY id DESC LIMIT 1`, status.ID, th.BasicUser.Id).Scan(&payload))
	var fields map[string]any
	require.NoError(t, json.Unmarshal(payload, &fields))
	require.Equal(t, th.BasicUser.Username, fields["username"])
	require.Equal(t, "new display identity with private credentials", fields["nickname"])
	for _, key := range []string{"password", "mfasecret", "authdata"} {
		require.NotContains(t, fields, key)
	}
	var original string
	require.NoError(t, c.fence.db.QueryRow(`SELECT message FROM posts WHERE id=$1`, post.Id).Scan(&original))
	_, err = c.fence.db.Exec(`ALTER TABLE workspace_legal_hold.case_versions ADD CONSTRAINT test_archive_failure CHECK(false) NOT VALID`)
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`UPDATE posts SET message='must not commit' WHERE id=$1`, post.Id)
	require.Error(t, err)
	var retained string
	require.NoError(t, c.fence.db.QueryRow(`SELECT message FROM posts WHERE id=$1`, post.Id).Scan(&retained))
	require.Equal(t, original, retained)
	_, err = c.fence.db.Exec(`ALTER TABLE workspace_legal_hold.case_versions DROP CONSTRAINT test_archive_failure`)
	require.NoError(t, err)
}

func TestCasesNativeChannelAndUserDeletionPreserveScopedData(t *testing.T) {
	for _, kind := range []string{"channels", "users"} {
		t.Run(kind, func(t *testing.T) {
			th, c, m, rctx, _ := caseFixture(t)
			data := []byte("byte evidence before native bulk deletion")
			post, info := uploadFixtureFile(t, th, data)
			target := th.BasicChannel.Id
			if kind == "users" {
				target = th.BasicUser.Id
			}
			status, err := m.StartCapture(rctx, CaseRequest{Title: "حذف جماعي أصلي", Scopes: []CaseScope{{Kind: kind, TargetID: target}}})
			require.NoError(t, err)
			if kind == "channels" {
				require.Nil(t, th.App.PermanentDeleteChannel(th.Context, th.BasicChannel))
			} else {
				require.Nil(t, th.App.PermanentDeleteUser(th.Context, th.BasicUser))
			}
			var remaining int
			require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM posts WHERE id=$1`, post.Id).Scan(&remaining))
			require.Zero(t, remaining)
			require.NoError(t, c.fence.db.QueryRow("SELECT count(*) FROM "+kind+" WHERE id=$1", target).Scan(&remaining))
			require.Zero(t, remaining)
			require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id), post.Message)
			var tombstone bool
			require.NoError(t, c.fence.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM workspace_legal_hold.case_versions WHERE case_id=$1 AND kind=$2 AND entity_id=$3 AND operation='delete')`, status.ID, kind, target).Scan(&tombstone))
			require.True(t, tombstone)
			evidence, err := c.fence.LatestFileEvidence(context.Background(), info.Id)
			require.NoError(t, err)
			require.Equal(t, data, readVaultBlob(t, c.vault, evidence.Objects[0].BlobRef))
		})
	}
}

func TestCasesActivationWaitsForActualNativeDeleteCommitOrRollback(t *testing.T) {
	for _, commit := range []bool{false, true} {
		name := "rollback"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			th, c, m, rctx, _ := caseFixture(t)
			tx, err := c.fence.db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			defer tx.Rollback()
			_, err = tx.Exec(`DELETE FROM posts WHERE id=$1`, th.BasicPost.Id)
			require.NoError(t, err)
			type outcome struct {
				status CaseStatus
				err    error
			}
			result := make(chan outcome, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			go func() {
				status, err := m.StartCapture(rctx.WithContext(ctx), CaseRequest{Title: "سباق حذف وتفعيل", Scopes: []CaseScope{{Kind: "channels", TargetID: th.BasicPost.ChannelId}}})
				result <- outcome{status, err}
			}()
			require.Eventually(t, func() bool {
				var blocked int
				err := c.fence.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'UPDATE workspace_legal_hold.control SET revision=%'`).Scan(&blocked)
				return err == nil && blocked == 1
			}, 4*time.Second, 10*time.Millisecond, "PostgreSQL must confirm the activation waits on the native transaction")
			if commit {
				require.NoError(t, tx.Commit())
			} else {
				require.NoError(t, tx.Rollback())
			}
			var out outcome
			select {
			case out = <-result:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			require.NoError(t, out.err)
			versions := caseMessageVersions(t, c.fence.db, out.status.ID, "posts", th.BasicPost.Id)
			if commit {
				require.Empty(t, versions, "a post deleted before capture starts is not invented as preserved")
			} else {
				require.Contains(t, versions, th.BasicPost.Message)
			}
		})
	}
}
