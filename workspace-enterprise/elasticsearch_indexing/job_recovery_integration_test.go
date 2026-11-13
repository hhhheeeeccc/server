package elasticsearch_indexing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func saveRecoveryJob(t *testing.T, th *api4.TestHelper, status, typ string, data model.StringMap) *model.Job {
	t.Helper()
	job, err := th.Store.Job().Save(&model.Job{Id: model.NewId(), Type: typ, CreateAt: model.GetMillis(), Status: status, Data: data})
	require.NoError(t, err)
	return job
}
func acquireRecoveryClaim(t *testing.T, w *checkpointWorker, job *model.Job) (*indexingExecution, *model.Job) {
	t.Helper()
	execution, err := newIndexingExecution(w.databaseDSN, w.queryTimeout)
	require.NoError(t, err)
	t.Cleanup(execution.Close)
	acquired, err := execution.acquire(context.Background(), job.Id)
	require.NoError(t, err)
	require.True(t, acquired)
	claimed, err := execution.claim(job.Id)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	return execution, claimed
}

func TestIndexingRecoveryProtectsLiveAndNewerAttempts(t *testing.T) {
	th, _, _ := setupIndexer(t)
	w := New(th.Server).MakeWorker().(*checkpointWorker)
	job := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, nil)
	first, old := acquireRecoveryClaim(t, w, job)
	require.NotEmpty(t, old.Data[indexingAttemptKey])
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec(`UPDATE Jobs SET LastActivityAt=1 WHERE Id=$1`, job.Id)
	require.NoError(t, err)
	require.NoError(t, w.recoverJobs(make(chan struct{})))
	current, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusInProgress, current.Status)
	require.EqualValues(t, 1, current.LastActivityAt, "age alone must not revoke a live lock")
	var pid int
	require.NoError(t, first.db.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
	var killed bool
	require.NoError(t, db.QueryRow(`SELECT pg_terminate_backend($1)`, pid).Scan(&killed))
	require.True(t, killed)
	require.Eventually(t, func() bool {
		if w.recoverJobs(make(chan struct{})) != nil {
			return false
		}
		current, err = th.Store.Job().Get(th.Context, job.Id)
		return err == nil && current.Status == model.JobStatusPending
	}, 3*time.Second, 20*time.Millisecond)
	_, err = first.claim(job.Id)
	require.Error(t, err, "a lost advisory-lock session must not reconnect")
	second, next := acquireRecoveryClaim(t, w, current)
	require.NotEqual(t, old.Data[indexingAttemptKey], next.Data[indexingAttemptKey])
	guard := &checkpointJobStore{JobStore: th.Store.Job(), postgres: true, execution: second, candidate: old}
	old.Progress = 100
	old.Status = model.JobStatusInProgress
	_, err = guard.UpdateOptimistically(old, model.JobStatusInProgress)
	require.ErrorIs(t, err, errIndexingOwnershipLost)
	for _, status := range []string{model.JobStatusSuccess, model.JobStatusPending, model.JobStatusError} {
		_, err = guard.UpdateStatus(old.Id, status)
		require.ErrorIs(t, err, errIndexingOwnershipLost)
	}
	old.Status = model.JobStatusError
	_, err = guard.UpdateOptimistically(old, model.JobStatusInProgress)
	require.ErrorIs(t, err, errIndexingOwnershipLost, "panic/error paths must retain the attempt predicate")
	current, err = th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusInProgress, current.Status)
	require.EqualValues(t, 0, current.Progress)
	require.Equal(t, next.Data[indexingAttemptKey], current.Data[indexingAttemptKey])
	require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
	_, err = guard.UpdateStatus(old.Id, model.JobStatusCanceled)
	require.ErrorIs(t, err, errIndexingOwnershipLost)
	guard.candidate = next
	updated, err := guard.UpdateStatus(next.Id, model.JobStatusCanceled)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusCanceled, updated.Status)
}

func TestIndexingRecoveryHonorsLicenseConfigurationAndLegacyClaims(t *testing.T) {
	th, _, _ := setupIndexer(t)
	w := New(th.Server).MakeWorker().(*checkpointWorker)
	legacy := saveRecoveryJob(t, th, model.JobStatusInProgress, model.JobTypeElasticsearchPostIndexing, model.StringMap{})
	foreign := saveRecoveryJob(t, th, model.JobStatusInProgress, model.JobTypeMessageExport, model.StringMap{indexingAttemptKey: model.NewId()})
	job := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, model.StringMap{"preserved": "value"})
	canceled := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, nil)
	for _, candidate := range []*model.Job{job, canceled} {
		execution, _ := acquireRecoveryClaim(t, w, candidate)
		if candidate.Id == canceled.Id {
			require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, candidate.Id))
		}
		execution.Close()
	}
	get := func(job *model.Job) *model.Job {
		current, err := th.Store.Job().Get(th.Context, job.Id)
		require.NoError(t, err)
		return current
	}
	th.Server.SetLicense(nil)
	require.NoError(t, w.recoverJobs(make(chan struct{})))
	require.Equal(t, model.JobStatusInProgress, get(job).Status)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.EnableIndexing = new(false) })
	require.NoError(t, w.recoverJobs(make(chan struct{})))
	require.Equal(t, model.JobStatusInProgress, get(job).Status)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.EnableIndexing = new(true) })
	stopped := make(chan struct{})
	close(stopped)
	require.ErrorIs(t, w.recoverJobs(stopped), context.Canceled)
	require.Equal(t, model.JobStatusInProgress, get(job).Status)
	require.NoError(t, w.recoverJobs(make(chan struct{})))
	require.Equal(t, model.JobStatusPending, get(job).Status)
	require.Equal(t, "value", get(job).Data["preserved"])
	require.Equal(t, model.JobStatusCanceled, get(canceled).Status)
	require.Equal(t, model.JobStatusInProgress, get(legacy).Status)
	require.Equal(t, model.JobStatusInProgress, get(foreign).Status)
}

func TestIndexingLostSessionCannotFlushBufferedWritesOrFinishNewAttempt(t *testing.T) {
	th, endpoint, _ := setupIndexer(t)
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var bulkRequests atomic.Int32
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_bulk") {
			bulkRequests.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ElasticsearchSettings.ConnectionURL = new(front.URL)
		cfg.ElasticsearchSettings.BatchSize = new(2)
	})
	w := New(th.Server).MakeWorker().(*checkpointWorker)
	gate := &checkpointReadGate{Store: w.server.Store, phase: "posts", entered: make(chan struct{}), release: make(chan struct{})}
	w.server.Store = gate
	var once sync.Once
	unblock := func() { once.Do(func() { close(gate.release) }) }
	go w.Run()
	t.Cleanup(func() { unblock(); w.Stop() })
	job := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, model.StringMap{
		"start_time": "0", "index_posts": "true", "index_channels": "false", "index_users": "false", "index_files": "false",
	})
	sendCheckpointJob(t, w, job)
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not reach second batch")
	}
	var pid int
	require.NoError(t, w.store.execution.db.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
	var killed bool
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT pg_terminate_backend($1)`, pid).Scan(&killed))
	require.True(t, killed)
	successor := New(th.Server).MakeWorker().(*checkpointWorker)
	require.NoError(t, successor.recoverJobs(make(chan struct{})))
	current, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusPending, current.Status)
	second, next := acquireRecoveryClaim(t, successor, current)
	oldDone := make(chan struct{})
	stopSignal := w.stop
	go func() { w.Stop(); close(oldDone) }()
	select {
	case <-stopSignal:
	case <-time.After(5 * time.Second):
		t.Fatal("stop not delivered")
	}
	unblock()
	select {
	case <-oldDone:
	case <-time.After(20 * time.Second):
		t.Fatal("obsolete worker did not stop")
	}
	require.Zero(t, bulkRequests.Load(), "buffered items from the lost execution must never be dispatched")
	current, err = th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusInProgress, current.Status)
	require.Equal(t, next.Data[indexingAttemptKey], current.Data[indexingAttemptKey])
	require.Equal(t, "0", current.Data["done_posts_count"])
	second.Close()
}
