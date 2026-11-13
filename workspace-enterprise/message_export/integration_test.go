package message_export

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/enterprise/message_export/actiance_export"
	"github.com/mattermost/enterprise/message_export/shared"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

type progressVerificationFailureStore struct {
	store.Store
	jobStore store.JobStore
}

func (s progressVerificationFailureStore) Job() store.JobStore { return s.jobStore }

type progressVerificationFailureJobStore struct {
	store.JobStore
	failed bool
}

type progressWriteFailureJobStore struct {
	store.JobStore
}

func (s progressWriteFailureJobStore) UpdateOptimistically(*model.Job, string) (*model.Job, error) {
	return nil, errors.New("progress write unavailable")
}

func (s *progressVerificationFailureJobStore) UpdateOptimistically(*model.Job, string) (*model.Job, error) {
	// The core allows a zero-row progress update to return without error.
	return nil, nil
}

func (s *progressVerificationFailureJobStore) UpdateStatusOptimistically(id, from, to string) (*model.Job, error) {
	if !s.failed && from == model.JobStatusInProgress && to == model.JobStatusInProgress {
		s.failed = true
		return nil, errors.New("verification unavailable")
	}
	return s.JobStore.UpdateStatusOptimistically(id, from, to)
}

func TestMain(m *testing.M) {
	app.RegisterMessageExportInterface(New)
	app.RegisterJobsMessageExportJobInterface(NewJob)
	if input := os.Getenv(messageExportProcessEnv); input != "" {
		os.Exit(runMessageExportProcess(input))
	}
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestStartSynchronizeJobDisabled(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.MessageExportSettings.EnableExport = new(false)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))

	job, appErr := th.App.MessageExport().StartSynchronizeJob(th.Context, 0)
	require.Nil(t, job)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusForbidden, appErr.StatusCode)
}

func TestExportDirectoryIsUniquePerJobAndStableOnResume(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	worker := &MessageExportWorker{jobServer: th.Server.Jobs}
	now := time.Now()
	first := &model.Job{Id: model.NewId(), Data: model.StringMap{shared.JobDataBatchStartTime: "100"}}
	second := &model.Job{Id: model.NewId(), Data: model.StringMap{shared.JobDataBatchStartTime: "100"}}
	require.NoError(t, worker.initJobData(th.Context, th.App.Log(), first, now))
	require.NoError(t, worker.initJobData(th.Context, th.App.Log(), second, now))
	require.NotEqual(t, first.Data[shared.JobDataExportDir], second.Data[shared.JobDataExportDir])

	original := first.Data[shared.JobDataExportDir]
	require.NoError(t, worker.initJobData(th.Context, th.App.Log(), first, now.Add(time.Minute)))
	require.Equal(t, original, first.Data[shared.JobDataExportDir])
	first.Data[shared.JobDataExportDir] = "export/legacy-archive"
	require.NoError(t, worker.initJobData(th.Context, th.App.Log(), first, now))
	require.Equal(t, "export/legacy-archive", first.Data[shared.JobDataExportDir])
}

func TestMessageExportClaimStopsWithoutLicense(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.MessageExportSettings.EnableExport = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeMessageExport, nil)
	require.Nil(t, appErr)

	th.Server.SetLicense(nil)
	worker := &MessageExportWorker{
		jobServer: th.Server.Jobs,
		server:    th.Server,
		logger:    th.App.Log(),
		license:   th.Server.License,
	}
	worker.DoJob(job)

	stored, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusPending, stored.Status)
}

func TestMessageExportStopsBetweenBatchesWhenLicenseRemoved(t *testing.T) {
	exportDirectory := t.TempDir()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.FileSettings.Directory = &exportDirectory
		cfg.MessageExportSettings.EnableExport = new(true)
		cfg.MessageExportSettings.BatchSize = new(1)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
	start := model.GetMillis() - 1
	for i := 0; i < 2; i++ {
		_, err := th.Store.Post().Save(th.Context, &model.Post{
			ChannelId: th.BasicChannel.Id,
			UserId:    th.BasicUser.Id,
			Message:   "export license transition",
		})
		require.NoError(t, err)
	}
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeMessageExport, model.StringMap{
		shared.JobDataBatchStartTime: strconv.FormatInt(start, 10),
	})
	require.Nil(t, appErr)
	worker, ok := NewJob(th.Server).MakeWorker().(*MessageExportWorker)
	require.True(t, ok)
	firstBatchCompleted := false
	testEndOfBatchCb = func(*MessageExportWorker) {
		firstBatchCompleted = true
		th.Server.SetLicense(nil)
	}
	t.Cleanup(func() { testEndOfBatchCb = nil })
	worker.DoJob(job)

	require.True(t, firstBatchCompleted)
	stored, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusPending, stored.Status)
	require.Equal(t, "1", stored.Data[shared.JobDataBatchNumber])

	testEndOfBatchCb = nil
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
	replacementWorker, ok := NewJob(th.Server).MakeWorker().(*MessageExportWorker)
	require.True(t, ok)
	replacementWorker.DoJob(stored)
	completed, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusSuccess, completed.Status)
	require.Equal(t, "2", completed.Data[shared.JobDataBatchNumber])
}

func TestExportCompletionOnlyTransitionsInProgressJob(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	worker := &MessageExportWorker{jobServer: th.Server.Jobs}
	for _, finish := range []struct {
		name string
		run  func(*model.Job)
	}{
		{"success", func(job *model.Job) { worker.setJobSuccess(th.App.Log(), job) }},
		{"warning", func(job *model.Job) { worker.setJobWarning(th.App.Log(), job) }},
	} {
		for _, originalStatus := range []string{model.JobStatusCanceled, model.JobStatusCancelRequested, model.JobStatusInProgress} {
			t.Run(finish.name+"/"+originalStatus, func(t *testing.T) {
				job := &model.Job{Id: model.NewId(), Type: model.JobTypeMessageExport, CreateAt: model.GetMillis(), Status: originalStatus, Data: model.StringMap{}}
				_, err := th.Store.Job().Save(job)
				require.NoError(t, err)
				job.Data[shared.JobDataIsDownloadable] = "true"
				finish.run(job)
				stored, err := th.Store.Job().Get(th.Context, job.Id)
				require.NoError(t, err)
				if originalStatus == model.JobStatusInProgress {
					require.Equal(t, finish.name, stored.Status)
					require.Equal(t, int64(100), stored.Progress)
					require.Equal(t, "true", stored.Data[shared.JobDataIsDownloadable])
				} else {
					require.Equal(t, originalStatus, stored.Status)
					require.NotEqual(t, "true", stored.Data[shared.JobDataIsDownloadable])
				}
			})
		}
	}
}

func TestExportStopAndCancellationPreserveLatestStatus(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	worker := &MessageExportWorker{jobServer: th.Server.Jobs, server: th.Server}
	for _, tc := range []struct {
		name      string
		stopFirst bool
	}{
		{name: "cancel before stop"},
		{name: "cancel after stop", stopFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := &model.Job{
				Id: model.NewId(), Type: model.JobTypeMessageExport,
				CreateAt: model.GetMillis(), Status: model.JobStatusInProgress,
				Data: model.StringMap{},
			}
			_, err := th.Store.Job().Save(job)
			require.NoError(t, err)
			if !tc.stopFirst {
				require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
			}
			worker.SetJobPending(th.App.Log(), job)
			if tc.stopFirst {
				require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
			}
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, model.JobStatusCanceled, stored.Status)
		})
	}

	job := &model.Job{Id: model.NewId(), Type: model.JobTypeMessageExport,
		CreateAt: model.GetMillis(), Status: model.JobStatusSuccess, Data: model.StringMap{}}
	_, err := th.Store.Job().Save(job)
	require.NoError(t, err)
	worker.SetJobPending(th.App.Log(), job)
	worker.setJobCanceled(th.App.Log(), job)
	stored, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusSuccess, stored.Status, "an old worker must not overwrite a completed job")
}

func TestExportProgressRespectsCancellation(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	worker := &MessageExportWorker{jobServer: th.Server.Jobs, server: th.Server}
	logger := th.App.Log()

	job := &model.Job{Id: model.NewId(), Type: model.JobTypeMessageExport,
		CreateAt: model.GetMillis(), Status: model.JobStatusInProgress,
		Data: model.StringMap{shared.JobDataMessagesExported: "12"}}
	_, err := th.Store.Job().Save(job)
	require.NoError(t, err)
	require.NoError(t, worker.setJobProgress(logger, job, 25))
	stored, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusInProgress, stored.Status)
	require.Equal(t, int64(25), stored.Progress)
	require.Equal(t, "12", stored.Data[shared.JobDataMessagesExported])

	require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
	job.Data[shared.JobDataMessagesExported] = "13"
	require.ErrorIs(t, worker.setJobProgress(logger, job, 30), errJobNoLongerInProgress)
	stored, err = th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusCanceled, stored.Status)
	require.Equal(t, "12", stored.Data[shared.JobDataMessagesExported], "a canceled batch must not advance the saved cursor")
}

func TestExportProgressVerificationFailureReturnsJobToPending(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	job := &model.Job{Id: model.NewId(), Type: model.JobTypeMessageExport,
		CreateAt: model.GetMillis(), Status: model.JobStatusInProgress,
		Data: model.StringMap{shared.JobDataMessagesExported: "12"}}
	_, err := th.Store.Job().Save(job)
	require.NoError(t, err)

	jobStore := &progressVerificationFailureJobStore{JobStore: th.Store.Job()}
	worker := &MessageExportWorker{
		jobServer: &jobs.JobServer{Store: progressVerificationFailureStore{Store: th.Store, jobStore: jobStore}},
		server:    th.Server,
	}
	require.ErrorContains(t, worker.setJobProgress(th.App.Log(), job, 30), "verification unavailable")
	require.True(t, jobStore.failed)
	stored, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusPending, stored.Status)
	require.Equal(t, "12", stored.Data[shared.JobDataMessagesExported])
}

func TestExportProgressWriteFailurePreservesCursorAndCancellation(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	for _, tc := range []struct {
		name     string
		cancel   bool
		expected string
	}{
		{name: "retry", expected: model.JobStatusPending},
		{name: "cancel", cancel: true, expected: model.JobStatusCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := &model.Job{Id: model.NewId(), Type: model.JobTypeMessageExport,
				CreateAt: model.GetMillis(), Status: model.JobStatusInProgress,
				Data: model.StringMap{shared.JobDataMessagesExported: "12"}}
			_, err := th.Store.Job().Save(job)
			require.NoError(t, err)
			if tc.cancel {
				require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
			}
			worker := &MessageExportWorker{
				jobServer: &jobs.JobServer{Store: progressVerificationFailureStore{
					Store: th.Store, jobStore: progressWriteFailureJobStore{JobStore: th.Store.Job()},
				}},
				server: th.Server,
			}
			job.Data[shared.JobDataMessagesExported] = "13"
			require.ErrorContains(t, worker.setJobProgress(th.App.Log(), job, 30), "progress write unavailable")
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, tc.expected, stored.Status)
			require.Equal(t, "12", stored.Data[shared.JobDataMessagesExported])
		})
	}
}

func TestExportInitializationDoesNotRestartHistoryWhenPreviousJobLookupFails(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	worker := &MessageExportWorker{jobServer: th.Server.Jobs}
	logger := th.App.Log()

	first := &model.Job{Data: model.StringMap{}}
	require.NoError(t, worker.initJobData(th.Context, logger, first, time.Now()))
	require.Equal(t, strconv.FormatInt(*th.App.Config().MessageExportSettings.ExportFromTimestamp, 10),
		first.Data[shared.JobDataBatchStartTime], "a confirmed absence of earlier jobs uses the configured start")

	ctx, cancel := context.WithCancel(th.Context.Context())
	cancel()
	failed := &model.Job{Data: model.StringMap{}}
	err := worker.initJobData(th.Context.WithContext(ctx), logger, failed, time.Now())
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, failed.Data[shared.JobDataBatchStartTime], "a failed lookup must not restart the export from the configured start")
}

func TestExportResumeCursorUsesLatestNonCLIJobOnMaster(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	worker := &MessageExportWorker{jobServer: th.Server.Jobs}
	now := model.GetMillis()
	for _, tc := range []struct {
		created int64
		cursor  string
		byMMCTL bool
	}{
		{now - 3000, "100", false},
		{now - 2000, "200", false},
		{now - 1000, "300", true},
	} {
		data := model.StringMap{shared.JobDataBatchStartTime: tc.cursor}
		if tc.byMMCTL {
			data[shared.JobDataInitiatedBy] = "mmctl"
		}
		_, err := th.Store.Job().Save(&model.Job{
			Id: model.NewId(), Type: model.JobTypeMessageExport,
			CreateAt: tc.created, Status: model.JobStatusSuccess, Data: data,
		})
		require.NoError(t, err)
	}

	previous, err := worker.getPreviousNonCliJob(th.Context)
	require.NoError(t, err)
	require.NotNil(t, previous)
	require.Equal(t, "200", previous.Data[shared.JobDataBatchStartTime])
}

func TestCSVExportJobAndDownloadPermissions(t *testing.T) {
	testExportJobAndDownloadPermissions(t, model.ComplianceExportTypeCsv, exportTestOptions{})
}

func TestCSVExportIncludesAttachment(t *testing.T) {
	testExportJobAndDownloadPermissions(t, model.ComplianceExportTypeCsv, exportTestOptions{withAttachment: true})
}

func TestCSVExportWarnsWhenAttachmentIsMissing(t *testing.T) {
	testExportJobAndDownloadPermissions(t, model.ComplianceExportTypeCsv, exportTestOptions{withAttachment: true, missingAttachment: true})
}

func TestCSVExportRecordsDeletedAttachment(t *testing.T) {
	testExportJobAndDownloadPermissions(t, model.ComplianceExportTypeCsv, exportTestOptions{withAttachment: true, softDeleteAttachment: true})
}

func TestCSVExportStreamsAttachmentLargerThanCopyBuffer(t *testing.T) {
	testExportJobAndDownloadPermissions(t, model.ComplianceExportTypeCsv, exportTestOptions{withAttachment: true, largeAttachment: true})
}

func TestCSVExportUsesDedicatedLocalStore(t *testing.T) {
	testExportJobAndDownloadPermissions(t, model.ComplianceExportTypeCsv, exportTestOptions{dedicatedExportStore: true})
}

func TestCSVExportRecordsAttachmentRemovalInLaterJob(t *testing.T) {
	previousPolling := jobs.DefaultWatcherPollingInterval
	jobs.DefaultWatcherPollingInterval = 100
	t.Cleanup(func() { jobs.DefaultWatcherPollingInterval = previousPolling })

	exportDirectory := t.TempDir()
	format := model.ComplianceExportTypeCsv
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.FileSettings.Directory = &exportDirectory
		cfg.MessageExportSettings.EnableExport = new(true)
		cfg.MessageExportSettings.DownloadExportResults = new(true)
		cfg.MessageExportSettings.ExportFormat = &format
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
	require.NoError(t, th.Server.Jobs.StartWorkers())
	t.Cleanup(func() { require.NoError(t, th.Server.Jobs.StopWorkers()) })

	start := model.GetMillis() - 1
	content := []byte("attachment removed after first export")
	uploaded, _, err := th.Client.UploadFile(context.Background(), content, th.BasicChannel.Id, "removed-later.txt")
	require.NoError(t, err)
	require.Len(t, uploaded.FileInfos, 1)
	fileID := uploaded.FileInfos[0].Id
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{
		ChannelId: th.BasicChannel.Id,
		Message:   "later attachment removal",
		FileIds:   []string{fileID},
	})
	require.NoError(t, err)
	storedFile, fileErr := th.App.GetFileInfo(th.Context, fileID)
	require.Nil(t, fileErr)
	expectedPath := path.Join("files", post.Id, fileID+"-"+path.Base(storedFile.Path))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	firstJob, appErr := th.App.MessageExport().StartSynchronizeJob(th.Context.WithContext(ctx), start)
	require.Nil(t, appErr)
	require.Equal(t, model.JobStatusSuccess, firstJob.Status, "job data: %#v", firstJob.Data)
	firstArchive, _, err := th.SystemAdminClient.DownloadJob(ctx, firstJob.Id)
	require.NoError(t, err)
	require.True(t, archiveContainsFile(t, firstArchive, expectedPath, content))

	firstEnd, parseErr := strconv.ParseInt(firstJob.Data[shared.JobDataJobEndTime], 10, 64)
	require.NoError(t, parseErr)
	for model.GetMillis() <= firstEnd {
		time.Sleep(time.Millisecond)
	}
	emptyFileIDs := model.StringArray{}
	updated, _, err := th.Client.PatchPost(ctx, post.Id, &model.PostPatch{FileIds: &emptyFileIDs})
	require.NoError(t, err)
	require.Empty(t, updated.FileIds)
	files, err := th.Store.FileInfo().GetForPost(post.Id, true, true, false)
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Positive(t, files[0].DeleteAt)

	secondJob, appErr := th.App.MessageExport().StartSynchronizeJob(th.Context.WithContext(ctx), firstEnd)
	require.Nil(t, appErr)
	require.Equal(t, model.JobStatusSuccess, secondJob.Status, "job data: %#v", secondJob.Data)
	secondArchive, _, err := th.SystemAdminClient.DownloadJob(ctx, secondJob.Id)
	require.NoError(t, err)
	require.True(t, archiveContainsDeletedAttachmentRow(t, secondArchive, storedFile.Name))
	require.True(t, archiveContainsFile(t, secondArchive, expectedPath, content))
}

func TestCSVExportRecordsIndependentAttachmentDeletionInLaterJob(t *testing.T) {
	previousPolling := jobs.DefaultWatcherPollingInterval
	jobs.DefaultWatcherPollingInterval = 100
	t.Cleanup(func() { jobs.DefaultWatcherPollingInterval = previousPolling })

	exportDirectory := t.TempDir()
	format := model.ComplianceExportTypeCsv
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.FileSettings.Directory = &exportDirectory
		cfg.MessageExportSettings.EnableExport = new(true)
		cfg.MessageExportSettings.DownloadExportResults = new(true)
		cfg.MessageExportSettings.ExportFormat = &format
		cfg.MessageExportSettings.BatchSize = new(1)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
	require.NoError(t, th.Server.Jobs.StartWorkers())
	t.Cleanup(func() { require.NoError(t, th.Server.Jobs.StopWorkers()) })

	start := model.GetMillis() - 1
	content := []byte("independently deleted attachment")
	uploaded, _, err := th.Client.UploadFile(context.Background(), content, th.BasicChannel.Id, "independent.txt")
	require.NoError(t, err)
	require.Len(t, uploaded.FileInfos, 1)
	fileID := uploaded.FileInfos[0].Id
	uploadedSecond, _, err := th.Client.UploadFile(context.Background(), content, th.BasicChannel.Id, "independent-second.txt")
	require.NoError(t, err)
	require.Len(t, uploadedSecond.FileInfos, 1)
	secondFileID := uploadedSecond.FileInfos[0].Id
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{
		ChannelId: th.BasicChannel.Id,
		Message:   "independent removal",
		FileIds:   []string{fileID, secondFileID},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	firstJob, appErr := th.App.MessageExport().StartSynchronizeJob(th.Context.WithContext(ctx), start)
	require.Nil(t, appErr)
	require.Equal(t, model.JobStatusSuccess, firstJob.Status, "job data: %#v", firstJob.Data)
	firstEnd, parseErr := strconv.ParseInt(firstJob.Data[shared.JobDataJobEndTime], 10, 64)
	require.NoError(t, parseErr)
	for model.GetMillis() <= firstEnd {
		time.Sleep(time.Millisecond)
	}
	require.NoError(t, th.Store.FileInfo().DeleteForPostByIds(th.Context, post.Id, []string{fileID, secondFileID}))
	deletedFiles, err := th.Store.FileInfo().GetForPost(post.Id, true, true, false)
	require.NoError(t, err)
	require.Len(t, deletedFiles, 2)
	require.Equal(t, deletedFiles[0].DeleteAt, deletedFiles[1].DeleteAt)
	storedPost, getErr := th.Store.Post().GetSingle(th.Context, post.Id, true)
	require.NoError(t, getErr)
	require.Equal(t, post.UpdateAt, storedPost.UpdateAt)

	secondJob, appErr := th.App.MessageExport().StartSynchronizeJob(th.Context.WithContext(ctx), firstEnd)
	require.Nil(t, appErr)
	require.Equal(t, model.JobStatusSuccess, secondJob.Status, "job data: %#v", secondJob.Data)
	archive, _, err := th.SystemAdminClient.DownloadJob(ctx, secondJob.Id)
	require.NoError(t, err)
	require.Equal(t, 1, countDeletedAttachmentRows(t, archive, "independent.txt"))
	require.Equal(t, 1, countDeletedAttachmentRows(t, archive, "independent-second.txt"))
	require.NotEmpty(t, secondJob.Data[shared.JobDataFileDeleteStartId])

	secondEnd, parseErr := strconv.ParseInt(secondJob.Data[shared.JobDataJobEndTime], 10, 64)
	require.NoError(t, parseErr)
	for model.GetMillis() <= secondEnd {
		time.Sleep(time.Millisecond)
	}
	_, _, err = th.Client.CreatePost(ctx, &model.Post{ChannelId: th.BasicChannel.Id, Message: "after file deletion"})
	require.NoError(t, err)
	thirdJob, appErr := th.App.MessageExport().StartSynchronizeJob(th.Context.WithContext(ctx), -1)
	require.Nil(t, appErr)
	require.Equal(t, model.JobStatusSuccess, thirdJob.Status, "job data: %#v", thirdJob.Data)
	thirdArchive, _, err := th.SystemAdminClient.DownloadJob(ctx, thirdJob.Id)
	require.NoError(t, err)
	require.True(t, archiveContainsCSVMessage(t, thirdArchive, "after file deletion"))
	require.Zero(t, countDeletedAttachmentRows(t, thirdArchive, "independent.txt"))
	require.Zero(t, countDeletedAttachmentRows(t, thirdArchive, "independent-second.txt"))
}

func TestOtherFormatsRecordIndependentAttachmentDeletion(t *testing.T) {
	for _, format := range []string{model.ComplianceExportTypeActiance, model.ComplianceExportTypeGlobalrelayZip} {
		t.Run(format, func(t *testing.T) {
			previousPolling := jobs.DefaultWatcherPollingInterval
			jobs.DefaultWatcherPollingInterval = 100
			t.Cleanup(func() { jobs.DefaultWatcherPollingInterval = previousPolling })
			exportDirectory := t.TempDir()
			th := api4.SetupConfig(t, func(cfg *model.Config) {
				cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
				cfg.FileSettings.Directory = &exportDirectory
				cfg.MessageExportSettings.EnableExport = new(true)
				cfg.MessageExportSettings.DownloadExportResults = new(true)
				cfg.MessageExportSettings.ExportFormat = &format
			}).InitBasic(t)
			require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
			require.NoError(t, th.Server.Jobs.StartWorkers())
			t.Cleanup(func() { require.NoError(t, th.Server.Jobs.StopWorkers()) })

			start := model.GetMillis() - 1
			uploaded, _, err := th.Client.UploadFile(context.Background(), []byte("other format"), th.BasicChannel.Id, "independent-other.txt")
			require.NoError(t, err)
			require.Len(t, uploaded.FileInfos, 1)
			fileID := uploaded.FileInfos[0].Id
			post, _, err := th.Client.CreatePost(context.Background(), &model.Post{
				ChannelId: th.BasicChannel.Id, Message: "other format original", FileIds: []string{fileID},
			})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			firstJob, appErr := th.App.MessageExport().StartSynchronizeJob(th.Context.WithContext(ctx), start)
			require.Nil(t, appErr)
			require.Equal(t, model.JobStatusSuccess, firstJob.Status, "job data: %#v", firstJob.Data)
			firstEnd, parseErr := strconv.ParseInt(firstJob.Data[shared.JobDataJobEndTime], 10, 64)
			require.NoError(t, parseErr)
			for model.GetMillis() <= firstEnd {
				time.Sleep(time.Millisecond)
			}
			require.NoError(t, th.Store.FileInfo().DeleteForPostByIds(th.Context, post.Id, []string{fileID}))
			files, storeErr := th.Store.FileInfo().GetForPost(post.Id, true, true, false)
			require.NoError(t, storeErr)
			require.Len(t, files, 1)
			file := files[0]
			secondJob, appErr := th.App.MessageExport().StartSynchronizeJob(th.Context.WithContext(ctx), firstEnd)
			require.Nil(t, appErr)
			require.Equal(t, model.JobStatusSuccess, secondJob.Status, "job data: %#v", secondJob.Data)
			archive, _, err := th.SystemAdminClient.DownloadJob(ctx, secondJob.Id)
			require.NoError(t, err)
			if format == model.ComplianceExportTypeActiance {
				require.True(t, archiveContainsActianceMessage(t, archive, "delete "+file.Path))
			} else {
				require.True(t, archiveContainsGlobalRelayMessage(t, archive, "Deleted file "+file.Name))
			}
		})
	}
}

func TestActianceExportJobAndDownloadPermissions(t *testing.T) {
	testExportJobAndDownloadPermissions(t, model.ComplianceExportTypeActiance, exportTestOptions{})
}

func TestGlobalRelayZipExportJobAndDownloadPermissions(t *testing.T) {
	testExportJobAndDownloadPermissions(t, model.ComplianceExportTypeGlobalrelayZip, exportTestOptions{})
}

type exportTestOptions struct {
	withAttachment       bool
	missingAttachment    bool
	softDeleteAttachment bool
	largeAttachment      bool
	dedicatedExportStore bool
}

func testExportJobAndDownloadPermissions(t *testing.T, format string, options exportTestOptions) {
	t.Helper()
	previousPolling := jobs.DefaultWatcherPollingInterval
	jobs.DefaultWatcherPollingInterval = 100
	t.Cleanup(func() { jobs.DefaultWatcherPollingInterval = previousPolling })

	exportDirectory := t.TempDir()
	dedicatedDirectory := t.TempDir()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.FileSettings.Directory = &exportDirectory
		cfg.MessageExportSettings.EnableExport = new(true)
		cfg.MessageExportSettings.DownloadExportResults = new(true)
		cfg.MessageExportSettings.ExportFormat = &format
		cfg.MessageExportSettings.BatchSize = new(10)
		if options.dedicatedExportStore {
			cfg.FileSettings.DedicatedExportStore = new(true)
			cfg.FileSettings.ExportDriverName = new(model.ImageDriverLocal)
			cfg.FileSettings.ExportDirectory = &dedicatedDirectory
		}
	}).InitBasic(t)
	_, appErr := th.App.MessageExport().StartSynchronizeJob(th.Context, 0)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusForbidden, appErr.StatusCode)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
	require.True(t, *th.App.Config().JobSettings.RunJobs)
	require.NotNil(t, NewJob(th.Server).MakeWorker())
	require.NoError(t, th.Server.Jobs.StartWorkers())
	t.Cleanup(func() { require.NoError(t, th.Server.Jobs.StopWorkers()) })

	start := model.GetMillis() - 1
	message := "workspace enterprise " + format + " export integration marker"
	var attachedFile *model.FileInfo
	var post *model.Post
	var err error
	attachmentContent := []byte("workspace enterprise attachment export marker")
	if options.largeAttachment {
		attachmentContent = bytes.Repeat([]byte("workspace enterprise attachment export marker"), 80000)
	}
	if options.withAttachment {
		uploaded, _, uploadErr := th.Client.UploadFile(context.Background(), attachmentContent, th.BasicChannel.Id, "export-proof.txt")
		require.NoError(t, uploadErr)
		require.Len(t, uploaded.FileInfos, 1)
		attachedFile = uploaded.FileInfos[0]
		post, _, err = th.Client.CreatePost(context.Background(), &model.Post{
			ChannelId: th.BasicChannel.Id,
			Message:   message,
			FileIds:   []string{attachedFile.Id},
		})
	} else {
		post, err = th.Store.Post().Save(th.Context, &model.Post{
			ChannelId: th.BasicChannel.Id,
			UserId:    th.BasicUser.Id,
			Message:   message,
		})
	}
	require.NoError(t, err)
	require.Greater(t, post.UpdateAt, start)
	var storedFile *model.FileInfo
	if options.withAttachment {
		var fileErr *model.AppError
		storedFile, fileErr = th.App.GetFileInfo(th.Context, attachedFile.Id)
		require.Nil(t, fileErr)
		require.NotEmpty(t, storedFile.Path)
		if options.missingAttachment {
			require.NoError(t, os.Remove(filepath.Join(exportDirectory, storedFile.Path)))
		}
		if options.softDeleteAttachment {
			require.NoError(t, th.Store.FileInfo().DeleteForPostByIds(th.Context, post.Id, []string{storedFile.Id}))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	job, appErr := th.App.MessageExport().StartSynchronizeJob(th.Context.WithContext(ctx), start)
	require.Nil(t, appErr)
	if options.missingAttachment {
		require.Equal(t, model.JobStatusWarning, job.Status, "job data: %#v", job.Data)
		require.Equal(t, "1", job.Data["warning_count"])
	} else {
		require.Equal(t, model.JobStatusSuccess, job.Status, "job data: %#v", job.Data)
	}
	require.Equal(t, "true", job.Data["is_downloadable"])
	if options.dedicatedExportStore {
		require.NotEmpty(t, job.Data["export_dir"])
		batchDirectory := filepath.Join(dedicatedDirectory, job.Data["export_dir"])
		entries, readErr := os.ReadDir(batchDirectory)
		require.NoError(t, readErr)
		require.NotEmpty(t, entries)
		require.NoDirExists(t, filepath.Join(exportDirectory, job.Data["export_dir"]))
	}

	_, response, downloadErr := th.Client.DownloadJob(ctx, job.Id)
	require.Error(t, downloadErr)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	archive, response, downloadErr := th.SystemAdminClient.DownloadJob(ctx, job.Id)
	require.NoError(t, downloadErr)
	require.Equal(t, http.StatusOK, response.StatusCode)
	if format == model.ComplianceExportTypeCsv {
		require.True(t, archiveContainsCSVMessage(t, archive, message))
		if options.withAttachment {
			expectedPath := path.Join("files", post.Id, storedFile.Id+"-"+path.Base(storedFile.Path))
			if options.missingAttachment {
				require.False(t, archiveContainsFile(t, archive, expectedPath, attachmentContent))
				require.True(t, archiveContainsWarning(t, archive, storedFile.Path))
			} else {
				require.True(t, archiveContainsFile(t, archive, expectedPath, attachmentContent))
			}
			if options.softDeleteAttachment {
				require.True(t, archiveContainsDeletedAttachmentRow(t, archive, storedFile.Name))
			}
		}
	} else if format == model.ComplianceExportTypeActiance {
		require.True(t, archiveContainsActianceMessage(t, archive, message))
	} else {
		require.True(t, archiveContainsGlobalRelayMessage(t, archive, message))
	}
}

func archiveContainsDeletedAttachmentRow(t *testing.T, data []byte, filename string) bool {
	return countDeletedAttachmentRows(t, data, filename) > 0
}

func countDeletedAttachmentRows(t *testing.T, data []byte, filename string) int {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	count := 0
	for _, entry := range reader.File {
		file, openErr := entry.Open()
		require.NoError(t, openErr)
		contents, readErr := io.ReadAll(file)
		require.NoError(t, readErr)
		require.NoError(t, file.Close())
		if strings.HasSuffix(entry.Name, ".zip") {
			count += countDeletedAttachmentRows(t, contents, filename)
		}
		if strings.HasSuffix(entry.Name, "posts.csv") {
			rows, parseErr := csv.NewReader(bytes.NewReader(contents)).ReadAll()
			require.NoError(t, parseErr)
			for _, row := range rows[1:] {
				if len(row) > 17 && row[17] == "deleted attachment" && strings.Contains(row[16], filename) {
					count++
				}
			}
		}
	}
	return count
}

func archiveContainsWarning(t *testing.T, data []byte, filePath string) bool {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	for _, entry := range reader.File {
		file, openErr := entry.Open()
		require.NoError(t, openErr)
		contents, readErr := io.ReadAll(file)
		require.NoError(t, readErr)
		require.NoError(t, file.Close())
		if strings.HasSuffix(entry.Name, ".zip") && archiveContainsWarning(t, contents, filePath) {
			return true
		}
		if entry.Name == "warning.txt" && bytes.Contains(contents, []byte(filePath)) {
			return true
		}
	}
	return false
}

func archiveContainsFile(t *testing.T, data []byte, name string, expected []byte) bool {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	for _, entry := range reader.File {
		file, openErr := entry.Open()
		require.NoError(t, openErr)
		contents, readErr := io.ReadAll(file)
		require.NoError(t, readErr)
		require.NoError(t, file.Close())
		if strings.HasSuffix(entry.Name, ".zip") && archiveContainsFile(t, contents, name, expected) {
			return true
		}
		if entry.Name == name && bytes.Equal(contents, expected) {
			return true
		}
	}
	return false
}

func archiveContainsCSVMessage(t *testing.T, data []byte, message string) bool {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	for _, entry := range reader.File {
		file, openErr := entry.Open()
		require.NoError(t, openErr)
		contents, readErr := io.ReadAll(file)
		require.NoError(t, readErr)
		require.NoError(t, file.Close())
		if strings.HasSuffix(entry.Name, ".zip") {
			if archiveContainsCSVMessage(t, contents, message) {
				return true
			}
			continue
		}
		if strings.HasSuffix(entry.Name, "posts.csv") {
			rows, parseErr := csv.NewReader(bytes.NewReader(contents)).ReadAll()
			require.NoError(t, parseErr)
			for _, row := range rows[1:] {
				if len(row) > 16 && row[16] == message {
					return true
				}
			}
		}
	}
	return false
}

func archiveContainsActianceMessage(t *testing.T, data []byte, message string) bool {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	for _, entry := range reader.File {
		file, openErr := entry.Open()
		require.NoError(t, openErr)
		contents, readErr := io.ReadAll(file)
		require.NoError(t, readErr)
		require.NoError(t, file.Close())
		if strings.HasSuffix(entry.Name, ".zip") {
			if archiveContainsActianceMessage(t, contents, message) {
				return true
			}
			continue
		}
		if strings.HasSuffix(entry.Name, actiance_export.ActianceExportFilename) {
			var document struct {
				XMLName  xml.Name `xml:"FileDump"`
				Channels []struct {
					Messages []struct {
						Content string `xml:"Content"`
					} `xml:"Message"`
				} `xml:"Conversation"`
			}
			require.NoError(t, xml.Unmarshal(contents, &document))
			for _, channel := range document.Channels {
				for _, post := range channel.Messages {
					if post.Content == message {
						return true
					}
				}
			}
		}
	}
	return false
}

func archiveContainsGlobalRelayMessage(t *testing.T, data []byte, message string) bool {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	for _, entry := range reader.File {
		file, openErr := entry.Open()
		require.NoError(t, openErr)
		contents, readErr := io.ReadAll(file)
		require.NoError(t, readErr)
		require.NoError(t, file.Close())
		if strings.HasSuffix(entry.Name, ".zip") {
			if archiveContainsGlobalRelayMessage(t, contents, message) {
				return true
			}
			continue
		}
		if strings.HasSuffix(entry.Name, ".eml") && bytes.Contains(contents, []byte(message)) {
			return true
		}
	}
	return false
}
