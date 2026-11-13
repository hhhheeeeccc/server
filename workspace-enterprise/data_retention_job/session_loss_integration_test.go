package data_retention_job

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

// Exercise the actual worker SQL while the Posts table is blocked. The failed
// predecessor must exit before the blocked successor can perform any deletion.
func TestRetentionWorkerSessionLossStopsDeletionBeforeSuccessor(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.DataRetentionSettings.EnableMessageDeletion = new(true)
		cfg.DataRetentionSettings.MessageRetentionDays = new(0)
		cfg.DataRetentionSettings.MessageRetentionHours = new(1)
		cfg.DataRetentionSettings.TimeBetweenBatchesMilliseconds = new(0)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec(`UPDATE Posts SET CreateAt=$1 WHERE Id=$2`, time.Now().Add(-48*time.Hour).UnixMilli(), th.BasicPost.Id)
	require.NoError(t, err)
	current := th.CreatePost(t)
	lock, err := db.Begin()
	require.NoError(t, err)
	defer lock.Rollback()
	_, err = lock.Exec(`LOCK TABLE Posts IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	var blockerPID int
	require.NoError(t, lock.QueryRow(`SELECT pg_backend_pid()`).Scan(&blockerPID))

	ctx, cancel := context.WithTimeout(th.Context.Context(), 20*time.Second)
	defer cancel()
	type executionResult struct {
		reason stopReason
		err    error
	}
	start := func() <-chan executionResult {
		worker := New(th.Server).MakeWorker().(*Worker)
		done := make(chan executionResult, 1)
		go func() {
			reason, err := worker.execute(th.Context.WithContext(ctx), th.Context.Logger(), make(chan struct{}))
			done <- executionResult{reason, err}
		}()
		return done
	}
	first := start()
	var second <-chan executionResult
	defer func() {
		_ = lock.Rollback()
		for _, done := range []<-chan executionResult{first, second} {
			if done == nil {
				continue
			}
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("retention execution did not finish after releasing the temporary Posts table lock")
			}
		}
	}()
	lockOwner := func() int {
		var pid int
		err := db.QueryRow(`SELECT pid FROM pg_locks
            WHERE locktype='advisory' AND classid=$1::oid AND objid=$2::oid
              AND objsubid=1 AND granted AND mode='ExclusiveLock'`,
			retentionExecutionLock>>32, retentionExecutionLock&0xffffffff).Scan(&pid)
		if err != nil {
			return 0
		}
		return pid
	}
	blockedDeletes := func() []int64 {
		var pids pq.Int64Array
		err := db.QueryRow(`SELECT COALESCE(array_agg(pid ORDER BY pid), ARRAY[]::integer[])
            FROM pg_stat_activity WHERE datname=current_database() AND state='active'
              AND query LIKE 'DELETE FROM Posts %' AND wait_event_type='Lock'
              AND $1=ANY(pg_blocking_pids(pid))`, blockerPID).Scan(&pids)
		if err != nil {
			return nil
		}
		return pids
	}
	var firstOwner int
	var firstDelete []int64
	require.Eventually(t, func() bool {
		firstOwner, firstDelete = lockOwner(), blockedDeletes()
		return firstOwner > 0 && len(firstDelete) == 1
	}, 3*time.Second, 10*time.Millisecond)
	require.EqualValues(t, firstOwner, firstDelete[0], "execution lock and retention DELETE must use the same backend")
	var terminated bool
	require.NoError(t, db.QueryRow(`SELECT pg_terminate_backend($1)
        WHERE EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND datname=current_database())`, firstOwner).Scan(&terminated))
	require.True(t, terminated)
	require.Eventually(t, func() bool { return lockOwner() == 0 }, time.Second, 10*time.Millisecond)
	select {
	case result := <-first:
		require.Error(t, result.err, "first worker must fail before the Posts lock is released")
		require.Equal(t, continueWork, result.reason)
		first = nil
	case <-time.After(2 * time.Second):
		t.Fatal("first worker kept deleting after its execution session died")
	}
	require.NotContains(t, blockedDeletes(), firstDelete[0])

	second = start()
	require.Eventually(t, func() bool { return len(blockedDeletes()) == 1 && lockOwner() != 0 }, 2*time.Second, 10*time.Millisecond)
	observed := blockedDeletes()
	overlap := len(observed) > 1

	successorOwner := lockOwner()
	t.Logf("execution lock loss: terminated_lock_pid=%d first_delete_pid=%d successor_lock_pid=%d active_delete_pids=%v overlap=%t application_sql_available=true", firstOwner, firstDelete[0], successorOwner, observed, overlap)
	require.NoError(t, lock.Rollback())
	for name, done := range map[string]<-chan executionResult{"second": second} {
		select {
		case result := <-done:
			t.Logf("worker drained: worker=%s reason=%d error=%v", name, result.reason, result.err)
			require.NoError(t, result.err)
			require.Equal(t, continueWork, result.reason)
		case <-time.After(10 * time.Second):
			t.Fatal("retention worker did not finish after releasing Posts")
		}
	}
	// Prevent the deferred cleanup from reading the already drained channels.
	first, second = nil, nil
	var oldCount, currentCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM Posts WHERE Id=$1`, th.BasicPost.Id).Scan(&oldCount))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM Posts WHERE Id=$1`, current.Id).Scan(&currentCount))
	require.Zero(t, oldCount)
	require.Equal(t, 1, currentCount)
	require.False(t, overlap, "execution-lock connection loss must not permit another retention job to start deletion while the first deletion is still running")
}

func TestRetentionExecutionDoesNotReconnectAfterIdleSessionLoss(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
	worker := New(th.Server).MakeWorker().(*Worker)
	execution, reason, err := worker.acquireExecutionLock(th.Context, make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, continueWork, reason)
	defer execution.Close()
	var pid int
	require.NoError(t, execution.db.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
	db := th.Store.GetInternalMasterDB()
	var terminated bool
	require.NoError(t, db.QueryRow(`SELECT pg_terminate_backend($1) WHERE EXISTS
        (SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND datname=current_database())`, pid).Scan(&terminated))
	require.True(t, terminated)
	require.Error(t, execution.checkSession())
	for range 3 {
		_, _, err = execution.postBatch(model.RetentionPolicyBatchConfigs{Limit: 1, GlobalPolicyEndTime: model.GetMillis()}, model.RetentionPolicyCursor{})
		require.ErrorIs(t, err, errRetentionSessionLost)
	}
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM Posts WHERE Id=$1`, th.BasicPost.Id).Scan(&count))
	require.Equal(t, 1, count)
}

func TestRetentionExecutionFailedConnectionClosesPool(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) })
	worker := New(th.Server).MakeWorker().(*Worker)
	dsn, err := pq.ParseURL(worker.databaseDSN)
	require.NoError(t, err)
	worker.databaseDSN = dsn + " dbname='retention_missing_" + model.NewId() + "' connect_timeout=1"
	openers := func() int {
		buffer := make([]byte, 4<<20)
		n := runtime.Stack(buffer, true)
		require.Less(t, n, len(buffer))
		return strings.Count(string(buffer[:n]), "database/sql.(*DB).connectionOpener(")
	}
	before := openers()
	execution, _, err := worker.acquireExecutionLock(th.Context, make(chan struct{}))
	require.Error(t, err)
	require.Nil(t, execution)
	require.Eventually(t, func() bool { return openers() <= before }, time.Second, 10*time.Millisecond,
		"the failed execution must close its database/sql pool instead of losing its handle")
}

func TestRetentionWorkerShutdownInterruptsLockWait(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) })
	db := th.Store.GetInternalMasterDB()
	holder, err := db.Begin()
	require.NoError(t, err)
	defer holder.Rollback()
	_, err = holder.Exec(`SELECT pg_advisory_xact_lock($1)`, retentionExecutionLock)
	require.NoError(t, err)
	worker := New(th.Server).MakeWorker().(*Worker)
	done := make(chan stopReason, 1)
	go func() {
		execution, reason, err := worker.acquireExecutionLock(th.Context, make(chan struct{}))
		if execution != nil {
			execution.Close()
		}
		if err != nil {
			t.Errorf("waiting worker failed: %v", err)
		}
		done <- reason
	}()
	defer func() {
		select {
		case <-worker.stop:
		default:
			close(worker.stop)
		}
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		return db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
            AND query='SELECT pg_try_advisory_lock($1)')`).Scan(&waiting) == nil && waiting
	}, 2*time.Second, 10*time.Millisecond)
	close(worker.stop)
	select {
	case reason := <-done:
		require.Equal(t, workerStopped, reason)
	case <-time.After(2 * time.Second):
		t.Fatal("worker shutdown did not interrupt retention lock waiting")
	}
}
