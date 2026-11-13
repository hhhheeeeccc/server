package content_extraction_test

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lockFileContent(t *testing.T, th *api4.TestHelper, id string) *sql.Tx {
	t.Helper()
	tx, err := th.Store.GetInternalMasterDB().BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	var stored string
	require.NoError(t, tx.QueryRow(`SELECT Id FROM FileInfo WHERE Id=$1 FOR UPDATE`, id).Scan(&stored))
	return tx
}

func waitingContentBackend(t *testing.T, th *api4.TestHelper) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		return th.Store.GetInternalMasterDB().QueryRow(`SELECT pid FROM pg_stat_activity
            WHERE datname=current_database() AND wait_event_type='Lock'
            AND query ILIKE '%UPDATE%FileInfo%Content%' LIMIT 1`).Scan(&pid) == nil
	}, 10*time.Second, 10*time.Millisecond, "the real worker must reach its content UPDATE while the file row is locked")
	return pid
}

func TestRegisteredExtractionStopInterruptsLockedContentWrite(t *testing.T) {
	th := setup(t)
	info := upload(t, th, "content after restart", "content-lock.txt", time.Now().Add(-time.Minute))
	tx := lockFileContent(t, th, info.Id)
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, nil)
	require.Nil(t, appErr)
	pid := waitingContentBackend(t, th)
	done := make(chan error, 1)
	go func() { done <- th.Server.Jobs.StopWorkers() }()
	returned := false
	select {
	case err := <-done:
		require.NoError(t, err)
		returned = true
	case <-time.After(2 * time.Second):
		assert.Fail(t, "StopWorkers remained blocked inside the content write")
	}
	if returned {
		// Join the server-side statement before unlocking its fixture. This
		// distinguishes an aborted write from a late unacknowledged commit.
		require.Eventually(t, func() bool {
			var n int
			return th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&n) == nil && n == 0
		}, 4*time.Second, 10*time.Millisecond)
	}
	require.NoError(t, tx.Rollback())
	if !returned {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("content writer did not exit after unlocking the fixture")
		}
	}
	require.Empty(t, get(t, th, info.Id).Content, "a canceled locked writer must not later save its old output")
	require.Contains(t, []string{model.JobStatusPending, model.JobStatusInProgress}, currentJob(t, th, job.Id).Status)
	require.NoError(t, th.Server.Jobs.StartWorkers())
	require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == model.JobStatusSuccess }, 10*time.Second, 20*time.Millisecond)
	require.Equal(t, "content after restart", get(t, th, info.Id).Content)
}

func TestRegisteredExtractionClipsUnicodeContent(t *testing.T) {
	const limit = 1024 * 1024
	for _, extension := range []string{"txt", "docx"} {
		t.Run(extension, func(t *testing.T) {
			th := setup(t)
			// The native DOCX converter adds two leading newlines. One
			// ASCII byte makes the App cap split a rune in either format.
			text := "a" + strings.Repeat("ع", limit/2+10)
			data := text
			if extension == "docx" {
				data = contentDOCX(t, text)
			}
			info := upload(t, th, data, "arabic-limit."+extension, time.Now().Add(-time.Minute))
			run(t, th, nil, model.JobStatusSuccess)
			content := get(t, th, info.Id).Content
			require.Equal(t, limit-1, len(content), "the byte limit must end before the partial Arabic rune")
			require.True(t, utf8.ValidString(content))
			require.True(t, strings.HasPrefix(text, strings.TrimSpace(content)))
		})
	}
}

func contentDOCX(t *testing.T, text string) string {
	t.Helper()
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	for _, part := range []struct{ name, body string }{
		{"[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`},
		{"word/document.xml", `<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:body></w:document>`},
	} {
		file, err := writer.Create(part.name)
		require.NoError(t, err)
		_, err = io.WriteString(file, part.body)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return data.String()
}

func TestRegisteredExtractionRejectsChangedContentSnapshot(t *testing.T) {
	for _, mode := range []string{"replaced", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			th := setup(t)
			created := time.Now().Add(-time.Minute)
			first := upload(t, th, "old output", "first.txt", created)
			tail := upload(t, th, "valid tail", "tail.txt", created.Add(time.Second))
			replacement := upload(t, th, "replacement contents", "replacement.txt", created)
			_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE FileInfo SET DeleteAt=1 WHERE Id=$1`, replacement.Id)
			require.NoError(t, err)
			tx := lockFileContent(t, th, first.Id)
			job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, nil)
			require.Nil(t, appErr)
			waitingContentBackend(t, th)
			if mode == "deleted" {
				_, err = tx.Exec(`UPDATE FileInfo SET DeleteAt=$2 WHERE Id=$1`, first.Id, model.GetMillis())
			} else {
				_, err = tx.Exec(`UPDATE FileInfo SET Path=$2,UpdateAt=UpdateAt+1,Size=$3 WHERE Id=$1`, first.Id, replacement.Path, replacement.Size)
			}
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			require.Eventually(t, func() bool {
				status := currentJob(t, th, job.Id).Status
				return status == model.JobStatusSuccess || status == model.JobStatusError
			}, 10*time.Second, 20*time.Millisecond)
			stored := currentJob(t, th, job.Id)
			assert.Equal(t, model.JobStatusError, stored.Status)
			assert.Equal(t, "2", stored.Data["processed"])
			assert.Equal(t, "1", stored.Data["errors"])
			var content string
			require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT COALESCE(Content,'') FROM FileInfo WHERE Id=$1`, first.Id).Scan(&content))
			assert.Empty(t, content, "a stale extraction must not overwrite a changed or deleted file")
			require.Equal(t, "valid tail", get(t, th, tail.Id).Content)
			if mode == "replaced" {
				run(t, th, nil, model.JobStatusSuccess)
				require.Equal(t, "replacement contents", get(t, th, first.Id).Content)
			}
		})
	}
}

func TestRegisteredExtractionFencesContentBeforeMonitorObservesNewAttempt(t *testing.T) {
	th, proxy := setupSQLProxy(t, "control", 30)
	blocked, tail, job, release := stalledExtraction(t, th, false)
	select {
	case <-proxy.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the actual ownership monitor must be waiting for its dropped SQL reply")
	}
	newToken := model.NewId()
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE Jobs SET Data=Data||jsonb_build_object($2::text,$3::text) WHERE Id=$1`, job.Id, recoveryAttempt, newToken)
	require.NoError(t, err)
	path := filepath.Join(*th.App.Config().FileSettings.Directory, blocked.Path)
	writer, err := os.OpenFile(path, os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = io.WriteString(writer, "stale converter output")
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	release()
	waitNoNativeReader(t, path)
	require.Eventually(t, func() bool {
		var n int
		return th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name='workspace_content_extraction_owner'`).Scan(&n) == nil && n == 0
	}, 2*time.Second, 10*time.Millisecond, "the content fence must reject the old attempt before the monitor's three-second SQL cutoff")
	require.Empty(t, get(t, th, blocked.Id).Content)
	require.Empty(t, get(t, th, tail.Id).Content)
	require.Equal(t, newToken, currentJob(t, th, job.Id).Data[recoveryAttempt])
	proxy.unblock.Store(true)
}

func TestRegisteredExtractionInvalidatesAttachedFileCache(t *testing.T) {
	th := setup(t)
	info := upload(t, th, "attached contents", "attached.txt", time.Now().Add(-time.Minute))
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: th.BasicChannel.Id, FileIds: model.StringArray{info.Id}})
	require.NoError(t, err)
	files, err := th.App.Srv().Store().FileInfo().GetForPost(post.Id, true, false, true)
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Empty(t, files[0].Content)
	run(t, th, nil, model.JobStatusSuccess)
	files, err = th.App.Srv().Store().FileInfo().GetForPost(post.Id, true, false, true)
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "attached contents", files[0].Content)
}

func TestRegisteredExtractionRecoversUnacknowledgedContent(t *testing.T) {
	for _, mode := range []string{"stop", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			th, proxy := setupSQLProxy(t, "owner", 30)
			proxy.dropAt.Store(6) // acquire, BEGIN, claim, COMMIT, range, first file's content
			proxy.responseOnly.Store(true)
			created := time.Now().Add(-time.Minute)
			first := upload(t, th, "first committed", "first.txt", created)
			tail := upload(t, th, "tail after recovery", "tail.txt", created.Add(time.Second))
			th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings.ExtractContent = new(true) })
			job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, model.StringMap{"catchup": "true"})
			require.Nil(t, appErr)
			select {
			case <-proxy.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the actual content UPDATE must reach the response-dropping fixture")
			}
			require.Eventually(t, func() bool { return get(t, th, first.Id).Content == "first committed" }, time.Second, 10*time.Millisecond)
			before := currentJob(t, th, job.Id)
			require.Equal(t, model.JobStatusInProgress, before.Status)
			if mode == "deadline" {
				require.Eventually(t, func() bool { return proxy.blocked() == 0 }, 5*time.Second, 10*time.Millisecond,
					"the owning content statement must close its silent socket without Stop or a fixture release")
			}
			done := make(chan error, 1)
			go func() { done <- th.Server.Jobs.StopWorkers() }()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("StopWorkers did not interrupt the unacknowledged content write")
			}
			require.Eventually(t, func() bool { return proxy.blocked() == 0 }, time.Second, 10*time.Millisecond)
			require.Positive(t, proxy.dropped.Load())
			require.Empty(t, get(t, th, tail.Id).Content, "a lost owner must not start the next file")
			require.Equal(t, model.JobStatusInProgress, currentJob(t, th, job.Id).Status)
			proxy.unblock.Store(true)
			require.NoError(t, th.Server.Jobs.StartWorkers())
			require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == model.JobStatusSuccess }, 10*time.Second, 20*time.Millisecond)
			after := currentJob(t, th, job.Id)
			require.NotEqual(t, before.Data[recoveryAttempt], after.Data[recoveryAttempt])
			require.Equal(t, "2", after.Data["processed"], "the confirmed file and recovered tail both count toward the same job")
			require.Equal(t, "0", after.Data["errors"])
			require.Equal(t, "first committed", get(t, th, first.Id).Content)
			require.Equal(t, "tail after recovery", get(t, th, tail.Id).Content)
		})
	}
}
