package content_extraction_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

const recoveryAttempt = "workspace_extraction_attempt_v1"

func TestRegisteredExtractionRecoversLostOwningSession(t *testing.T) {
	th := setup(t)
	blocked, tail, job, release := stalledExtraction(t, th, false)
	before := currentJob(t, th, job.Id)
	require.NotEmpty(t, before.Data[recoveryAttempt])
	var pid int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT pid FROM pg_stat_activity
        WHERE datname=current_database() AND application_name='workspace_content_extraction_owner'`).Scan(&pid))
	var terminated bool
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT pg_terminate_backend($1)`, pid).Scan(&terminated))
	require.True(t, terminated)
	require.NoError(t, th.Server.Jobs.StopWorkers())
	// The dead owner cannot reconnect to persist a terminal result without
	// its lock. Its known claim remains available for recovery instead.
	require.Equal(t, model.JobStatusInProgress, currentJob(t, th, job.Id).Status)
	require.Equal(t, before.Data[recoveryAttempt], currentJob(t, th, job.Id).Data[recoveryAttempt])
	release()
	path := filepath.Join(*th.App.Config().FileSettings.Directory, blocked.Path)
	waitNoNativeReader(t, path)
	require.NoError(t, os.Remove(path))
	_, err := th.Server.FileBackend().WriteFile(strings.NewReader("after lost session"), blocked.Path)
	require.NoError(t, err)
	require.NoError(t, th.Server.Jobs.StartWorkers())
	require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == model.JobStatusSuccess }, 10*time.Second, 20*time.Millisecond)
	require.NotEqual(t, before.Data[recoveryAttempt], currentJob(t, th, job.Id).Data[recoveryAttempt])
	require.Equal(t, "after lost session", get(t, th, blocked.Id).Content)
	require.Equal(t, "must wait", get(t, th, tail.Id).Content)
}

func TestRegisteredExtractionDoesNotRecoverUnmarkedClaims(t *testing.T) {
	th := setup(t)
	require.NoError(t, th.Server.Jobs.StopWorkers())
	legacy, err := th.Store.Job().Save(&model.Job{Id: model.NewId(), Type: model.JobTypeExtractContent,
		CreateAt: model.GetMillis() - 1000, StartAt: model.GetMillis() - 1000, Status: model.JobStatusInProgress, Data: model.StringMap{"legacy": "true"}})
	require.NoError(t, err)
	require.NoError(t, th.Server.Jobs.StartWorkers())
	run(t, th, nil, model.JobStatusSuccess)
	after := currentJob(t, th, legacy.Id)
	require.Equal(t, model.JobStatusInProgress, after.Status)
	require.Equal(t, legacy.StartAt, after.StartAt)
	require.Equal(t, legacy.Data, after.Data)
}

func TestRegisteredExtractionOldAttemptCannotFinalizeNewerState(t *testing.T) {
	th := setup(t)
	blocked, tail, job, release := stalledExtraction(t, th, false)
	newToken := model.NewId()
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE Jobs SET Status=$2,
        Data=Data||jsonb_build_object($3::text,$4::text,'newer_state','preserved') WHERE Id=$1`,
		job.Id, model.JobStatusCancelRequested, recoveryAttempt, newToken)
	require.NoError(t, err)
	// Keep the native converter blocked until the real monitor observes the
	// changed claim. A successor candidate proves the old worker has returned.
	run(t, th, model.StringMap{"from": fmt.Sprint(tail.CreateAt/1000 + 2)}, model.JobStatusSuccess)
	release()
	waitNoNativeReader(t, filepath.Join(*th.App.Config().FileSettings.Directory, blocked.Path))
	after := currentJob(t, th, job.Id)
	require.Equal(t, model.JobStatusCancelRequested, after.Status)
	require.Equal(t, newToken, after.Data[recoveryAttempt])
	require.Equal(t, "preserved", after.Data["newer_state"])
	require.Empty(t, get(t, th, tail.Id).Content)
}

func TestRegisteredExtractionRecoveryKeepsFrozenCatchupWindow(t *testing.T) {
	th := setup(t)
	require.NoError(t, th.Server.Jobs.StopWorkers())
	created := time.Now().Add(-48 * time.Hour)
	info := upload(t, th, "older frozen window", "old.txt", created)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings.ExtractContent = new(true) })
	oldToken := model.NewId()
	job, err := th.Store.Job().Save(&model.Job{Id: model.NewId(), Type: model.JobTypeExtractContent, CreateAt: model.GetMillis(),
		Status: model.JobStatusInProgress, Data: model.StringMap{"catchup": "true", recoveryAttempt: oldToken,
			"workspace_extraction_from_v1": fmt.Sprint(info.CreateAt - 1), "workspace_extraction_to_v1": fmt.Sprint(info.CreateAt + 1)}})
	require.NoError(t, err)
	require.NoError(t, th.Server.Jobs.StartWorkers())
	require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == model.JobStatusSuccess }, 10*time.Second, 20*time.Millisecond)
	after := currentJob(t, th, job.Id)
	require.NotEqual(t, oldToken, after.Data[recoveryAttempt])
	require.Equal(t, "1", after.Data["processed"])
	require.Equal(t, "older frozen window", get(t, th, info.Id).Content)
}
