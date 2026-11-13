package elasticsearch_indexing

import (
	"context"
	"database/sql"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestIndexingStopCancelsLockedBatchRead(t *testing.T) {
	for _, phase := range []string{"posts", "channels", "users", "files"} {
		t.Run(phase, func(t *testing.T) {
			th, endpoint, prefix := setupIndexer(t)
			file, err := th.Store.FileInfo().Save(th.Context, &model.FileInfo{
				CreatorId: th.BasicUser.Id, PostId: th.BasicPost.Id, ChannelId: th.BasicChannel.Id,
				Name: "cancel.txt", Path: "fixture/cancel.txt", Extension: "txt", MimeType: "text/plain", Content: "قراءة قابلة للقطع", Size: 20,
			})
			require.NoError(t, err)
			th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.BatchSize = new(2) })
			w := New(th.Server).MakeWorker().(*checkpointWorker)
			gate := &checkpointReadGate{Store: w.server.Store, phase: phase, entered: make(chan struct{}), release: make(chan struct{})}
			w.server.Store = gate
			var once sync.Once
			release := func() { once.Do(func() { close(gate.release) }) }
			var unlock func()
			t.Cleanup(func() {
				if unlock != nil {
					unlock()
				}
				release()
				w.Stop()
			})
			go w.Run()
			data := model.StringMap{"start_time": "0", "index_posts": "false", "index_channels": "false", "index_users": "false", "index_files": "false"}
			data["index_"+phase] = "true"
			job := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, data)
			sendCheckpointJob(t, w, job)
			select {
			case <-gate.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("worker did not reach the second batch")
			}
			table := map[string]string{"posts": "posts", "channels": "channels", "users": "users", "files": "fileinfo"}[phase]
			db := th.Store.GetInternalMasterDB()
			unlock = lockIndexingTable(t, db, table)
			release()
			pid := waitLockedIndexingRead(t, db, "%FROM "+table+"%", "%ORDER BY%")
			stopped := make(chan struct{})
			go func() { w.Stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("Stop remained blocked inside an actual PostgreSQL batch read")
			}
			assertIndexingReadNoLongerWaiting(t, db, pid)
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, model.JobStatusPending, stored.Status)
			cp, err := loadIndexingCheckpoint(stored)
			require.NoError(t, err)
			counts := map[string]int64{"posts": cp.Progress.DonePostsCount, "channels": cp.Progress.DoneChannelsCount, "users": cp.Progress.DoneUsersCount, "files": cp.Progress.DoneFilesCount}
			require.Positive(t, counts[phase], "the first acknowledged batch must remain durable")
			require.False(t, cp.Progress.IsDone(stored), "the blocked second batch must remain replayable")
			unlock()
			go w.Run()
			sendCheckpointJob(t, w, stored)
			waitCheckpointJob(t, th, stored.Id, model.JobStatusSuccess)
			index, id := phase, th.BasicPost.Id
			switch phase {
			case "posts":
				index += "*"
			case "channels":
				id = th.BasicChannel.Id
			case "users":
				id = th.BasicUser.Id
			case "files":
				id = file.Id
			}
			assertPostIndexed(t, endpoint, prefix+index, id)
		})
	}
}

func TestIndexingStopCancelsLockedTotals(t *testing.T) {
	for _, phase := range []string{"posts", "channels", "users", "files"} {
		t.Run(phase, func(t *testing.T) {
			th, _, _ := setupIndexer(t)
			w := New(th.Server).MakeWorker().(*checkpointWorker)
			db := th.Store.GetInternalMasterDB()
			table := map[string]string{"posts": "posts", "channels": "channels", "users": "users", "files": "file_stats"}[phase]
			unlock := lockIndexingTable(t, db, table)
			go w.Run()
			t.Cleanup(func() { unlock(); w.Stop() })
			data := model.StringMap{"start_time": "0", "index_posts": "false", "index_channels": "false", "index_users": "false", "index_files": "false"}
			data["index_"+phase] = "true"
			job := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, data)
			sendCheckpointJob(t, w, job)
			pid := waitLockedIndexingRead(t, db, "%FROM "+table+"%", "%SELECT%")
			stopped := make(chan struct{})
			go func() { w.Stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("Stop remained blocked in the initial entity count")
			}
			assertIndexingReadNoLongerWaiting(t, db, pid)
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, model.JobStatusPending, stored.Status)
			cp, err := loadIndexingCheckpoint(stored)
			require.NoError(t, err)
			require.Zero(t, cp.Progress.DonePostsCount+cp.Progress.DoneChannelsCount+cp.Progress.DoneUsersCount+cp.Progress.DoneFilesCount)
			require.Zero(t, cp.Progress.LastEntityTime)
			require.False(t, cp.Progress.IsDone(stored))
			unlock()
			go w.Run()
			sendCheckpointJob(t, w, stored)
			waitCheckpointJob(t, th, stored.Id, model.JobStatusSuccess)
		})
	}
}

func TestIndexingStopCancelsLockedMembershipReads(t *testing.T) {
	for _, scenario := range []struct{ name, phase, table string }{
		{"user-channels", "users", "channelmembers"}, {"user-teams", "users", "teammembers"},
		{"private-channel-users", "channels", "channelmembers"}, {"channel-team-users", "channels", "teammembers"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			th, _, _ := setupIndexer(t)
			data := model.StringMap{"start_time": "0", "index_posts": "false", "index_channels": "false", "index_users": "false", "index_files": "false"}
			data["index_"+scenario.phase] = "true"
			if scenario.name == "private-channel-users" {
				at := model.GetMillis() + 1000
				_, err := th.Store.Channel().Save(th.Context, &model.Channel{
					TeamId: th.BasicTeam.Id, Name: model.NewId(), DisplayName: "قناة عضوية خاصة", Type: model.ChannelTypePrivate, CreateAt: at,
				}, 100)
				require.NoError(t, err)
				data["start_time"], data["end_time"] = strconv.FormatInt(at-1, 10), strconv.FormatInt(at+1, 10)
			}
			w := New(th.Server).MakeWorker().(*checkpointWorker)
			db := th.Store.GetInternalMasterDB()
			unlock := lockIndexingTable(t, db, scenario.table)
			go w.Run()
			t.Cleanup(func() { unlock(); w.Stop() })
			job := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, data)
			sendCheckpointJob(t, w, job)
			// Channel team membership uses a comma join; the user batch uses FROM TeamMembers.
			pid := waitLockedIndexingRead(t, db, "%"+scenario.table+"%", "%SELECT%")
			stopped := make(chan struct{})
			go func() { w.Stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("Stop remained blocked in membership SQL used by native Bulk transformations")
			}
			assertIndexingReadNoLongerWaiting(t, db, pid)
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, model.JobStatusPending, stored.Status)
			cp, err := loadIndexingCheckpoint(stored)
			require.NoError(t, err)
			require.Zero(t, cp.Progress.DonePostsCount+cp.Progress.DoneChannelsCount+cp.Progress.DoneUsersCount+cp.Progress.DoneFilesCount)
			require.False(t, cp.Progress.IsDone(stored))
			unlock()
			go w.Run()
			sendCheckpointJob(t, w, stored)
			waitCheckpointJob(t, th, stored.Id, model.JobStatusSuccess)
		})
	}
}

func TestIndexingLockedReadHonorsCancellationAndLicense(t *testing.T) {
	for _, action := range []string{"cancel", "license", "configuration"} {
		t.Run(action, func(t *testing.T) {
			th, _, _ := setupIndexer(t)
			w := New(th.Server).MakeWorker().(*checkpointWorker)
			db := th.Store.GetInternalMasterDB()
			unlock := lockIndexingTable(t, db, "posts")
			go w.Run()
			t.Cleanup(func() { unlock(); w.Stop() })
			job := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, model.StringMap{
				"start_time": "0", "index_posts": "true", "index_channels": "false", "index_users": "false", "index_files": "false",
			})
			sendCheckpointJob(t, w, job)
			pid := waitLockedIndexingRead(t, db, "%FROM posts%", "%SELECT%")
			want, timeout := model.JobStatusPending, 2*time.Second
			switch action {
			case "cancel":
				_, err := th.Store.Job().UpdateStatus(job.Id, model.JobStatusCancelRequested)
				require.NoError(t, err)
				want, timeout = model.JobStatusCanceled, 7*time.Second
			case "license":
				th.Server.SetLicense(nil)
			case "configuration":
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.EnableIndexing = new(false) })
			}
			var stored *model.Job
			require.Eventually(t, func() bool {
				var err error
				stored, err = th.Store.Job().Get(th.Context, job.Id)
				require.NoError(t, err)
				return stored.Status == want
			}, timeout, 20*time.Millisecond)
			assertIndexingReadNoLongerWaiting(t, db, pid)
			cp, err := loadIndexingCheckpoint(stored)
			require.NoError(t, err)
			require.Zero(t, cp.Progress.DonePostsCount)
			require.Zero(t, cp.Progress.LastEntityTime)
			unlock()
			require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
			th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.EnableIndexing = new(true) })
			if action != "cancel" {
				sendCheckpointJob(t, w, stored)
				waitCheckpointJob(t, th, stored.Id, model.JobStatusSuccess)
			}
		})
	}
}

func TestIndexingStopJoinsSilentCancellationPoll(t *testing.T) {
	th, _, _ := setupIndexer(t)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.BatchSize = new(2) })
	w := New(th.Server).MakeWorker().(*checkpointWorker)
	proxy, dsn := newIndexingSQLProxy(t, w.databaseDSN)
	w.databaseDSN = dsn
	gate := &checkpointReadGate{Store: w.server.Store, phase: "posts", entered: make(chan struct{}), release: make(chan struct{})}
	w.server.Store = gate
	var once sync.Once
	release := func() { once.Do(func() { close(gate.release) }) }
	t.Cleanup(func() { proxy.Close(); release(); w.Stop() })
	go w.Run()
	job := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, model.StringMap{
		"start_time": "0", "index_posts": "true", "index_channels": "false", "index_users": "false", "index_files": "false",
	})
	sendCheckpointJob(t, w, job)
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not reach its read gate before cancellation polling")
	}
	// The worker is synchronously gated and has not flushed Bulk. Any traffic
	// on this owning session now comes from its actual cancellation poll.
	proxy.blocked.Store(true)
	require.Eventually(t, func() bool { return proxy.dropped.Load() > 0 }, 6*time.Second, 10*time.Millisecond)
	stopped := make(chan struct{})
	stopSignal := w.stop
	go func() { w.Stop(); close(stopped) }()
	select {
	case <-stopSignal:
	case <-time.After(time.Second):
		t.Fatal("stop was not delivered")
	}
	release()
	select {
	case <-stopped:
	case <-time.After(7 * time.Second):
		t.Fatal("Stop failed to join a cancellation watcher blocked on silent SQL")
	}
	proxy.Close()
	successor := New(th.Server).MakeWorker().(*checkpointWorker)
	require.NoError(t, successor.recoverJobs(make(chan struct{})))
	stored, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusPending, stored.Status, "loss of the poll's owner must leave a recoverable job")
	go successor.Run()
	t.Cleanup(successor.Stop)
	sendCheckpointJob(t, successor, stored)
	waitCheckpointJob(t, th, stored.Id, model.JobStatusSuccess)
}

func TestIndexingOwnerDeadlineEndsBlockedServerUpdateAndReleasesLock(t *testing.T) {
	th, _, _ := setupIndexer(t)
	w := New(th.Server).MakeWorker().(*checkpointWorker)
	job := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, nil)
	execution, claimed := acquireRecoveryClaim(t, w, job)
	execution.timeout = 100 * time.Millisecond
	db := th.Store.GetInternalMasterDB()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.Exec(`UPDATE Jobs SET Priority=Priority WHERE Id=$1`, job.Id)
	require.NoError(t, err)
	completed := make(chan error, 1)
	go func() { _, err := execution.update(claimed, model.JobStatusInProgress); completed <- err }()
	pid := waitLockedIndexingRead(t, db, "%UPDATE Jobs SET Status%", "%UPDATE%")
	select {
	case err := <-completed:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("owning update did not finish at its SQL deadline")
	}
	require.True(t, execution.lost.Load())
	assertIndexingReadNoLongerWaiting(t, db, pid)
	// The row remains locked. Releasing the reader's goroutine alone is not
	// enough: the server must also release this dead session's advisory lock.
	require.Eventually(t, func() bool {
		next, err := newIndexingExecution(w.databaseDSN, time.Second)
		require.NoError(t, err)
		defer next.Close()
		acquired, err := next.acquire(context.Background(), job.Id)
		require.NoError(t, err)
		return acquired
	}, time.Second, 20*time.Millisecond)
}

func lockIndexingTable(t *testing.T, db *sql.DB, table string) func() {
	t.Helper()
	require.Contains(t, []string{"posts", "channels", "users", "fileinfo", "file_stats", "channelmembers", "teammembers"}, table)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	if table == "file_stats" {
		_, err = tx.Exec(`REFRESH MATERIALIZED VIEW file_stats`)
	} else {
		_, err = tx.Exec(`LOCK TABLE ` + table + ` IN ACCESS EXCLUSIVE MODE`)
	}
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	var once sync.Once
	unlock := func() { once.Do(func() { _ = tx.Rollback() }) }
	t.Cleanup(unlock)
	return unlock
}

func waitLockedIndexingRead(t *testing.T, db *sql.DB, from, kind string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		err := db.QueryRow(`SELECT pid FROM pg_stat_activity WHERE datname=current_database()
			AND state='active' AND wait_event_type='Lock' AND pid<>pg_backend_pid()
			AND query ILIKE $1 AND query ILIKE $2 AND query NOT ILIKE '%pg_stat_activity%'
			ORDER BY query_start DESC LIMIT 1`, from, kind).Scan(&pid)
		if err == sql.ErrNoRows {
			return false
		}
		require.NoError(t, err)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "a real indexing query must be blocked in PostgreSQL: %s / %s", from, kind)
	return pid
}

func assertIndexingReadNoLongerWaiting(t *testing.T, db *sql.DB, pid int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var count int
		err := db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE pid=$1 AND state='active' AND wait_event_type='Lock'`, pid).Scan(&count)
		require.NoError(t, err)
		return count == 0
	}, time.Second, 20*time.Millisecond, "Stop must not abandon its SQL reader")
}
