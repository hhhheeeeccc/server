package message_export

import (
	"context"
	"testing"
	"time"

	"github.com/mattermost/enterprise/message_export/shared"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestMessageExportRecoveryPreservesLiveOwnerAndFencesPreviousClaim(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.MessageExportSettings.EnableExport = model.NewPointer(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
	worker := NewJob(th.Server).MakeWorker().(*MessageExportWorker)
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeMessageExport, model.StringMap{
		shared.JobDataBatchNumber: "0", shared.JobDataExportDir: "export/preserved",
		shared.JobDataJobEndTime: "12345",
	})
	require.Nil(t, appErr)
	old, acquired, err := worker.acquireJobOwner(context.Background(), job.Id)
	require.NoError(t, err)
	require.True(t, acquired)
	defer old.Close()
	claimed, err := old.claim()
	require.NoError(t, err)
	require.NotNil(t, claimed)
	firstToken := claimed.Data[messageExportAttemptKey]
	require.True(t, model.IsValidId(firstToken))
	worker.execution = old
	claimed.Data[shared.JobDataBatchNumber] = "3"
	require.NoError(t, worker.setJobProgress(th.App.Log(), claimed, 40))
	_, acquired, err = worker.acquireJobOwner(context.Background(), job.Id)
	require.NoError(t, err)
	require.False(t, acquired, "one job cannot have two live owners")
	other, acquired, err := worker.acquireJobOwner(context.Background(), model.NewId())
	require.NoError(t, err)
	require.True(t, acquired, "unrelated exports have independent locks")
	other.Close()
	require.NoError(t, worker.recoverClaims())
	stored, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusInProgress, stored.Status)
	require.Equal(t, claimed.Data, stored.Data)
	old.Close()
	worker.execution = nil
	require.NoError(t, worker.recoverClaims())
	recovered, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusPending, recovered.Status)
	require.Equal(t, claimed.Data, recovered.Data)
	require.Equal(t, int64(40), recovered.Progress)
	current, acquired, err := worker.acquireJobOwner(context.Background(), job.Id)
	require.NoError(t, err)
	require.True(t, acquired)
	defer current.Close()
	next, err := current.claim()
	require.NoError(t, err)
	require.NotNil(t, next)
	require.NotEqual(t, firstToken, next.Data[messageExportAttemptKey])
	worker.execution = current
	next.Data[shared.JobDataBatchNumber] = "4"
	require.NoError(t, worker.setJobProgress(th.App.Log(), next, 50))
	claimed.Data[shared.JobDataBatchNumber] = "99"
	_, err = old.update(claimed, model.JobStatusSuccess, 100)
	require.ErrorIs(t, err, errMessageExportAttemptLost)
	worker.SetJobPending(th.App.Log(), claimed)
	worker.setJobCanceled(th.App.Log(), claimed)
	stored, err = th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusInProgress, stored.Status)
	require.Equal(t, next.Data, stored.Data)
	require.Equal(t, int64(50), stored.Progress)
}

func TestMessageExportOwnerMonitorsCancellationAndLockLoss(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.MessageExportSettings.EnableExport = model.NewPointer(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
	worker := NewJob(th.Server).MakeWorker().(*MessageExportWorker)
	for _, action := range []string{"cancel", "unlock", "terminate", "disable", "license"} {
		t.Run(action, func(t *testing.T) {
			th.App.UpdateConfig(func(cfg *model.Config) { cfg.MessageExportSettings.EnableExport = model.NewPointer(true) })
			require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
			job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeMessageExport, nil)
			require.Nil(t, appErr)
			owner, acquired, err := worker.acquireJobOwner(context.Background(), job.Id)
			require.NoError(t, err)
			require.True(t, acquired)
			defer owner.Close()
			claimed, err := owner.claim()
			require.NoError(t, err)
			require.NotNil(t, claimed)
			owner.done = make(chan struct{})
			go owner.monitor(worker)
			switch action {
			case "cancel":
				require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
			case "unlock":
				_, err := owner.exec(`SELECT pg_advisory_unlock_all()`)
				require.NoError(t, err)
			case "terminate":
				var pid int
				query, stop := owner.queryContext()
				err := owner.db.QueryRowContext(query, `SELECT pg_backend_pid()`).Scan(&pid)
				stop()
				require.NoError(t, err)
				_, err = th.Store.GetInternalMasterDB().Exec(`SELECT pg_terminate_backend($1)`, pid)
				require.NoError(t, err)
			case "disable":
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.MessageExportSettings.EnableExport = model.NewPointer(false) })
			case "license":
				th.Server.SetLicense(nil)
			}
			select {
			case <-owner.ctx.Done():
			case <-time.After(4 * time.Second):
				t.Fatal("owning context did not stop after losing execution authority")
			}
			claimed.Data[shared.JobDataIsDownloadable] = "true"
			_, err = owner.update(claimed, model.JobStatusSuccess, 100)
			require.ErrorIs(t, err, errMessageExportAttemptLost)
			owner.Close()
			worker.SetJobPending(th.App.Log(), claimed)
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			expected := model.JobStatusPending
			if action == "cancel" {
				expected = model.JobStatusCanceled
			}
			require.Equal(t, expected, stored.Status)
			require.NotEqual(t, "true", stored.Data[shared.JobDataIsDownloadable])
		})
	}
}

func TestMessageExportRecoveryGatesAndRecognizedClaims(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.MessageExportSettings.EnableExport = model.NewPointer(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
	worker := NewJob(th.Server).MakeWorker().(*MessageExportWorker)
	type recoveryCase struct {
		status, token, expected string
	}
	cases := []recoveryCase{
		{model.JobStatusInProgress, model.NewId(), model.JobStatusPending},
		{model.JobStatusCancelRequested, model.NewId(), model.JobStatusCanceled},
		{model.JobStatusInProgress, "", model.JobStatusInProgress},
		{model.JobStatusInProgress, "unknown-protocol", model.JobStatusInProgress},
		{model.JobStatusSuccess, model.NewId(), model.JobStatusSuccess},
		{model.JobStatusPending, model.NewId(), model.JobStatusPending},
	}
	var saved []*model.Job
	for _, tc := range cases {
		job, err := th.Store.Job().Save(&model.Job{Id: model.NewId(), Type: model.JobTypeMessageExport,
			CreateAt: model.GetMillis(), Status: tc.status, Progress: 40,
			Data: model.StringMap{messageExportAttemptKey: tc.token, shared.JobDataBatchNumber: "2"}})
		require.NoError(t, err)
		saved = append(saved, job)
	}
	for _, gate := range []string{"license", "disabled"} {
		if gate == "license" {
			th.Server.SetLicense(nil)
		} else {
			require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
			th.App.UpdateConfig(func(cfg *model.Config) { cfg.MessageExportSettings.EnableExport = model.NewPointer(false) })
		}
		require.NoError(t, worker.recoverClaims())
		stored, err := th.Store.Job().Get(th.Context, saved[0].Id)
		require.NoError(t, err)
		require.Equal(t, model.JobStatusInProgress, stored.Status)
	}
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.MessageExportSettings.EnableExport = model.NewPointer(true) })
	require.NoError(t, worker.recoverClaims())
	for i, job := range saved {
		stored, err := th.Store.Job().Get(th.Context, job.Id)
		require.NoError(t, err)
		require.Equal(t, cases[i].expected, stored.Status)
		require.Equal(t, job.Data, stored.Data)
		require.Equal(t, job.Progress, stored.Progress)
	}
}

func TestMessageExportRecoveryBoundsAndRotatesPastLiveOwners(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.MessageExportSettings.EnableExport = model.NewPointer(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
	worker := NewJob(th.Server).MakeWorker().(*MessageExportWorker)
	cohort := model.NewId()
	var live *messageExportOwner
	for i := 0; i < 105; i++ {
		job, err := th.Store.Job().Save(&model.Job{Id: model.NewId(), Type: model.JobTypeMessageExport,
			CreateAt: model.GetMillis() + int64(i), Status: model.JobStatusInProgress,
			Data: model.StringMap{messageExportAttemptKey: model.NewId(), "cohort": cohort}})
		require.NoError(t, err)
		if i == 0 {
			var acquired bool
			live, acquired, err = worker.acquireJobOwner(context.Background(), job.Id)
			require.NoError(t, err)
			require.True(t, acquired)
			defer live.Close()
		}
	}
	for _, expected := range []int{6, 1, 1} {
		require.NoError(t, worker.recoverClaims())
		var remaining int
		err := th.Store.GetInternalMasterDB().QueryRow(`SELECT COUNT(*) FROM Jobs WHERE Type=$1 AND Status=$2 AND Data->>'cohort'=$3`,
			model.JobTypeMessageExport, model.JobStatusInProgress, cohort).Scan(&remaining)
		require.NoError(t, err)
		require.Equal(t, expected, remaining)
	}
	live.Close()
	require.NoError(t, worker.recoverClaims())
	var remaining int
	err := th.Store.GetInternalMasterDB().QueryRow(`SELECT COUNT(*) FROM Jobs WHERE Type=$1 AND Status=$2 AND Data->>'cohort'=$3`,
		model.JobTypeMessageExport, model.JobStatusInProgress, cohort).Scan(&remaining)
	require.NoError(t, err)
	require.Zero(t, remaining)
}

func TestMessageExportPanicFinishesOnlyOwnedAttempt(t *testing.T) {
	directory := t.TempDir()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.MessageExportSettings.EnableExport = model.NewPointer(true)
		cfg.MessageExportSettings.ExportFormat = model.NewPointer(model.ComplianceExportTypeCsv)
		cfg.MessageExportSettings.BatchSize = model.NewPointer(1)
		cfg.FileSettings.Directory = &directory
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
	post, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: th.BasicChannel.Id,
		UserId: th.BasicUser.Id, Message: "message export panic test"})
	require.NoError(t, err)
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeMessageExport,
		model.StringMap{shared.JobDataBatchStartTime: "0"})
	require.Nil(t, appErr)
	testEndOfBatchCb = func(*MessageExportWorker) { panic("test panic after committed checkpoint") }
	t.Cleanup(func() { testEndOfBatchCb = nil })
	worker := NewJob(th.Server).MakeWorker().(*MessageExportWorker)
	worker.DoJob(job)
	stored, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusError, stored.Status)
	require.Equal(t, int64(-1), stored.Progress)
	require.True(t, model.IsValidId(stored.Data[messageExportAttemptKey]))
	require.Equal(t, "1", stored.Data[shared.JobDataBatchNumber])
	require.Contains(t, stored.Data["error"], "test panic after committed checkpoint")
	require.NotEqual(t, "true", stored.Data[shared.JobDataIsDownloadable])
	require.NotEmpty(t, post.Id)
}
