package data_retention_job

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/mattermost/enterprise/data_retention"
	"github.com/mattermost/enterprise/internal/objectstoretest"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	app.RegisterDataRetentionInterface(data_retention.New)
	if path := os.Getenv(retentionProcessEnv); path != "" {
		if err := runRetentionTestProcess(path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestRetentionJobStopAndCancellationKeepTheirStatuses(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	worker := New(th.Server).MakeWorker().(*Worker)
	for _, tc := range []struct {
		name   string
		reason stopReason
		cancel bool
		want   string
	}{
		{"shutdown", workerStopped, false, model.JobStatusPending},
		{"cancel observed", jobCanceled, true, model.JobStatusCanceled},
		{"cancel before shutdown", workerStopped, true, model.JobStatusCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := &model.Job{Id: model.NewId(), Type: model.JobTypeDataRetention,
				CreateAt: model.GetMillis(), Status: model.JobStatusInProgress, Data: model.StringMap{}}
			_, err := th.Store.Job().Save(job)
			require.NoError(t, err)
			if tc.cancel {
				require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
			}
			require.Equal(t, tc.reason == workerStopped, worker.finishInterruption(th.App.Log(), job, tc.reason))
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, tc.want, stored.Status)
		})
	}
}

func TestRetentionCancellationWinsOverLateSuccessAndError(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	worker := New(th.Server).MakeWorker().(*Worker)
	for _, finish := range []struct {
		name string
		run  func(*testing.T, *model.Job)
	}{
		{"success", func(t *testing.T, job *model.Job) {
			updated, err := worker.transitionJob(job, model.JobStatusInProgress, model.JobStatusSuccess)
			require.NoError(t, err)
			require.False(t, updated)
		}},
		{"error", func(_ *testing.T, job *model.Job) {
			worker.finishError(th.App.Log(), job, model.NewAppError("test", "app.job.error", nil, "test", 500))
		}},
	} {
		t.Run(finish.name, func(t *testing.T) {
			job := &model.Job{Id: model.NewId(), Type: model.JobTypeDataRetention,
				CreateAt: model.GetMillis(), Status: model.JobStatusInProgress, Data: model.StringMap{}}
			_, err := th.Store.Job().Save(job)
			require.NoError(t, err)
			require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
			finish.run(t, job)
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			if finish.name == "success" {
				require.Equal(t, model.JobStatusCancelRequested, stored.Status)
				require.True(t, worker.finishInterruption(th.App.Log(), job, workerStopped))
			} else {
				require.Equal(t, model.JobStatusCanceled, stored.Status)
			}
			stored, err = th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, model.JobStatusCanceled, stored.Status)
		})
	}
}

func TestRetentionJobNormalTerminalStatuses(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	worker := New(th.Server).MakeWorker().(*Worker)
	for _, status := range []string{model.JobStatusSuccess, model.JobStatusError} {
		t.Run(status, func(t *testing.T) {
			job := &model.Job{Id: model.NewId(), Type: model.JobTypeDataRetention,
				CreateAt: model.GetMillis(), Status: model.JobStatusInProgress, Data: model.StringMap{}}
			_, err := th.Store.Job().Save(job)
			require.NoError(t, err)
			if status == model.JobStatusError {
				worker.finishError(th.App.Log(), job, model.NewAppError("test", "app.job.error", nil, "retention failed", 500))
			} else {
				require.Nil(t, th.Server.Jobs.SetJobProgress(job, 100))
				updated, finishErr := worker.transitionJob(job, model.JobStatusInProgress, model.JobStatusSuccess)
				require.NoError(t, finishErr)
				require.True(t, updated)
			}
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, status, stored.Status)
			if status == model.JobStatusError {
				require.Equal(t, int64(-1), stored.Progress)
				require.Contains(t, stored.Data["error"], "retention failed")
			} else {
				require.Equal(t, int64(100), stored.Progress)
			}
		})
	}
}

func TestRetentionWorkerDeletesExpiredPostAndKeepsCurrentPost(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.DataRetentionSettings.EnableMessageDeletion = new(true)
		cfg.DataRetentionSettings.MessageRetentionDays = new(0)
		cfg.DataRetentionSettings.MessageRetentionHours = new(1)
		cfg.DataRetentionSettings.TimeBetweenBatchesMilliseconds = new(0)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	oldPost := th.BasicPost
	currentPost := th.CreatePost(t)
	_, err := th.Store.GetInternalMasterDB().Exec("UPDATE Posts SET CreateAt=$1 WHERE Id=$2", time.Now().Add(-48*time.Hour).UnixMilli(), oldPost.Id)
	require.NoError(t, err)
	worker := New(th.Server).MakeWorker().(*Worker)
	ctx, cancel := context.WithTimeout(th.Context.Context(), 20*time.Second)
	defer cancel()
	reason, err := worker.execute(th.Context.WithContext(ctx), th.Context.Logger(), make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, continueWork, reason)
	var count int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow("SELECT COUNT(*) FROM Posts WHERE Id=$1", oldPost.Id).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow("SELECT COUNT(*) FROM Posts WHERE Id=$1", currentPost.Id).Scan(&count))
	require.Equal(t, 1, count)
}

func TestRetentionWorkersSerializeAcrossDatabaseConnections(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.DataRetentionSettings.EnableMessageDeletion = new(true)
		cfg.DataRetentionSettings.MessageRetentionDays = new(0)
		cfg.DataRetentionSettings.MessageRetentionHours = new(1)
		cfg.DataRetentionSettings.TimeBetweenBatchesMilliseconds = new(0)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	oldPost := th.BasicPost
	db := th.Store.GetInternalMasterDB()
	worker := New(th.Server).MakeWorker().(*Worker)
	maxConnections := db.Stats().MaxOpenConnections
	defer db.SetMaxOpenConns(maxConnections)
	_, err := db.Exec("UPDATE Posts SET CreateAt=$1 WHERE Id=$2", time.Now().Add(-48*time.Hour).UnixMilli(), oldPost.Id)
	require.NoError(t, err)

	lockHolder, err := db.BeginTx(th.Context.Context(), nil)
	require.NoError(t, err)
	defer lockHolder.Rollback()
	_, err = lockHolder.Exec(`SELECT pg_advisory_xact_lock($1)`, retentionExecutionLock)
	require.NoError(t, err)
	deadlineCtx, cancel := context.WithTimeout(th.Context.Context(), 700*time.Millisecond)
	defer cancel()
	reason, err := worker.execute(th.Context.WithContext(deadlineCtx), th.Context.Logger(), make(chan struct{}))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, continueWork, reason)
	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM Posts WHERE Id=$1", oldPost.Id).Scan(&count))
	require.Equal(t, 1, count, "another worker must not delete while the lock is held")

	canceled := make(chan struct{})
	timer := time.AfterFunc(100*time.Millisecond, func() { close(canceled) })
	defer timer.Stop()
	reason, err = worker.execute(th.Context, th.Context.Logger(), canceled)
	require.NoError(t, err)
	require.Equal(t, jobCanceled, reason, "cancellation must interrupt lock waiting")
	require.NoError(t, lockHolder.Rollback())

	// The execution owns a separate one-connection pool and must not consume
	// the application's only master connection while holding its lock.
	db.SetMaxOpenConns(1)
	runCtx, stop := context.WithTimeout(th.Context.Context(), 20*time.Second)
	defer stop()
	reason, err = worker.execute(th.Context.WithContext(runCtx), th.Context.Logger(), make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, continueWork, reason)
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM Posts WHERE Id=$1", oldPost.Id).Scan(&count))
	require.Zero(t, count, "the worker must continue after the other lock is released")
}

func TestRetentionWorkerHonorsPreexistingChannelAndTeamPolicies(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.DataRetentionSettings.EnableMessageDeletion = new(true)
		cfg.DataRetentionSettings.MessageRetentionDays = new(365)
		cfg.DataRetentionSettings.TimeBetweenBatchesMilliseconds = new(0)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))

	oldPost := th.BasicPost
	currentPost := th.CreatePost(t)
	oldTeamPost, _, appErr := th.App.CreatePostAsUser(th.Context, &model.Post{
		UserId: th.BasicUser.Id, ChannelId: th.BasicChannel2.Id, Message: "preexisting team policy",
	}, "", false)
	require.Nil(t, appErr)
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec("UPDATE Posts SET CreateAt=$1 WHERE Id=$2", time.Now().Add(-48*time.Hour).UnixMilli(), oldPost.Id)
	require.NoError(t, err)
	_, err = db.Exec("UPDATE Posts SET CreateAt=$1 WHERE Id=$2", time.Now().Add(-48*time.Hour).UnixMilli(), oldTeamPost.Id)
	require.NoError(t, err)
	// A policy row written before this Enterprise implementation used the
	// existing core tables. Verify that API and worker both honor that row.
	policyID := model.NewId()
	_, err = db.Exec("INSERT INTO retentionpolicies (id, displayname, postduration) VALUES ($1,$2,$3)", policyID, "Existing channel policy", 1)
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO retentionpolicieschannels (policyid, channelid) VALUES ($1,$2)", policyID, th.BasicChannel.Id)
	require.NoError(t, err)
	teamPolicyID := model.NewId()
	_, err = db.Exec("INSERT INTO retentionpolicies (id, displayname, postduration) VALUES ($1,$2,$3)", teamPolicyID, "Existing team policy", 1)
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO retentionpoliciesteams (policyid, teamid) VALUES ($1,$2)", teamPolicyID, th.BasicTeam.Id)
	require.NoError(t, err)

	policy, response, err := th.SystemAdminClient.GetDataRetentionPolicyByID(th.Context.Context(), policyID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, int64(1), *policy.PostDurationDays)
	require.Equal(t, int64(1), policy.ChannelCount)
	teamPolicy, response, err := th.SystemAdminClient.GetDataRetentionPolicyByID(th.Context.Context(), teamPolicyID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, int64(1), teamPolicy.TeamCount)
	seedRetentionPostTranslations(t, th, oldPost, oldTeamPost, currentPost)

	worker := New(th.Server).MakeWorker().(*Worker)
	ctx, cancel := context.WithTimeout(th.Context.Context(), 20*time.Second)
	defer cancel()
	reason, err := worker.execute(th.Context.WithContext(ctx), th.Context.Logger(), make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, continueWork, reason)
	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM Posts WHERE Id=$1", oldPost.Id).Scan(&count))
	require.Zero(t, count, "the preexisting channel policy must delete the expired post")
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM Posts WHERE Id=$1", oldTeamPost.Id).Scan(&count))
	require.Zero(t, count, "the preexisting team policy must delete the expired post")
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM Posts WHERE Id=$1", currentPost.Id).Scan(&count))
	require.Equal(t, 1, count, "a recent post must remain")
	for _, post := range []*model.Post{oldPost, oldTeamPost, currentPost} {
		want := 0
		if post.Id == currentPost.Id {
			want = 2
		}
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM translations WHERE objecttype='post' AND objectid=$1`, post.Id).Scan(&count))
		require.Equal(t, want, count, "translations must follow the channel/team policy's actual deletion decision")
	}
}

func TestRetentionCancellationBeforeBatchPreservesExpiredPost(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.DataRetentionSettings.EnableMessageDeletion = new(true)
		cfg.DataRetentionSettings.MessageRetentionDays = new(0)
		cfg.DataRetentionSettings.MessageRetentionHours = new(1)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	oldPost := th.BasicPost
	_, err := th.Store.GetInternalMasterDB().Exec("UPDATE Posts SET CreateAt=$1 WHERE Id=$2", time.Now().Add(-48*time.Hour).UnixMilli(), oldPost.Id)
	require.NoError(t, err)
	worker := New(th.Server).MakeWorker().(*Worker)
	canceled := make(chan struct{})
	close(canceled)
	reason, runErr := worker.execute(th.Context, th.Context.Logger(), canceled)
	require.NoError(t, runErr)
	require.Equal(t, jobCanceled, reason)
	var count int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow("SELECT COUNT(*) FROM Posts WHERE Id=$1", oldPost.Id).Scan(&count))
	require.Equal(t, 1, count)
}

func TestRetentionWorkerKeepsExpiredPostWithoutLicense(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.DataRetentionSettings.EnableMessageDeletion = new(true)
		cfg.DataRetentionSettings.MessageRetentionDays = new(0)
		cfg.DataRetentionSettings.MessageRetentionHours = new(1)
	}).InitBasic(t)
	oldPost := th.BasicPost
	_, err := th.Store.GetInternalMasterDB().Exec("UPDATE Posts SET CreateAt=$1 WHERE Id=$2", time.Now().Add(-48*time.Hour).UnixMilli(), oldPost.Id)
	require.NoError(t, err)

	worker := New(th.Server).MakeWorker().(*Worker)
	reason, err := worker.execute(th.Context, th.Context.Logger(), make(chan struct{}))
	require.ErrorContains(t, err, "not enabled by the installed license")
	require.Equal(t, continueWork, reason)
	var count int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow("SELECT COUNT(*) FROM Posts WHERE Id=$1", oldPost.Id).Scan(&count))
	require.Equal(t, 1, count)
}

func TestRetentionWorkerStopsBeforeNextBatchWhenLicenseIsRemoved(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	worker := New(th.Server).MakeWorker().(*Worker)
	calls := 0
	reason, err := worker.runPolicyBatches(th.App.Log(), make(chan struct{}), 0, "posts", model.RetentionPolicyBatchConfigs{},
		func(model.RetentionPolicyBatchConfigs, model.RetentionPolicyCursor) (int64, model.RetentionPolicyCursor, error) {
			calls++
			th.Server.SetLicense(nil)
			return 1, model.RetentionPolicyCursor{}, nil
		})
	require.Equal(t, continueWork, reason)
	require.ErrorContains(t, err, "not enabled by the installed license")
	require.Equal(t, 1, calls, "no second deletion batch may start after license removal")
}

func TestRetentionWorkerStopsFileDeletionAfterLicenseRemoval(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.DataRetentionSettings.EnableMessageDeletion = new(false)
		cfg.DataRetentionSettings.EnableFileDeletion = new(true)
		cfg.DataRetentionSettings.FileRetentionDays = new(0)
		cfg.DataRetentionSettings.FileRetentionHours = new(1)
		cfg.DataRetentionSettings.BatchSize = new(1)
		cfg.DataRetentionSettings.TimeBetweenBatchesMilliseconds = new(1000)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	first, appErr := th.App.DoUploadFile(th.Context, time.Now().Add(-48*time.Hour), th.BasicTeam.Id, th.BasicChannel.Id, th.BasicUser.Id, "first.txt", []byte("first"), false)
	require.Nil(t, appErr)
	second, appErr := th.App.DoUploadFile(th.Context, time.Now().Add(-47*time.Hour), th.BasicTeam.Id, th.BasicChannel.Id, th.BasicUser.Id, "second.txt", []byte("second"), false)
	require.Nil(t, appErr)

	worker := New(th.Server).MakeWorker().(*Worker)
	type result struct {
		reason stopReason
		err    error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(th.Context.Context(), 15*time.Second)
	defer cancel()
	go func() {
		reason, err := worker.execute(th.Context.WithContext(ctx), th.Context.Logger(), make(chan struct{}))
		done <- result{reason, err}
	}()
	require.Eventually(t, func() bool {
		_, err := th.Store.FileInfo().GetFromMaster(first.Id)
		return err != nil
	}, 10*time.Second, 10*time.Millisecond)
	th.Server.SetLicense(nil)
	select {
	case outcome := <-done:
		require.Equal(t, continueWork, outcome.reason)
		require.ErrorContains(t, outcome.err, "not enabled by the installed license")
	case <-ctx.Done():
		t.Fatal("retention worker did not stop after license removal")
	}
	_, err := th.Store.FileInfo().GetFromMaster(second.Id)
	require.NoError(t, err)
	exists, appErr := th.App.FileExists(second.Path)
	require.Nil(t, appErr)
	require.True(t, exists, "the next file must remain after license removal")
}

func TestRetentionWorkerDeletesExpiredFilesAndPreservesCurrentAndBookmarkFiles(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.DataRetentionSettings.EnableMessageDeletion = new(false)
		cfg.DataRetentionSettings.EnableFileDeletion = new(true)
		cfg.DataRetentionSettings.FileRetentionDays = new(0)
		cfg.DataRetentionSettings.FileRetentionHours = new(1)
		cfg.DataRetentionSettings.BatchSize = new(1)
		cfg.DataRetentionSettings.TimeBetweenBatchesMilliseconds = new(0)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))

	oldTime := time.Now().Add(-48 * time.Hour)
	oldFile, appErr := th.App.DoUploadFile(th.Context, oldTime, th.BasicTeam.Id, th.BasicChannel.Id, th.BasicUser.Id, "expired.txt", []byte("expired"), false)
	require.Nil(t, appErr)
	currentFile, appErr := th.App.DoUploadFile(th.Context, time.Now(), th.BasicTeam.Id, th.BasicChannel.Id, th.BasicUser.Id, "current.txt", []byte("current"), false)
	require.Nil(t, appErr)
	bookmarkFile, appErr := th.App.DoUploadFile(th.Context, oldTime, th.BasicTeam.Id, th.BasicChannel.Id, model.BookmarkFileOwner, "bookmark.txt", []byte("bookmark"), false)
	require.Nil(t, appErr)

	worker := New(th.Server).MakeWorker().(*Worker)
	ctx, cancel := context.WithTimeout(th.Context.Context(), 20*time.Second)
	defer cancel()
	reason, err := worker.execute(th.Context.WithContext(ctx), th.Context.Logger(), make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, continueWork, reason)

	_, err = th.Store.FileInfo().GetFromMaster(oldFile.Id)
	require.Error(t, err)
	exists, appErr := th.App.FileExists(oldFile.Path)
	require.Nil(t, appErr)
	require.False(t, exists)

	for _, file := range []*model.FileInfo{currentFile, bookmarkFile} {
		_, err = th.Store.FileInfo().GetFromMaster(file.Id)
		require.NoError(t, err)
		exists, appErr = th.App.FileExists(file.Path)
		require.Nil(t, appErr)
		require.True(t, exists)
	}
}

func TestRetentionWorkerDeletesExpiredObjectStoreFiles(t *testing.T) {
	for _, emulator := range objectstoretest.Cases() {
		t.Run(emulator.Name, func(t *testing.T) {
			endpoint := os.Getenv(emulator.EndpointEnv)
			if endpoint == "" {
				t.Skipf("set %s to test retention against an object store emulator", emulator.EndpointEnv)
			}
			fileSettings := emulator.Setup(t, endpoint)
			th := api4.SetupConfig(t, func(cfg *model.Config) {
				cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
				cfg.FileSettings = fileSettings
				cfg.ServiceSettings.AllowedUntrustedInternalConnections = model.NewPointer("127.0.0.1")
				cfg.DataRetentionSettings.EnableMessageDeletion = new(false)
				cfg.DataRetentionSettings.EnableFileDeletion = new(true)
				cfg.DataRetentionSettings.FileRetentionDays = new(0)
				cfg.DataRetentionSettings.FileRetentionHours = new(1)
				cfg.DataRetentionSettings.BatchSize = new(1)
				cfg.DataRetentionSettings.TimeBetweenBatchesMilliseconds = new(0)
			}).InitBasic(t)
			require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))

			oldFile, appErr := th.App.DoUploadFile(th.Context, time.Now().Add(-48*time.Hour), th.BasicTeam.Id, th.BasicChannel.Id, th.BasicUser.Id, "expired.txt", []byte("expired"), false)
			require.Nil(t, appErr)
			currentFile, appErr := th.App.DoUploadFile(th.Context, time.Now(), th.BasicTeam.Id, th.BasicChannel.Id, th.BasicUser.Id, "current.txt", []byte("current"), false)
			require.Nil(t, appErr)
			bookmarkFile, appErr := th.App.DoUploadFile(th.Context, time.Now().Add(-48*time.Hour), th.BasicTeam.Id, th.BasicChannel.Id, model.BookmarkFileOwner, "bookmark.txt", []byte("bookmark"), false)
			require.Nil(t, appErr)

			worker := New(th.Server).MakeWorker().(*Worker)
			ctx, cancel := context.WithTimeout(th.Context.Context(), 30*time.Second)
			defer cancel()
			reason, err := worker.execute(th.Context.WithContext(ctx), th.Context.Logger(), make(chan struct{}))
			require.NoError(t, err)
			require.Equal(t, continueWork, reason)

			_, err = th.Store.FileInfo().GetFromMaster(oldFile.Id)
			require.Error(t, err)
			exists, appErr := th.App.FileExists(oldFile.Path)
			require.Nil(t, appErr)
			require.False(t, exists)
			for _, file := range []*model.FileInfo{currentFile, bookmarkFile} {
				_, err = th.Store.FileInfo().GetFromMaster(file.Id)
				require.NoError(t, err)
				exists, appErr = th.App.FileExists(file.Path)
				require.Nil(t, appErr)
				require.True(t, exists)
			}
		})
	}
}
