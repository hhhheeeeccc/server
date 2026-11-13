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

func completionFixture(t *testing.T) (*api4.TestHelper, *Worker, *retentionExecution, *model.FileInfo) {
	t.Helper()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.DataRetentionSettings.EnableMessageDeletion = new(false)
		cfg.DataRetentionSettings.EnableFileDeletion = new(true)
		cfg.DataRetentionSettings.FileRetentionDays = new(0)
		cfg.DataRetentionSettings.FileRetentionHours = new(1)
		cfg.DataRetentionSettings.TimeBetweenBatchesMilliseconds = new(0)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	file, appErr := th.App.DoUploadFile(th.Context, time.Now().Add(-48*time.Hour), th.BasicTeam.Id, th.BasicChannel.Id, th.BasicUser.Id, "expired.txt", []byte("expired"), false)
	require.Nil(t, appErr)
	worker := New(th.Server).MakeWorker().(*Worker)
	execution, reason, err := worker.acquireExecutionLock(th.Context, make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, continueWork, reason)
	t.Cleanup(execution.Close)
	require.Empty(t, th.App.RemoveFilesFromFileStore(th.Context, []*model.FileInfo{file}))
	var pid int
	require.NoError(t, execution.db.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
	var killed bool
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT pg_terminate_backend($1) WHERE EXISTS
        (SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND datname=current_database())`, pid).Scan(&killed))
	require.True(t, killed)
	dsn, err := pq.ParseURL(worker.databaseDSN)
	require.NoError(t, err)
	worker.databaseDSN = dsn + " application_name='retention_completion_" + file.Id + "'"
	return th, worker, execution, file
}

func TestRetentionFileCompletionOwnsFreshLockOnMetadataBackend(t *testing.T) {
	th, worker, execution, file := completionFixture(t)
	db := th.Store.GetInternalMasterDB()
	holder, err := db.Begin()
	require.NoError(t, err)
	defer holder.Rollback()
	_, err = holder.Exec(`SELECT pg_advisory_xact_lock($1)`, retentionExecutionLock)
	require.NoError(t, err)
	blocker, err := db.Begin()
	require.NoError(t, err)
	defer blocker.Rollback()
	_, err = blocker.Exec(`LOCK TABLE FileInfo IN SHARE MODE`)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- worker.completeDeletedFile(execution, file) }()
	defer func() {
		_ = holder.Rollback()
		_ = blocker.Rollback()
		if done != nil {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("completion did not drain")
			}
		}
	}()
	name := "retention_completion_" + file.Id
	require.Eventually(t, func() bool {
		var waiting bool
		return db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
            AND application_name=$1 AND query='SELECT pg_try_advisory_lock($1)')`, name).Scan(&waiting) == nil && waiting
	}, 2*time.Second, 10*time.Millisecond)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM FileInfo WHERE Id=$1`, file.Id).Scan(&count))
	require.Equal(t, 1, count, "completion must not delete metadata while a successor owns the retention lock")
	select {
	case err := <-done:
		done = nil
		t.Fatalf("completion skipped the successor lock: %v", err)
	default:
	}
	require.NoError(t, holder.Rollback())
	var metadataPID, lockPID int
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT a.pid,l.pid FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid
            WHERE a.datname=current_database() AND a.application_name=$1
              AND a.query LIKE 'DELETE FROM FileInfo %' AND a.state='active' AND a.wait_event_type='Lock'
              AND l.locktype='advisory' AND l.classid=$2::oid AND l.objid=$3::oid AND l.objsubid=1 AND l.granted`,
			name, retentionExecutionLock>>32, retentionExecutionLock&0xffffffff).Scan(&metadataPID, &lockPID) == nil
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, lockPID, metadataPID)
	t.Logf("file completion: metadata_pid=%d fresh_lock_pid=%d same_backend=true", metadataPID, lockPID)
	require.NoError(t, blocker.Rollback())
	select {
	case err := <-done:
		done = nil
		require.ErrorContains(t, err, "original execution stopped")
	case <-time.After(5 * time.Second):
		t.Fatal("metadata completion did not finish")
	}
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM FileInfo WHERE Id=$1`, file.Id).Scan(&count))
	require.Zero(t, count)
	require.Error(t, execution.checkSession(), "the failed execution must remain closed")
}

func TestRetentionFileCompletionStopsWaitingAtItsBudget(t *testing.T) {
	th, worker, execution, file := completionFixture(t)
	worker.queryTimeout = 250 * time.Millisecond
	db := th.Store.GetInternalMasterDB()
	holder, err := db.Begin()
	require.NoError(t, err)
	defer holder.Rollback()
	_, err = holder.Exec(`SELECT pg_advisory_xact_lock($1)`, retentionExecutionLock)
	require.NoError(t, err)
	started := time.Now()
	err = worker.completeDeletedFile(execution, file)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), 2*time.Second)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM FileInfo WHERE Id=$1`, file.Id).Scan(&count))
	require.Equal(t, 1, count, "failure to obtain a fresh lock must preserve the metadata")
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()
            AND application_name=$1`, "retention_completion_"+file.Id).Scan(&count) == nil && count == 0
	}, time.Second, 10*time.Millisecond, "completion must close its temporary SQL pool")
	require.NoError(t, holder.Rollback())
	fresh := New(th.Server).MakeWorker().(*Worker)
	reason, err := fresh.execute(th.Context, th.Context.Logger(), make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, continueWork, reason)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM FileInfo WHERE Id=$1`, file.Id).Scan(&count))
	require.Zero(t, count)
}

func TestRetentionFileCompletionPreservesChangedStorageIdentity(t *testing.T) {
	for _, field := range []string{"Path", "PreviewPath", "ThumbnailPath", "CreatorId", "CreateAt", "UpdateAt", "DeleteAt"} {
		t.Run(field, func(t *testing.T) {
			th, worker, execution, file := completionFixture(t)
			var value any = "replacement/path.txt"
			switch field {
			case "CreatorId":
				value = model.BookmarkFileOwner
			case "CreateAt":
				value = file.CreateAt + 1
			case "UpdateAt":
				value = file.UpdateAt + 1
			case "DeleteAt":
				value = model.GetMillis()
			}
			db := th.Store.GetInternalMasterDB()
			_, err := db.Exec(`UPDATE FileInfo SET `+field+`=$1 WHERE Id=$2`, value, file.Id)
			require.NoError(t, err)
			require.ErrorIs(t, worker.completeDeletedFile(execution, file), errRetentionFileChanged)
			var count int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM FileInfo WHERE Id=$1`, file.Id).Scan(&count))
			require.Equal(t, 1, count, "completion must preserve a file version that it did not remove")
		})
	}
}
