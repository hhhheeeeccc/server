package data_retention_job

import (
	"context"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestRetentionMembershipBatchPreservesRecentMembersAndLimit(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
	schema := "retention_members_" + model.NewId()
	quoted := pq.QuoteIdentifier(schema)
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec("CREATE SCHEMA " + quoted)
	require.NoError(t, err)
	t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + quoted + " CASCADE"); require.NoError(t, err) })
	execution, err := newRetentionExecution(th.Context.Context(), *th.App.Config().SqlSettings.DataSource, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(execution.Close)
	_, err = execution.exec("SET search_path TO " + quoted)
	require.NoError(t, err)
	now := model.GetMillis()
	seedRetentionSQLComparison(t, execution, now)
	_, err = execution.exec(`DELETE FROM ThreadMemberships`)
	require.NoError(t, err)
	for _, id := range []string{"one", "two", "three", "four", "five"} {
		_, err = execution.exec(`INSERT INTO ThreadMemberships VALUES ('global_recent',$1,$2)`, id, now-int64((2*time.Hour)/time.Millisecond))
		require.NoError(t, err)
	}
	_, err = execution.exec(`INSERT INTO ThreadMemberships VALUES ('global_recent','recent',$1)`, now)
	require.NoError(t, err)
	cfg := model.RetentionPolicyBatchConfigs{GlobalPolicyEndTime: now - int64(time.Hour/time.Millisecond), Limit: 2}
	count, _, err := execution.membershipBatch(cfg, model.RetentionPolicyCursor{})
	require.NoError(t, err)
	require.Equal(t, int64(2), count, "one thread with many members must not exceed the row batch limit")
	var recent, expired int
	require.NoError(t, execution.db.QueryRow(`SELECT count(*) FROM ThreadMemberships WHERE UserId='recent'`).Scan(&recent))
	require.Equal(t, 1, recent, "another user's expired membership must not delete a current user's state")
	require.NoError(t, execution.db.QueryRow(`SELECT count(*) FROM ThreadMemberships WHERE UserId<>'recent'`).Scan(&expired))
	require.Equal(t, 3, expired)
	cursor := model.RetentionPolicyCursor{}
	for step := 0; step < 5; step++ {
		count, next, err := execution.membershipBatch(cfg, cursor)
		require.NoError(t, err)
		require.LessOrEqual(t, count, cfg.Limit)
		if retentionCursorDone(next) {
			break
		}
		require.NotEqual(t, 4, step, "bounded deletion must finish")
		cursor = next
	}
	require.NoError(t, execution.db.QueryRow(`SELECT count(*) FROM ThreadMemberships`).Scan(&recent))
	require.Equal(t, 1, recent)
}

func TestRetentionWorkerRemovesMembershipsForDeletedThreads(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.DataRetentionSettings.EnableMessageDeletion = new(true)
		cfg.DataRetentionSettings.MessageRetentionDays = new(0)
		cfg.DataRetentionSettings.MessageRetentionHours = new(1)
		cfg.DataRetentionSettings.BatchSize = new(2)
		cfg.DataRetentionSettings.TimeBetweenBatchesMilliseconds = new(0)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	db := th.Store.GetInternalMasterDB()
	now := model.GetMillis()
	old := now - int64((2*time.Hour)/time.Millisecond)
	expired, current := model.NewId(), model.NewId()
	_, err := db.Exec(`INSERT INTO Threads(PostId,ChannelId,ReplyCount,LastReplyAt,Participants)
		VALUES($1,$3,1,$4,'[]'),($2,$3,1,$5,'[]')`, expired, current, th.BasicChannel.Id, old, now)
	require.NoError(t, err)
	for range 5 {
		_, err = db.Exec(`INSERT INTO ThreadMemberships(PostId,UserId,Following,LastViewed,LastUpdated)
			VALUES($1,$2,TRUE,$3,$3)`, expired, model.NewId(), old)
		require.NoError(t, err)
	}
	_, err = db.Exec(`INSERT INTO ThreadMemberships(PostId,UserId,Following,LastViewed,LastUpdated)
		VALUES($1,$2,TRUE,$3,$3)`, current, th.BasicUser.Id, now)
	require.NoError(t, err)
	worker := New(th.Server).MakeWorker().(*Worker)
	ctx, cancel := context.WithTimeout(th.Context.Context(), 20*time.Second)
	defer cancel()
	reason, err := worker.execute(th.Context.WithContext(ctx), th.Context.Logger(), make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, continueWork, reason)
	var threads, members int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM Threads WHERE PostId=$1`, expired).Scan(&threads))
	require.Zero(t, threads)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM ThreadMemberships WHERE PostId=$1`, expired).Scan(&members))
	require.Zero(t, members, "memberships of a removed thread must not accumulate as unreachable rows")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM ThreadMemberships WHERE PostId=$1`, current).Scan(&members))
	require.Equal(t, 1, members, "a live thread's current membership remains")
}

func newOrphanMembershipFixture(t *testing.T) (*api4.TestHelper, *retentionExecution) {
	t.Helper()
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	schema := "retention_orphans_" + model.NewId()
	quoted := pq.QuoteIdentifier(schema)
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec("CREATE SCHEMA " + quoted)
	require.NoError(t, err)
	t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + quoted + " CASCADE"); require.NoError(t, err) })
	execution, err := newRetentionExecution(th.Context.Context(), *th.App.Config().SqlSettings.DataSource, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(execution.Close)
	_, err = execution.exec("SET search_path TO " + quoted)
	require.NoError(t, err)
	_, err = execution.exec(`CREATE TABLE Threads(PostId text PRIMARY KEY);
		CREATE TABLE ThreadMemberships(PostId text, UserId text);
		INSERT INTO Threads VALUES('live');
		INSERT INTO ThreadMemberships VALUES('live','current');
		INSERT INTO ThreadMemberships SELECT 'orphan',n::text FROM generate_series(1,5) n`)
	require.NoError(t, err)
	return th, execution
}

func TestRetentionOrphanMembershipBatchIsBounded(t *testing.T) {
	_, execution := newOrphanMembershipFixture(t)
	count, err := execution.orphanedMembershipBatch(2)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
	var orphans, current int
	require.NoError(t, execution.db.QueryRow(`SELECT count(*) FROM ThreadMemberships WHERE PostId='orphan'`).Scan(&orphans))
	require.Equal(t, 3, orphans)
	require.NoError(t, execution.db.QueryRow(`SELECT count(*) FROM ThreadMemberships WHERE PostId='live'`).Scan(&current))
	require.Equal(t, 1, current)
}

func TestRetentionOrphanMembershipCleanupHonorsInterruption(t *testing.T) {
	for _, mode := range []string{"cancel", "stop", "license"} {
		t.Run(mode, func(t *testing.T) {
			th, execution := newOrphanMembershipFixture(t)
			worker := New(th.Server).MakeWorker().(*Worker)
			cancelRequested := make(chan struct{})
			const fixtureLock int64 = 6492261473
			_, err := execution.exec(`CREATE FUNCTION block_orphan_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN PERFORM pg_advisory_xact_lock(6492261473); RETURN NULL; END $$;
				CREATE TRIGGER block_cleanup BEFORE DELETE ON ThreadMemberships FOR EACH STATEMENT EXECUTE FUNCTION block_orphan_cleanup()`)
			require.NoError(t, err)
			var pid int
			require.NoError(t, execution.db.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
			db := th.Store.GetInternalMasterDB()
			blocker, err := db.Begin()
			require.NoError(t, err)
			defer blocker.Rollback()
			_, err = blocker.Exec(`SELECT pg_advisory_xact_lock($1)`, fixtureLock)
			require.NoError(t, err)
			type outcome struct {
				reason stopReason
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				reason, err := worker.deleteOrphanedMemberships(cancelRequested, 0, 2, execution)
				done <- outcome{reason, err}
			}()
			require.Eventually(t, func() bool {
				var waiting bool
				return db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, pid).Scan(&waiting) == nil && waiting
			}, 3*time.Second, 10*time.Millisecond, "the first SQL batch must actually be blocked before interruption")
			switch mode {
			case "cancel":
				close(cancelRequested)
			case "stop":
				close(worker.stop)
			case "license":
				th.Server.SetLicense(nil)
				require.Nil(t, th.App.License())
			}
			require.NoError(t, blocker.Rollback())
			select {
			case result := <-done:
				if mode == "license" {
					require.Error(t, result.err)
				} else {
					require.NoError(t, result.err)
					require.Equal(t, map[string]stopReason{"cancel": jobCanceled, "stop": workerStopped}[mode], result.reason)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("orphan cleanup did not finish its in-flight batch")
			}
			var remaining int
			require.NoError(t, execution.db.QueryRow(`SELECT count(*) FROM ThreadMemberships WHERE PostId='orphan'`).Scan(&remaining))
			require.Equal(t, 3, remaining, "interruption must prevent the second batch; the first batch may finish")
		})
	}
}
