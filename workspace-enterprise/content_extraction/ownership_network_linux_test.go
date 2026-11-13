package content_extraction_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisteredExtractionRecoversUnacknowledgedWrites(t *testing.T) {
	for _, stage := range []struct {
		name  string
		query int64
	}{{"claim", 4}, {"window", 5}} {
		t.Run(stage.name, func(t *testing.T) {
			th, proxy := setupSQLProxy(t, "owner", 30)
			proxy.dropAt.Store(stage.query)
			proxy.responseOnly.Store(true)
			info := upload(t, th, "after unknown write outcome", "unknown.txt", time.Now().Add(-time.Minute))
			job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, nil)
			require.Nil(t, appErr)
			select {
			case <-proxy.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("no owning mutation reached the response-dropping fixture")
			}
			require.Eventually(t, func() bool {
				stored := currentJob(t, th, job.Id)
				return stored.Status == model.JobStatusInProgress && (stage.name != "window" || stored.Data["workspace_extraction_from_v1"] != "")
			}, time.Second, 10*time.Millisecond,
				"the real mutation must commit despite its response being dropped")
			before := currentJob(t, th, job.Id)
			require.NotEmpty(t, before.Data[recoveryAttempt])
			if stage.name == "window" {
				require.NotEmpty(t, before.Data["workspace_extraction_from_v1"])
				done := make(chan error, 1)
				go func() { done <- th.Server.Jobs.StopWorkers() }()
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(2 * time.Second):
					t.Fatal("StopWorkers did not interrupt an unacknowledged progress write")
				}
			}
			require.Eventually(t, func() bool { return proxy.blocked() == 0 }, 5*time.Second, 20*time.Millisecond)
			require.Positive(t, proxy.dropped.Load())
			require.Empty(t, get(t, th, info.Id).Content)
			require.Equal(t, model.JobStatusInProgress, currentJob(t, th, job.Id).Status)
			proxy.unblock.Store(true)
			if stage.name == "window" {
				require.NoError(t, th.Server.Jobs.StartWorkers())
			}
			// The claim case keeps the worker running: its 30-second recovery
			// sweep must repair the unknown commit without a manual requeue.
			require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == model.JobStatusSuccess }, 40*time.Second, 20*time.Millisecond)
			after := currentJob(t, th, job.Id)
			require.NotEqual(t, before.Data[recoveryAttempt], after.Data[recoveryAttempt])
			require.Equal(t, "1", after.Data["processed"])
			require.Equal(t, "after unknown write outcome", get(t, th, info.Id).Content)
		})
	}
}

func TestRegisteredExtractionKeepsUnacknowledgedTerminalCommit(t *testing.T) {
	th, proxy := setupSQLProxy(t, "owner", 30)
	proxy.dropAt.Store(7) // acquire, BEGIN, claim, COMMIT, range, content, then terminal status
	proxy.responseOnly.Store(true)
	info := upload(t, th, "committed once", "terminal.txt", time.Now().Add(-time.Minute))
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, nil)
	require.Nil(t, appErr)
	select {
	case <-proxy.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no terminal write reached the response-dropping fixture")
	}
	require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == model.JobStatusSuccess }, time.Second, 10*time.Millisecond)
	before := currentJob(t, th, job.Id)
	done := make(chan error, 1)
	go func() { done <- th.Server.Jobs.StopWorkers() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(4 * time.Second):
		t.Fatal("terminal SQL exceeded its independent finalization deadline")
	}
	require.Eventually(t, func() bool { return proxy.blocked() == 0 }, time.Second, 20*time.Millisecond)
	require.Positive(t, proxy.dropped.Load())
	require.Equal(t, "committed once", get(t, th, info.Id).Content)
	proxy.unblock.Store(true)
	require.NoError(t, th.Server.Jobs.StartWorkers())
	run(t, th, model.StringMap{"from": fmt.Sprint(time.Now().Add(time.Hour).Unix()), "to": fmt.Sprint(time.Now().Add(2 * time.Hour).Unix())}, model.JobStatusSuccess)
	after := currentJob(t, th, job.Id)
	require.Equal(t, model.JobStatusSuccess, after.Status)
	require.Equal(t, before.StartAt, after.StartAt)
	require.Equal(t, before.Data[recoveryAttempt], after.Data[recoveryAttempt])
	require.Equal(t, "1", after.Data["processed"])
}

func TestRegisteredExtractionStopInterruptsLockedClaim(t *testing.T) {
	th := setup(t)
	require.NoError(t, th.Server.Jobs.StopWorkers())
	info := upload(t, th, "after claim lock", "locked-claim.txt", time.Now().Add(-time.Minute))
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, nil)
	require.Nil(t, appErr)
	db := th.Store.GetInternalMasterDB()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	var id string
	require.NoError(t, tx.QueryRow(`SELECT Id FROM Jobs WHERE Id=$1 FOR UPDATE`, job.Id).Scan(&id))
	require.NoError(t, th.Server.Jobs.StartWorkers())
	require.Eventually(t, func() bool {
		var n int
		err := db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()
            AND application_name='workspace_content_extraction_owner' AND wait_event_type='Lock' AND query LIKE 'UPDATE Jobs%'`).Scan(&n)
		return err == nil && n == 1
	}, 5*time.Second, 10*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- th.Server.Jobs.StopWorkers() }()
	returned := false
	select {
	case err := <-done:
		require.NoError(t, err)
		returned = true
	case <-time.After(2 * time.Second):
		assert.Fail(t, "StopWorkers waited for its locked job claim")
	}
	require.NoError(t, tx.Rollback())
	if !returned {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("claim did not exit after unlocking its fixture")
		}
	}
	require.Equal(t, model.JobStatusPending, currentJob(t, th, job.Id).Status)
	require.Empty(t, get(t, th, info.Id).Content)
	require.NoError(t, th.Server.Jobs.StartWorkers())
	require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == model.JobStatusSuccess }, 10*time.Second, 20*time.Millisecond)
	require.Equal(t, "after claim lock", get(t, th, info.Id).Content)
}
