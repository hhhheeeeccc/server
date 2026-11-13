package access_control_job

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestInterruptedABACJobsKeepDistinctStatuses(t *testing.T) {
	th, worker := setupChannelRules(t)
	for _, tc := range []struct {
		name   string
		reason stopReason
		status string
		stops  bool
	}{
		{"worker stop", workerStopped, model.JobStatusPending, true},
		{"administrator cancellation", jobCanceled, model.JobStatusCanceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := &model.Job{
				Id: model.NewId(), Type: model.JobTypeAccessControlSync,
				CreateAt: model.GetMillis(), Status: model.JobStatusInProgress,
				Data: model.StringMap{},
			}
			_, err := th.Store.Job().Save(job)
			require.NoError(t, err)
			if tc.reason == jobCanceled {
				require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
			}
			require.Equal(t, tc.stops, worker.finishInterruption(th.App.Log(), job, tc.reason))
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, tc.status, stored.Status)
		})
	}
}

func TestABACWorkerStopReturnsClaimedJobToPending(t *testing.T) {
	th, worker := setupChannelRules(t)
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeAccessControlSync, nil)
	require.Nil(t, appErr)
	worker.stop <- true
	require.True(t, worker.doJob(job))
	stored, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusPending, stored.Status)
	require.False(t, worker.doJob(stored), "the returned job must be claimable after restart")
	stored, err = th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusSuccess, stored.Status)
}

func TestABACCancellationAndWorkerStopKeepCancellation(t *testing.T) {
	for _, cancelFirst := range []bool{true, false} {
		name := "stop then cancel"
		if cancelFirst {
			name = "cancel then stop"
		}
		t.Run(name, func(t *testing.T) {
			th, worker := setupChannelRules(t)
			job := &model.Job{
				Id: model.NewId(), Type: model.JobTypeAccessControlSync,
				CreateAt: model.GetMillis(), Status: model.JobStatusInProgress,
				Data: model.StringMap{},
			}
			_, err := th.Store.Job().Save(job)
			require.NoError(t, err)
			if cancelFirst {
				require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
			}
			require.True(t, worker.finishInterruption(th.App.Log(), job, workerStopped))
			if !cancelFirst {
				require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
			}
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, model.JobStatusCanceled, stored.Status)
		})
	}
}

func TestABACCancellationAfterProgressPreventsSuccess(t *testing.T) {
	th, worker := setupChannelRules(t)
	for _, cancelBeforeSuccess := range []bool{true, false} {
		name := "success first"
		if cancelBeforeSuccess {
			name = "cancellation first"
		}
		t.Run(name, func(t *testing.T) {
			job := &model.Job{Id: model.NewId(), Type: model.JobTypeAccessControlSync,
				CreateAt: model.GetMillis(), Status: model.JobStatusInProgress, Data: model.StringMap{}}
			_, err := th.Store.Job().Save(job)
			require.NoError(t, err)
			require.Nil(t, th.Server.Jobs.SetJobProgress(job, 100))
			if cancelBeforeSuccess {
				require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
			}
			updated, err := worker.finishJobStatus(job, model.JobStatusInProgress, model.JobStatusSuccess)
			require.NoError(t, err)
			require.Equal(t, !cancelBeforeSuccess, updated)
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			if cancelBeforeSuccess {
				require.Equal(t, model.JobStatusCancelRequested, stored.Status)
				require.False(t, worker.finishInterruption(th.App.Log(), job, jobCanceled))
				stored, err = th.Store.Job().Get(th.Context, job.Id)
				require.NoError(t, err)
				require.Equal(t, model.JobStatusCanceled, stored.Status)
			} else {
				require.Equal(t, model.JobStatusSuccess, stored.Status)
			}
		})
	}
}
