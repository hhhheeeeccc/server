package data_retention_job

import (
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestRetentionJobRecoveryFencesOldAttemptsAfterSessionLoss(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.DataRetentionSettings.EnableFileDeletion = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	w := New(th.Server).MakeWorker().(*Worker)
	job, err := th.Store.Job().Save(&model.Job{Id: model.NewId(), Type: model.JobTypeDataRetention,
		CreateAt: model.GetMillis(), Status: model.JobStatusPending})
	require.NoError(t, err)
	first, reason, err := w.acquireExecutionLock(th.Context, make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, continueWork, reason)
	defer first.Close()
	old, err := first.claimJob(job.Id)
	require.NoError(t, err)
	require.NotNil(t, old)
	require.NotEmpty(t, old.Data[retentionAttemptKey], "a nil Job.Data must acquire an attempt atomically")
	// Age is not proof of death: an active session owns this job regardless
	// of its timestamp, so even an old-looking active job must be preserved.
	db := th.Store.GetInternalMasterDB()
	_, err = db.Exec(`UPDATE Jobs SET LastActivityAt=1 WHERE Id=$1`, job.Id)
	require.NoError(t, err)
	require.NoError(t, w.recoverJobs())
	current, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusInProgress, current.Status)
	require.Equal(t, int64(1), current.LastActivityAt)
	var pid int
	require.NoError(t, first.db.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
	var terminated bool
	require.NoError(t, db.QueryRow(`SELECT pg_terminate_backend($1)`, pid).Scan(&terminated))
	require.True(t, terminated)
	require.Eventually(t, func() bool {
		if err := w.recoverJobs(); err != nil {
			return false
		}
		current, err = th.Store.Job().Get(th.Context, job.Id)
		return err == nil && current.Status == model.JobStatusPending
	}, 3*time.Second, 20*time.Millisecond)
	_, err = first.claimJob(job.Id)
	require.Error(t, err, "a failed session cannot claim through a reconnected SQL backend")
	second, reason, err := w.acquireExecutionLock(th.Context, make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, continueWork, reason)
	defer second.Close()
	next, err := second.claimJob(job.Id)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.NotEqual(t, old.Data[retentionAttemptKey], next.Data[retentionAttemptKey])
	require.ErrorIs(t, w.setJobProgress(old, 100), errRetentionAttemptLost)
	w.finishError(th.App.Log(), old, model.NewAppError("old", "app.job.error", nil, "obsolete failure", 500))
	require.Panics(t, func() {
		defer w.handleJobPanic(th.App.Log(), old)
		panic("obsolete panic")
	})
	for _, transition := range [][2]string{
		{model.JobStatusInProgress, model.JobStatusSuccess},
		{model.JobStatusInProgress, model.JobStatusPending},
		{model.JobStatusCancelRequested, model.JobStatusCanceled},
	} {
		changed, err := w.transitionJob(old, transition[0], transition[1])
		require.NoError(t, err)
		require.False(t, changed)
	}
	current, err = th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusInProgress, current.Status)
	require.Equal(t, int64(0), current.Progress)
	require.NotContains(t, current.Data, "error")
	require.Equal(t, next.Data[retentionAttemptKey], current.Data[retentionAttemptKey])
	require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
	w.finishError(th.App.Log(), old, model.NewAppError("old", "app.job.error", nil, "obsolete panic/error", 500))
	current, err = th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusCancelRequested, current.Status, "the obsolete attempt cannot finish a newer cancellation")
	require.False(t, w.finishInterruption(th.App.Log(), next, jobCanceled))
	current, err = th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusCanceled, current.Status)
	newest, err := second.claimJob(job.Id)
	require.NoError(t, err)
	require.Nil(t, newest, "a terminal canceled Job cannot be claimed again")
}

func TestRetentionJobRecoveryHonorsLicensePolicyAndCancellation(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.DataRetentionSettings.EnableMessageDeletion = new(false)
		cfg.DataRetentionSettings.EnableFileDeletion = new(true)
	}).InitBasic(t)
	w := New(th.Server).MakeWorker().(*Worker)
	save := func(status, typ string, data model.StringMap) *model.Job {
		job, err := th.Store.Job().Save(&model.Job{Id: model.NewId(), Type: typ,
			CreateAt: model.GetMillis(), Status: status, Data: data})
		require.NoError(t, err)
		return job
	}
	legacy := save(model.JobStatusInProgress, model.JobTypeDataRetention, model.StringMap{})
	foreign := save(model.JobStatusInProgress, model.JobTypeMessageExport, model.StringMap{retentionAttemptKey: model.NewId()})
	job := save(model.JobStatusPending, model.JobTypeDataRetention, model.StringMap{"preserved": "value"})
	canceled := save(model.JobStatusPending, model.JobTypeDataRetention, nil)
	session, reason, err := w.acquireExecutionLock(th.Context, make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, continueWork, reason)
	defer session.Close()
	for _, candidate := range []*model.Job{job, canceled} {
		claimed, err := session.claimJob(candidate.Id)
		require.NoError(t, err)
		require.NotNil(t, claimed)
	}
	require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, canceled.Id))
	session.Close()
	require.NoError(t, w.recoverJobs(), "recovery must do nothing without a license")
	get := func(job *model.Job) *model.Job {
		current, err := th.Store.Job().Get(th.Context, job.Id)
		require.NoError(t, err)
		return current
	}
	require.Equal(t, model.JobStatusInProgress, get(job).Status)
	require.Equal(t, model.JobStatusCancelRequested, get(canceled).Status)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.DataRetentionSettings.EnableFileDeletion = new(false) })
	require.NoError(t, w.recoverJobs())
	require.Equal(t, model.JobStatusInProgress, get(job).Status)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.DataRetentionSettings.EnableFileDeletion = new(true) })
	require.NoError(t, w.recoverJobs())
	require.Equal(t, model.JobStatusPending, get(job).Status)
	require.Equal(t, "value", get(job).Data["preserved"])
	require.Equal(t, model.JobStatusCanceled, get(canceled).Status)
	require.Equal(t, model.JobStatusInProgress, get(legacy).Status, "legacy claims have no ownership protocol and must not be guessed dead")
	require.Equal(t, model.JobStatusInProgress, get(foreign).Status)
	close(w.stop)
	require.NoError(t, w.recoverJobs(), "shutdown must prevent starting recovery")
}
