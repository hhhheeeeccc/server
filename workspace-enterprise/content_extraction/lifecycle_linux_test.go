package content_extraction_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A FIFO keeps the actual native TXT extractor blocked in Read, after the
// storage open has succeeded. No test replaces the worker, extractor or store.
// /proc verifies that the native reader opened the exact isolated fixture.
func stalledExtraction(t *testing.T, th *api4.TestHelper, catchup bool) (*model.FileInfo, *model.FileInfo, *model.Job, func()) {
	t.Helper()
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings.ExtractContentTimeout = new(0) })
	created := time.Now().Add(-time.Minute)
	blocked := upload(t, th, "blocked", "blocked.txt", created)
	tail := upload(t, th, "must wait", "tail.txt", created.Add(time.Second))
	path := filepath.Join(*th.App.Config().FileSettings.Directory, blocked.Path)
	require.NoError(t, os.Remove(path))
	require.NoError(t, syscall.Mkfifo(path, 0600))
	writer, err := os.OpenFile(path, os.O_RDWR, 0600)
	require.NoError(t, err)
	var once sync.Once
	release := func() { once.Do(func() { require.NoError(t, writer.Close()) }) }
	t.Cleanup(release)
	var data model.StringMap
	if catchup {
		th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings.ExtractContent = new(true) })
		data = model.StringMap{"catchup": "true"}
	}
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, data)
	require.Nil(t, appErr)
	require.Eventually(t, func() bool { return openNativeReader(path, writer.Fd()) }, 10*time.Second, 20*time.Millisecond)
	require.Equal(t, model.JobStatusInProgress, currentJob(t, th, job.Id).Status)
	return blocked, tail, job, release
}

func openNativeReader(path string, fixtureFD uintptr) bool {
	entries, _ := os.ReadDir("/proc/self/fd")
	for _, entry := range entries {
		if entry.Name() == strconv.FormatUint(uint64(fixtureFD), 10) {
			continue
		}
		if link, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name())); err == nil && link == path {
			return true
		}
	}
	return false
}

func currentJob(t *testing.T, th *api4.TestHelper, id string) *model.Job {
	t.Helper()
	job, err := th.Store.Job().Get(th.Context, id)
	require.NoError(t, err)
	return job
}

func waitNoNativeReader(t *testing.T, path string) {
	t.Helper()
	require.Eventually(t, func() bool { return !openNativeReader(path, ^uintptr(0)) }, 5*time.Second, 20*time.Millisecond)
}

func TestRegisteredExtractionCancellationInterruptsActiveExtractor(t *testing.T) {
	th := setup(t)
	blocked, tail, job, release := stalledExtraction(t, th, false)
	_, err := th.Client.CancelJob(context.Background(), job.Id)
	require.Error(t, err, "an ordinary user must not cancel an administrative extraction job")
	_, err = th.SystemAdminClient.CancelJob(context.Background(), job.Id)
	require.NoError(t, err)
	assert.Eventually(t, func() bool {
		return currentJob(t, th, job.Id).Status == model.JobStatusCanceled
	}, 4*time.Second, 20*time.Millisecond, "cancellation must release the worker while the actual extractor is waiting for bytes")
	assert.Empty(t, get(t, th, tail.Id).Content, "a canceled job must not extract its remaining files")
	release()
	waitNoNativeReader(t, filepath.Join(*th.App.Config().FileSettings.Directory, blocked.Path))
	// A later real job completes before checking the old job: this proves that
	// the worker has returned from its prior candidate, without a timing sleep.
	run(t, th, model.StringMap{"from": strconv.FormatInt(tail.CreateAt/1000+2, 10)}, model.JobStatusSuccess)
	stored := currentJob(t, th, job.Id)
	assert.Equal(t, model.JobStatusCanceled, stored.Status, "a late native return must not overwrite cancellation with success")
	assert.Equal(t, "0", stored.Data["processed"])
	assert.Equal(t, "0", stored.Data["errors"])
	assert.Empty(t, get(t, th, blocked.Id).Content)
	assert.Empty(t, get(t, th, tail.Id).Content)
}

func TestRegisteredExtractionStopInterruptsAndRestartsWorker(t *testing.T) {
	th := setup(t)
	blocked, tail, job, release := stalledExtraction(t, th, false)
	stopped := make(chan error, 1)
	go func() { stopped <- th.Server.Jobs.StopWorkers() }()
	returned := false
	select {
	case err := <-stopped:
		require.NoError(t, err)
		returned = true
	case <-time.After(4 * time.Second):
		assert.Fail(t, "StopWorkers still waits for the blocked extractor")
	}
	if returned {
		assert.Equal(t, model.JobStatusPending, currentJob(t, th, job.Id).Status)
		assert.Empty(t, get(t, th, tail.Id).Content)
	}
	release()
	if !returned {
		select {
		case err := <-stopped:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("StopWorkers did not finish after releasing the fixture")
		}
	}
	waitNoNativeReader(t, filepath.Join(*th.App.Config().FileSettings.Directory, blocked.Path))
	// Restore normal bytes, then restart the same centrally registered worker.
	require.NoError(t, os.Remove(filepath.Join(*th.App.Config().FileSettings.Directory, blocked.Path)))
	_, err := th.Server.FileBackend().WriteFile(strings.NewReader("restored"), blocked.Path)
	require.NoError(t, err)
	require.NoError(t, th.Server.Jobs.StartWorkers())
	require.Eventually(t, func() bool {
		return currentJob(t, th, job.Id).Status == model.JobStatusSuccess
	}, 10*time.Second, 20*time.Millisecond)
	assert.Equal(t, "restored", get(t, th, blocked.Id).Content)
	assert.Equal(t, "must wait", get(t, th, tail.Id).Content)
}

func TestRegisteredExtractionPausesUntilRuntimeGateReturns(t *testing.T) {
	for _, mode := range []string{"license", "automatic"} {
		t.Run(mode, func(t *testing.T) {
			th := setup(t)
			blocked, tail, job, release := stalledExtraction(t, th, mode == "automatic")
			if mode == "license" {
				th.Server.SetLicense(nil)
			} else {
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings.ExtractContent = new(false) })
			}
			require.Eventually(t, func() bool {
				return currentJob(t, th, job.Id).Status == model.JobStatusPending
			}, 4*time.Second, 20*time.Millisecond)
			paused := currentJob(t, th, job.Id)
			require.Equal(t, "0", paused.Data["processed"])
			require.Equal(t, "0", paused.Data["errors"])
			require.Empty(t, get(t, th, tail.Id).Content)
			// A second candidate is dispatched by the real watcher while the
			// original stays paused, proving it is not claimed and failed again.
			barrierStatus := model.JobStatusSuccess
			if mode == "license" {
				barrierStatus = model.JobStatusError
			}
			run(t, th, model.StringMap{"from": strconv.FormatInt(tail.CreateAt/1000+2, 10)}, barrierStatus)
			require.Equal(t, model.JobStatusPending, currentJob(t, th, job.Id).Status)
			require.Equal(t, paused.StartAt, currentJob(t, th, job.Id).StartAt)
			release()
			path := filepath.Join(*th.App.Config().FileSettings.Directory, blocked.Path)
			waitNoNativeReader(t, path)
			require.NoError(t, os.Remove(path))
			_, err := th.Server.FileBackend().WriteFile(strings.NewReader("resumed"), blocked.Path)
			require.NoError(t, err)
			if mode == "license" {
				require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
			} else {
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings.ExtractContent = new(true) })
			}
			require.Eventually(t, func() bool {
				return currentJob(t, th, job.Id).Status == model.JobStatusSuccess
			}, 10*time.Second, 20*time.Millisecond)
			require.Equal(t, "resumed", get(t, th, blocked.Id).Content)
			require.Equal(t, "must wait", get(t, th, tail.Id).Content)
			require.Equal(t, "2", currentJob(t, th, job.Id).Data["processed"])
		})
	}
}

func TestRegisteredExtractionCancellationInterruptsLockedBatchQuery(t *testing.T) {
	th := setup(t)
	info := upload(t, th, "locked", "locked.txt", time.Now().Add(-time.Minute))
	db := th.Store.GetInternalMasterDB()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`LOCK TABLE FileInfo IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, nil)
	require.Nil(t, appErr)
	require.Eventually(t, func() bool {
		var waiting int
		err := db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE pid<>pg_backend_pid()
			AND datname=current_database() AND wait_event_type='Lock'
			AND query LIKE 'SELECT Id, CreateAt, Name, Extension, MimeType, Path%'`).Scan(&waiting)
		require.NoError(t, err)
		return waiting > 0
	}, 10*time.Second, 20*time.Millisecond)
	_, err = th.SystemAdminClient.CancelJob(context.Background(), job.Id)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return currentJob(t, th, job.Id).Status == model.JobStatusCanceled
	}, 4*time.Second, 20*time.Millisecond, "the job must cancel while the real PostgreSQL lock is still held")
	require.NoError(t, tx.Rollback())
	require.Empty(t, get(t, th, info.Id).Content)
	require.Equal(t, "0", currentJob(t, th, job.Id).Data["processed"])
}

func TestRegisteredExtractionCancellationWinsLateCompletion(t *testing.T) {
	th := setup(t)
	blocked, tail, job, release := stalledExtraction(t, th, false)
	_, err := th.SystemAdminClient.CancelJob(context.Background(), job.Id)
	require.NoError(t, err)
	// Do not wait for the cancellation poll: let the native extractor finish
	// immediately. Its completion must not unconditionally set job success.
	release()
	waitNoNativeReader(t, filepath.Join(*th.App.Config().FileSettings.Directory, blocked.Path))
	run(t, th, model.StringMap{"from": strconv.FormatInt(tail.CreateAt/1000+2, 10)}, model.JobStatusSuccess)
	require.Equal(t, model.JobStatusCanceled, currentJob(t, th, job.Id).Status)
}
