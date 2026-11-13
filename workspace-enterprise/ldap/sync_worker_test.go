package ldap

import (
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestSyncWorkerStoppedBeforeRunExits(t *testing.T) {
	w := &syncWorker{
		jobs: make(chan model.Job), stop: make(chan struct{}), stopped: make(chan struct{}),
	}
	w.Stop()
	go w.Run()
	select {
	case <-w.stopped:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit after stopping before Run")
	}
}

func TestLDAPWorkerTerminalTransitionsDoNotOverwriteCancellation(t *testing.T) {
	th, _ := setupDirectoryAPI(t)
	worker := newSyncWorker(th.App.Ldap().(*Service), func(*model.Config) bool { return true })

	for _, initial := range []string{model.JobStatusInProgress, model.JobStatusCancelRequested, model.JobStatusSuccess} {
		t.Run(initial, func(t *testing.T) {
			job := &model.Job{Id: model.NewId(), Type: model.JobTypeLdapSync,
				CreateAt: model.GetMillis(), Status: initial, Data: model.StringMap{}}
			_, err := th.Store.Job().Save(job)
			require.NoError(t, err)
			if initial == model.JobStatusInProgress {
				require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
			}
			_, err = worker.transitionJob(job, model.JobStatusInProgress, model.JobStatusPending)
			require.NoError(t, err)
			_, err = worker.transitionJob(job, model.JobStatusInProgress, model.JobStatusSuccess)
			require.NoError(t, err)
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			if initial == model.JobStatusSuccess {
				require.Equal(t, model.JobStatusSuccess, stored.Status)
				return
			}
			require.Equal(t, model.JobStatusCancelRequested, stored.Status)
			_, err = worker.transitionJob(job, model.JobStatusCancelRequested, model.JobStatusCanceled)
			require.NoError(t, err)
			stored, err = th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, model.JobStatusCanceled, stored.Status)
		})
	}
}

func TestLDAPWorkerFailurePreservesCancellation(t *testing.T) {
	th, _ := setupDirectoryAPI(t)
	worker := newSyncWorker(th.App.Ldap().(*Service), func(*model.Config) bool { return true })
	jobErr := model.NewAppError("LdapSync", "ent.ldap.sync_failed.app_error", nil, "directory unavailable", 500)
	for _, tc := range []struct {
		name     string
		cancel   bool
		expected string
	}{
		{name: "failure", expected: model.JobStatusError},
		{name: "cancellation", cancel: true, expected: model.JobStatusCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := &model.Job{Id: model.NewId(), Type: model.JobTypeLdapSync,
				CreateAt: model.GetMillis(), Status: model.JobStatusInProgress, Data: model.StringMap{}}
			_, err := th.Store.Job().Save(job)
			require.NoError(t, err)
			if tc.cancel {
				require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
				status, err := worker.readJobStatus(th.Context.Context(), job.Id)
				require.NoError(t, err)
				require.Equal(t, model.JobStatusCancelRequested, status)
			}
			worker.finishError(th.App.Log(), job, jobErr)
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, tc.expected, stored.Status)
			if tc.cancel {
				require.Empty(t, stored.Data["error"], "cancellation must not become a failure")
			} else {
				require.Contains(t, stored.Data["error"], "directory unavailable")
			}
		})
	}
}
