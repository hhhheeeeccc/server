package content_extraction_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestRegisteredExtractionResumesManualRangeAfterLostFileCommit(t *testing.T) {
	th, proxy := setupSQLProxy(t, "owner", 30)
	proxy.dropAt.Store(6) // acquire, BEGIN, claim, COMMIT, range, then the first file commit
	proxy.responseOnly.Store(true)
	created := time.Now().Add(-time.Minute)
	first := upload(t, th, "confirmed before lost response", "confirmed.txt", created)
	tail := upload(t, th, "remaining after restart", "tail.txt", created.Add(time.Second))
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, nil)
	require.Nil(t, appErr)
	select {
	case <-proxy.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first file commit must reach the response-dropping SQL proxy")
	}
	require.Eventually(t, func() bool { return get(t, th, first.Id).Content == "confirmed before lost response" }, time.Second, 10*time.Millisecond)
	before := currentJob(t, th, job.Id)
	done := make(chan error, 1)
	go func() { done <- th.Server.Jobs.StopWorkers() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Stop must interrupt the lost response without releasing the proxy")
	}
	require.Eventually(t, func() bool { return proxy.blocked() == 0 }, time.Second, 10*time.Millisecond)
	require.Empty(t, get(t, th, tail.Id).Content)
	// Reopening a confirmed file is an observable error, rather than a
	// mocked extractor count. Its content is already in the real database.
	require.Nil(t, th.App.RemoveFile(first.Path))
	proxy.unblock.Store(true)
	require.NoError(t, th.Server.Jobs.StartWorkers())
	require.Eventually(t, func() bool {
		status := currentJob(t, th, job.Id).Status
		return status == model.JobStatusSuccess || status == model.JobStatusError
	}, 10*time.Second, 20*time.Millisecond)
	after := currentJob(t, th, job.Id)
	require.Equal(t, model.JobStatusSuccess, after.Status, "a manual range must not reopen its confirmed file: %v", after.Data)
	require.NotEqual(t, before.Data[recoveryAttempt], after.Data[recoveryAttempt])
	require.Equal(t, "2", after.Data["processed"], "confirmed counters must survive the lost reply and new attempt")
	require.Equal(t, "0", after.Data["errors"])
	require.Equal(t, "confirmed before lost response", get(t, th, first.Id).Content)
	require.Equal(t, "remaining after restart", get(t, th, tail.Id).Content)
}

func TestRegisteredExtractionResumesConfirmedEmptyFailedAndIgnoredFiles(t *testing.T) {
	for _, revokeLicense := range []bool{false, true} {
		name := "stop"
		if revokeLicense {
			name = "license"
		}
		t.Run(name, func(t *testing.T) { testConfirmedFileOutcomes(t, revokeLicense) })
	}
}

func testConfirmedFileOutcomes(t *testing.T, revokeLicense bool) {
	t.Helper()
	th := setup(t)
	created := time.Now().Add(-2 * time.Minute)
	empty := upload(t, th, "", "empty.txt", created)
	failed := upload(t, th, "missing body", "missing.txt", created.Add(time.Second))
	ignored := upload(t, th, "ignored bytes", "ignored.mp4", created.Add(2*time.Second))
	require.Nil(t, th.App.RemoveFile(failed.Path))
	blocked, tail, job, release := stalledExtraction(t, th, false)
	before := currentJob(t, th, job.Id)
	var checkpoint struct{ ID string }
	require.NoError(t, json.Unmarshal([]byte(before.Data["workspace_extraction_checkpoint_v1"]), &checkpoint))
	require.Equal(t, ignored.Id, checkpoint.ID, "even an ignored outcome must advance the durable cursor")
	require.Equal(t, "2", before.Data["processed"])
	require.Equal(t, "1", before.Data["errors"])
	if revokeLicense {
		th.Server.SetLicense(nil)
		require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == model.JobStatusPending }, 4*time.Second, 20*time.Millisecond)
		paused := currentJob(t, th, job.Id)
		require.Equal(t, before.Data["workspace_extraction_checkpoint_v1"], paused.Data["workspace_extraction_checkpoint_v1"])
		require.Equal(t, "2", paused.Data["processed"])
		require.Equal(t, "1", paused.Data["errors"])
	}
	require.NoError(t, th.Server.Jobs.StopWorkers())
	// Empty content cannot be distinguished from unfinished work by the
	// catchup Content filter. The checkpoint must cover that outcome too.
	require.Nil(t, th.App.RemoveFile(empty.Path))
	_, err := th.Server.FileBackend().WriteFile(strings.NewReader("must not retry a confirmed failure"), failed.Path)
	require.NoError(t, err)
	release()
	path := filepath.Join(*th.App.Config().FileSettings.Directory, blocked.Path)
	waitNoNativeReader(t, path)
	require.NoError(t, os.Remove(path))
	_, err = th.Server.FileBackend().WriteFile(strings.NewReader("unfinished file recovered"), blocked.Path)
	require.NoError(t, err)
	if revokeLicense {
		require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	}
	require.NoError(t, th.Server.Jobs.StartWorkers())
	require.Eventually(t, func() bool {
		status := currentJob(t, th, job.Id).Status
		return status == model.JobStatusSuccess || status == model.JobStatusError
	}, 10*time.Second, 20*time.Millisecond)
	after := currentJob(t, th, job.Id)
	require.Equal(t, model.JobStatusError, after.Status, "the failure from the earlier attempt must remain visible")
	require.Equal(t, "4", after.Data["processed"])
	require.Equal(t, "1", after.Data["errors"], "a confirmed empty file must not be reopened and counted as a new missing-body error")
	require.Empty(t, get(t, th, empty.Id).Content)
	require.Empty(t, get(t, th, failed.Id).Content, "retrying a failed file requires a new job, not recovery of its completed outcome")
	require.Empty(t, get(t, th, ignored.Id).Content)
	require.Equal(t, "unfinished file recovered", get(t, th, blocked.Id).Content)
	require.Equal(t, "must wait", get(t, th, tail.Id).Content)
}

func TestRegisteredExtractionRejectsInconsistentCheckpoint(t *testing.T) {
	th := setup(t)
	info := upload(t, th, "must not be skipped", "scope.txt", time.Now().Add(-time.Minute))
	completed := run(t, th, nil, model.JobStatusSuccess)
	require.NoError(t, th.Server.Jobs.StopWorkers())
	// A copied checkpoint could silently omit the entire range. Corruption
	// must terminate the new job rather than claim success or reopen files.
	require.Nil(t, th.App.RemoveFile(info.Path))
	for _, mode := range []string{"another-job", "damaged", "changed-mode", "inconsistent-count"} {
		t.Run(mode, func(t *testing.T) {
			data := model.StringMap{}
			for key, value := range completed.Data {
				data[key] = value
			}
			job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, data)
			require.Nil(t, appErr)
			var checkpoint map[string]any
			require.NoError(t, json.Unmarshal([]byte(data["workspace_extraction_checkpoint_v1"]), &checkpoint))
			if mode != "another-job" {
				checkpoint["job_id"] = job.Id
			}
			if mode == "changed-mode" {
				checkpoint["catchup"] = true
			}
			encoded, err := json.Marshal(checkpoint)
			require.NoError(t, err)
			data["workspace_extraction_checkpoint_v1"] = string(encoded)
			if mode == "damaged" {
				data["workspace_extraction_checkpoint_v1"] = "{"
			}
			if mode == "inconsistent-count" {
				data["processed"] = "0"
			}
			encoded, err = json.Marshal(data)
			require.NoError(t, err)
			_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE Jobs SET Data=$2::jsonb WHERE Id=$1`, job.Id, string(encoded))
			require.NoError(t, err)
			require.NoError(t, th.Server.Jobs.StartWorkers())
			require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == model.JobStatusError }, 10*time.Second, 20*time.Millisecond)
			require.Contains(t, currentJob(t, th, job.Id).Data["error"], "invalid saved content extraction checkpoint")
			require.Equal(t, "must not be skipped", get(t, th, info.Id).Content)
			require.NoError(t, th.Server.Jobs.StopWorkers())
		})
	}
}
