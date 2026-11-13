package data_retention_job

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

type retentionSQLProxy struct {
	listener  net.Listener
	target    string
	blocked   atomic.Bool
	dropped   atomic.Int64
	mu        sync.Mutex
	links     map[net.Conn]net.Conn
	closed    bool
	workers   sync.WaitGroup
	finished  chan struct{}
	closeOnce sync.Once
}

func newRetentionSQLProxy(t *testing.T, dsn string) (*retentionSQLProxy, string) {
	t.Helper()
	address, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, address.Scheme)
	require.Contains(t, []string{"127.0.0.1", "localhost"}, address.Hostname(), "only isolated loopback PostgreSQL may be proxied")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &retentionSQLProxy{listener: listener, target: address.Host, links: make(map[net.Conn]net.Conn), finished: make(chan struct{})}
	go func() {
		defer close(p.finished)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			p.workers.Add(1)
			go p.relay(client)
		}
	}()
	t.Cleanup(p.Close)
	address.Host = listener.Addr().String()
	query := address.Query()
	query.Set("connect_timeout", "0")
	address.RawQuery = query.Encode()
	return p, address.String()
}

func (p *retentionSQLProxy) relay(client net.Conn) {
	defer p.workers.Done()
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.links[client] = upstream
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.links, client); p.mu.Unlock() }()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(retentionSQLProxyWriter{p: p, target: upstream}, client)
		_ = upstream.Close()
		_ = client.Close()
		close(done)
	}()
	_, _ = io.Copy(retentionSQLProxyWriter{p: p, target: client}, upstream)
	_ = upstream.Close()
	_ = client.Close()
	<-done
}

func (p *retentionSQLProxy) Close() {
	p.closeOnce.Do(func() {
		_ = p.listener.Close()
		<-p.finished
		p.mu.Lock()
		p.closed = true
		for client, upstream := range p.links {
			_ = client.Close()
			_ = upstream.Close()
		}
		p.mu.Unlock()
		p.workers.Wait()
	})
}

type retentionSQLProxyWriter struct {
	p      *retentionSQLProxy
	target io.Writer
}

func (w retentionSQLProxyWriter) Write(data []byte) (int, error) {
	if w.p.blocked.Load() {
		w.p.dropped.Add(int64(len(data)))
		return len(data), nil
	}
	return w.target.Write(data)
}

func retentionDeadlineServer(t *testing.T) *api4.TestHelper {
	t.Helper()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.DataRetentionSettings.EnableFileDeletion = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	return th
}

func TestRetentionExecutionBoundsSilentQueryAndReleasesLock(t *testing.T) {
	th := retentionDeadlineServer(t)
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			w := New(th.Server).MakeWorker().(*Worker)
			proxy, dsn := newRetentionSQLProxy(t, w.databaseDSN)
			ctx, cancel := context.WithCancel(th.Context.Context())
			defer cancel()
			oldWorker := New(th.Server).MakeWorker().(*Worker)
			oldWorker.databaseDSN, oldWorker.queryTimeout = dsn, 5*time.Second
			execution, reason, err := oldWorker.acquireExecutionLock(th.Context.WithContext(ctx), make(chan struct{}))
			require.NoError(t, err)
			require.Equal(t, continueWork, reason)
			defer func() { proxy.Close(); execution.Close() }()
			job, err := th.Store.Job().Save(&model.Job{Id: model.NewId(), Type: model.JobTypeDataRetention,
				CreateAt: model.GetMillis(), Status: model.JobStatusPending})
			require.NoError(t, err)
			claimed, err := execution.claimJob(job.Id)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			if mode == "deadline" {
				execution.timeout = 150 * time.Millisecond
			}
			proxy.blocked.Store(true)
			finished := make(chan error, 1)
			go func() { _, err := execution.exec("SELECT 1"); finished <- err }()
			require.Eventually(t, func() bool { return proxy.dropped.Load() > 0 }, time.Second, time.Millisecond)
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-finished:
				require.Error(t, err)
			case <-time.After(time.Second):
				t.Fatal("retention query remained blocked while SQL and its cancellation link were silent")
			}
			_, err = execution.exec("SELECT 1")
			require.Error(t, err, "a lost lock owner must not reconnect")
			// SQL and the native application pool still work. A successor must
			// obtain the real lock before recovering and reclaiming this Job.
			require.NoError(t, th.Store.GetInternalMasterDB().Ping())
			require.Eventually(t, func() bool {
				if err := w.recoverJobs(); err != nil {
					return false
				}
				current, err := th.Store.Job().Get(th.Context, job.Id)
				return err == nil && current.Status == model.JobStatusPending
			}, 3*time.Second, 20*time.Millisecond)
			successor, reason, err := w.acquireExecutionLock(th.Context, make(chan struct{}))
			require.NoError(t, err)
			require.Equal(t, continueWork, reason)
			defer successor.Close()
			next, err := successor.claimJob(job.Id)
			require.NoError(t, err)
			require.NotNil(t, next)
			require.NotEqual(t, claimed.Data[retentionAttemptKey], next.Data[retentionAttemptKey])
			require.ErrorIs(t, w.setJobProgress(claimed, 100), errRetentionAttemptLost)
		})
	}
}

func TestRetentionLockAcquisitionStopsDuringSilentStartup(t *testing.T) {
	th := retentionDeadlineServer(t)
	for _, mode := range []string{"stop", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			dsn, entered, cleanup := silentRetentionDatabase(t)
			w := New(th.Server).MakeWorker().(*Worker)
			w.databaseDSN, w.queryTimeout = dsn, 10*time.Second
			cancelRequested := make(chan struct{})
			finished := make(chan stopReason, 1)
			go func() {
				execution, reason, err := w.acquireExecutionLock(th.Context, cancelRequested)
				if execution != nil {
					execution.Close()
				}
				if err != nil {
					t.Errorf("interrupted lock acquisition: %v", err)
				}
				finished <- reason
			}()
			defer cleanup()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("lock acquisition did not reach the silent PostgreSQL fixture")
			}
			want := workerStopped
			if mode == "stop" {
				close(w.stop)
			} else {
				close(cancelRequested)
				want = jobCanceled
			}
			select {
			case reason := <-finished:
				require.Equal(t, want, reason)
			case <-time.After(time.Second):
				t.Fatal("lock acquisition waited for its 10-second SQL deadline after interruption")
			}
		})
	}
}

func TestRetentionRecoveryStopsDuringSilentStartup(t *testing.T) {
	th := retentionDeadlineServer(t)
	dsn, entered, cleanup := silentRetentionDatabase(t)
	w := New(th.Server).MakeWorker().(*Worker)
	w.databaseDSN, w.queryTimeout = dsn, 10*time.Second
	go w.Run()
	defer func() { cleanup(); w.Stop() }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker recovery did not reach silent PostgreSQL startup")
	}
	finished := make(chan struct{})
	go func() { w.Stop(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("worker Stop remained blocked in recovery startup")
	}
	require.NoError(t, th.Store.GetInternalMasterDB().Ping())
}

func TestRetentionJobMetadataUpdateBoundsSilentStartup(t *testing.T) {
	th := retentionDeadlineServer(t)
	w := New(th.Server).MakeWorker().(*Worker)
	job, err := th.Store.Job().Save(&model.Job{Id: model.NewId(), Type: model.JobTypeDataRetention,
		CreateAt: model.GetMillis(), Status: model.JobStatusInProgress,
		Data: model.StringMap{retentionAttemptKey: model.NewId()}})
	require.NoError(t, err)
	dsn, entered, cleanup := silentRetentionDatabase(t)
	w.databaseDSN, w.queryTimeout = dsn, 150*time.Millisecond
	finished := make(chan error, 1)
	go func() { finished <- w.setJobProgress(job, 100) }()
	defer cleanup()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("metadata update did not reach silent PostgreSQL startup")
	}
	select {
	case err := <-finished:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("attempt metadata update exceeded its SQL budget")
	}
	current, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, int64(0), current.Progress)
	require.Equal(t, model.JobStatusInProgress, current.Status)
}

func TestRetentionSilentDeleteReleasesLockWithoutLateDeletion(t *testing.T) {
	th := retentionDeadlineServer(t)
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec(`UPDATE Posts SET CreateAt=$1 WHERE Id=$2`, time.Now().Add(-48*time.Hour).UnixMilli(), th.BasicPost.Id)
	require.NoError(t, err)
	w := New(th.Server).MakeWorker().(*Worker)
	proxy, dsn := newRetentionSQLProxy(t, w.databaseDSN)
	previous := New(th.Server).MakeWorker().(*Worker)
	previous.databaseDSN, previous.queryTimeout = dsn, 6*time.Second
	execution, reason, err := previous.acquireExecutionLock(th.Context, make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, continueWork, reason)
	defer func() { proxy.Close(); execution.Close() }()
	var ownerPID int
	require.NoError(t, execution.db.QueryRow(`SELECT pg_backend_pid()`).Scan(&ownerPID))
	blocker, err := db.Begin()
	require.NoError(t, err)
	defer blocker.Rollback()
	_, err = blocker.Exec(`LOCK TABLE Posts IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	execution.timeout = 2 * time.Second
	batch := model.RetentionPolicyBatchConfigs{Limit: 1, GlobalPolicyEndTime: time.Now().Add(-time.Hour).UnixMilli()}
	finished := make(chan error, 1)
	go func() { _, _, err := execution.postBatch(batch, model.RetentionPolicyCursor{}); finished <- err }()
	require.Eventually(t, func() bool {
		var blocked bool
		err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1
            AND query LIKE 'DELETE FROM Posts %' AND wait_event_type='Lock')`, ownerPID).Scan(&blocked)
		return err == nil && blocked
	}, time.Second, time.Millisecond)
	// The real DELETE is now blocked on PostgreSQL, not on a mock function.
	// Drop its response and the separate cancel link without closing either.
	proxy.blocked.Store(true)
	select {
	case err := <-finished:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("retention DELETE did not return before its six-second server statement timeout")
	}
	require.Eventually(t, func() bool {
		var present bool
		err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)`, ownerPID).Scan(&present)
		return err == nil && !present
	}, 2*time.Second, 10*time.Millisecond, "the backend must abandon its real DELETE and release the advisory lock while Posts remains blocked")
	successor, reason, err := w.acquireExecutionLock(th.Context, make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, continueWork, reason)
	defer successor.Close()
	require.NoError(t, blocker.Rollback())
	var present int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM Posts WHERE Id=$1`, th.BasicPost.Id).Scan(&present))
	require.Equal(t, 1, present, "closing the old connection must roll back its deletion")
	_, _, err = execution.postBatch(batch, model.RetentionPolicyCursor{})
	require.ErrorIs(t, err, errRetentionSessionLost)
	cursor := model.RetentionPolicyCursor{}
	for range 10 {
		_, next, err := successor.postBatch(batch, cursor)
		require.NoError(t, err)
		cursor = next
		if retentionCursorDone(next) {
			break
		}
	}
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM Posts WHERE Id=$1`, th.BasicPost.Id).Scan(&present))
	require.Zero(t, present, "only a fresh lock owner may finish deleting the expired post")
}

func TestRetentionExecutionClosesSilentCancellationSocket(t *testing.T) {
	th := retentionDeadlineServer(t)
	w := New(th.Server).MakeWorker().(*Worker)
	proxy, dsn := newRetentionSQLProxy(t, w.databaseDSN)
	execution, err := newRetentionExecution(th.Context.Context(), dsn, 5*time.Second)
	require.NoError(t, err)
	defer func() { proxy.Close(); execution.Close() }()
	require.NoError(t, execution.checkSession())
	execution.timeout = time.Second
	queryCtx, stopQuery := execution.queryContext()
	defer stopQuery()
	// Expire the driver's request before the enclosing session budget. This
	// forces pq to open its distinct cancellation socket while it is still
	// allowed to dial, instead of racing cancellation against our socket close.
	statementCtx, cancelStatement := context.WithCancel(queryCtx)
	defer cancelStatement()
	proxy.blocked.Store(true)
	finished := make(chan error, 1)
	go func() { _, err := execution.db.ExecContext(statementCtx, "SELECT 1"); finished <- err }()
	require.Eventually(t, func() bool { return proxy.dropped.Load() > 0 }, 300*time.Millisecond, time.Millisecond)
	beforeCancel := proxy.dropped.Load()
	cancelStatement()
	require.Eventually(t, func() bool { return proxy.dropped.Load() > beforeCancel }, 300*time.Millisecond, time.Millisecond,
		"the driver's real cancellation request must reach the silent proxy")
	select {
	case err := <-finished:
		require.Error(t, err)
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("the driver's silent cancellation socket survived the enclosing SQL budget")
	}
	_, err = execution.exec("SELECT 1")
	require.ErrorIs(t, err, errRetentionSessionLost)
}

func TestRetentionHealthySessionReusesBackend(t *testing.T) {
	th := retentionDeadlineServer(t)
	source := New(th.Server).MakeWorker().(*Worker).databaseDSN
	u, err := url.Parse(source)
	require.NoError(t, err)
	query := u.Query()
	query.Set("application_name", "workspace_retention_custom_name")
	u.RawQuery = query.Encode()
	source = u.String()
	password, _ := u.User.Password()
	keyword := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable application_name=workspace_retention_custom_name",
		u.Hostname(), u.Port(), u.User.Username(), password, u.Path[1:])
	for _, config := range []struct{ name, dsn string }{{"url", source}, {"keyword", keyword}} {
		t.Run(config.name, func(t *testing.T) {
			execution, err := newRetentionExecution(th.Context.Context(), config.dsn, 2*time.Second)
			require.NoError(t, err)
			defer execution.Close()
			first := 0
			for range 3 {
				ctx, cancel := execution.queryContext()
				var pid int
				var name, timeout string
				err := execution.db.QueryRowContext(ctx, `SELECT pg_backend_pid(),
                    current_setting('application_name'),current_setting('statement_timeout')`).Scan(&pid, &name, &timeout)
				cancel()
				cancel() // context.CancelFunc cleanup must be idempotent.
				require.NoError(t, err)
				require.Equal(t, "workspace_retention_custom_name", name)
				require.Equal(t, "2s", timeout)
				if first == 0 {
					first = pid
				}
				require.Equal(t, first, pid, "normal cleanup must keep the healthy lock-capable backend")
			}
		})
	}
}
