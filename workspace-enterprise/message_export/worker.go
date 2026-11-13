// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.enterprise for license information.

package message_export

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/mattermost/enterprise/message_export/shared"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/mattermost/mattermost/server/v8/channels/utils/fileutils"
	"github.com/mattermost/mattermost/server/v8/platform/shared/templates"
)

const (
	TimeBetweenBatchesMs = 100
)

var errJobNoLongerInProgress = errors.New("message export job is no longer in progress")

// testEndOfBatchCb is only used for testing
var testEndOfBatchCb func(worker *MessageExportWorker)

type MessageExportWorker struct {
	name string
	// stateMut protects stopCh, cancel, and stopped and helps enforce
	// ordering in case subsequent Run or Stop calls are made.
	stateMut            sync.Mutex
	stopCh              chan struct{}
	stopped             bool
	stoppedCh           chan struct{}
	jobs                chan model.Job
	jobServer           *jobs.JobServer
	server              *app.Server
	logger              mlog.LoggerIFace
	htmlTemplateWatcher *templates.Container
	license             func() *model.License

	context context.Context
	cancel  func()

	execution  *messageExportOwner
	recoveryAt int64
	recoveryID string
	reader     *messageExportReader
}

func (dr *MessageExportJobInterfaceImpl) MakeWorker() model.Worker {
	const workerName = "MessageExportWorker"
	logger := dr.Server.Jobs.Logger().With(mlog.String("worker_name", workerName))

	templatesDir, ok := fileutils.FindDir("templates")
	if !ok {
		logger.Error("Failed to initialize HTMLTemplateWatcher, templates directory not found")
		return nil
	}
	htmlTemplateWatcher, err := templates.New(templatesDir)
	if err != nil {
		logger.Error("Failed to initialize HTMLTemplateWatcher", mlog.Err(err))
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &MessageExportWorker{
		name:                workerName,
		stoppedCh:           make(chan struct{}, 1),
		jobs:                make(chan model.Job),
		jobServer:           dr.Server.Jobs,
		server:              dr.Server,
		logger:              logger,
		htmlTemplateWatcher: htmlTemplateWatcher,
		// It is not a best practice to store context inside a struct,
		// however we need to cancel a SQL query during a job execution.
		// There is no other good way.
		context: ctx,
		cancel:  cancel,
		license: dr.Server.License,
		stopped: true,
	}
}

func (w *MessageExportWorker) IsEnabled(cfg *model.Config) bool {
	license := w.license()
	return cfg != nil && license != nil && license.Features != nil &&
		model.SafeDereference(license.Features.MessageExport) &&
		model.SafeDereference(cfg.MessageExportSettings.EnableExport)
}

func (w *MessageExportWorker) Run() {
	w.stateMut.Lock()
	// We have to re-assign the stop channel again, because
	// it might happen that the job was restarted due to a config change.
	if w.stopped {
		w.stopped = false
		w.stopCh = make(chan struct{})
		w.context, w.cancel = context.WithCancel(context.Background())
	} else {
		w.stateMut.Unlock()
		return
	}
	// Run is called from a separate goroutine and doesn't return.
	// So we cannot Unlock in a defer clause.
	w.stateMut.Unlock()

	w.logger.Debug("Worker Started")

	defer func() {
		w.logger.Debug("Worker finished")
		w.stoppedCh <- struct{}{}
	}()

	w.recoverInterruptedJobs()
	recovery := time.NewTicker(30 * time.Second)
	defer recovery.Stop()
	for {
		select {
		case <-recovery.C:
			w.recoverInterruptedJobs()
		case <-w.stopCh:
			w.logger.Debug("Worker: Received stop signal")
			return
		case job := <-w.jobs:
			w.DoJob(&job)
		}
	}
}

func (w *MessageExportWorker) Stop() {
	w.stateMut.Lock()
	defer w.stateMut.Unlock()

	// Set to close, and if already closed before, then return.
	if w.stopped {
		return
	}
	w.stopped = true

	w.logger.Debug("Worker: Stopping")
	w.cancel()
	close(w.stopCh)
	<-w.stoppedCh
}

func (w *MessageExportWorker) JobChannel() chan<- model.Job {
	return w.jobs
}

func (w *MessageExportWorker) DoJob(job *model.Job) {
	logger := w.logger.With(jobs.JobLoggerFields(job)...)
	logger.Debug("Worker: Received a new candidate job.")
	if !w.IsEnabled(w.jobServer.Config()) {
		return
	}
	parent := w.context
	if parent == nil {
		parent = context.Background()
	}
	owner, acquired, err := w.acquireJobOwner(parent, job.Id)
	if err != nil || !acquired {
		if err != nil {
			logger.Warn("Unable to acquire message export job ownership", mlog.Err(err))
		}
		return
	}
	defer owner.Close()
	job, err = owner.claim()
	if err != nil || job == nil {
		if err != nil {
			logger.Warn("Unable to claim message export job", mlog.Err(err))
		}
		return
	}
	w.execution = owner
	if metrics := w.server.GetMetrics(); metrics != nil {
		metrics.IncrementJobActive(job.Type)
		defer metrics.DecrementJobActive(job.Type)
	}
	defer func() {
		if value := recover(); value != nil {
			w.setJobError(logger, job, model.NewAppError("MessageExportWorker.DoJob",
				"ent.message_export.run_export.app_error", nil, "", http.StatusInternalServerError).
				Wrap(fmt.Errorf("message export panic: %v", value)))
		}
		// No work continues after this point. Release ownership before recovery
		// of metadata, using only the last committed cursor for another claim.
		owner.Close()
		w.execution = nil
		if job.Status == model.JobStatusInProgress || job.Status == model.JobStatusCancelRequested {
			w.SetJobPending(logger, job)
		}
	}()
	owner.done = make(chan struct{})
	go owner.monitor(w)
	if err := w.publishJob(job); err != nil {
		logger.Warn("Unable to publish message export claim", mlog.Err(err))
	}
	if !w.IsEnabled(w.jobServer.Config()) {
		return
	}
	rctx := request.EmptyContext(logger).WithContext(owner.ctx)
	reader := newMessageExportReader(owner.ctx, w.jobServer.Config().SqlSettings)
	w.reader = reader
	defer func() { reader.Close(); w.reader = nil }()
	// if job data is missing, we'll do our best to recover
	if err := w.initJobData(rctx, logger, job, time.Now()); err != nil {
		w.setJobError(logger, job, model.NewAppError("initJobData", "ent.message_export.run_export.app_error", nil, "", http.StatusInternalServerError).Wrap(err))
		return
	}
	// Persist bounds and archive path before any metadata reads or file writes.
	// A process crash must resume this exact export period and directory.
	if err := w.updateOwnedJob(job, model.JobStatusInProgress, job.Progress); err != nil {
		logger.Warn("Unable to persist initialized message export job", mlog.Err(err))
		return
	}
	data, err := extractJobData(logger, job.Data)
	if err != nil {
		// Error in conversion. Not much we can do about that. But it shouldn't happen, unless someone edited the db.
		w.setJobError(logger, job, model.NewAppError("Job.DoJob", "ent.message_export.job_data_conversion.app_error", nil, "", http.StatusBadRequest).Wrap(err))
		return
	}

	reportProgress := func(message string) {
		logger.Debug(message)
		// An unavailable owned write cancels the execution context. Later
		// writes must not continue after losing this ownership session.
		w.setJobProgressMessage(0, message, rctx.Logger(), job)
	}

	jobParams := shared.BackendParams{
		Config:        w.jobServer.Config(),
		Store:         &exportReadStore{MessageExportStore: shared.NewMessageExportStore(w.jobServer.Store), reader: reader},
		HtmlTemplates: w.htmlTemplateWatcher,
	}
	jobParams.FileAttachmentBackend, err = shared.GetFileAttachmentBackend(rctx, w.jobServer.Config())
	if err != nil {
		w.setJobError(logger, job, model.NewAppError("GetFileAttachmentBackend", "api.file.no_driver.app_error", nil, "", http.StatusInternalServerError).Wrap(err))
		return
	}
	jobParams.ExportBackend, err = shared.GetExportBackend(rctx, w.jobServer.Config())
	if err != nil {
		w.setJobError(logger, job, model.NewAppError("GetExportBackend", "api.file.no_driver.app_error", nil, "", http.StatusInternalServerError).Wrap(err))
		return
	}

	data, err = shared.GetInitialExportPeriodData(rctx, jobParams.Store, data, reportProgress)
	if err != nil {
		w.setJobError(logger, job, model.NewAppError("DoJob", "ent.message_export.calculate_channel_exports.app_error", nil, "", http.StatusInternalServerError).Wrap(err))
		return
	}
	job.Data[shared.JobDataTotalPostsExpected] = strconv.Itoa(data.TotalPostsExpected)

	for {
		select {
		case <-owner.ctx.Done():
			return

		case <-w.stopCh:
			logger.Debug("Worker: Job has been canceled via Worker Stop. Setting the job back to pending")
			w.SetJobPending(logger, job)
			return

		case <-time.After(TimeBetweenBatchesMs * time.Millisecond):
			if !w.IsEnabled(w.jobServer.Config()) {
				logger.Info("Worker: Message export disabled; returning job to pending")
				w.SetJobPending(logger, job)
				return
			}
			logger.Debug("Starting batch export", mlog.Int("last_post_update_at", data.Cursor.LastPostUpdateAt))

			_, data, err = RunBatch(rctx, data, jobParams)
			if err != nil {
				// We ignore error if the job was explicitly cancelled
				if owner.ctx.Err() != nil {
					logger.Debug("Worker: Job has been canceled via worker's context. Setting the job back to pending")
					w.SetJobPending(logger, job)
				} else {
					w.setJobError(logger, job, model.NewAppError("DoJob", "ent.message_export.run_export.app_error", nil, "", http.StatusInternalServerError).Wrap(err))
				}
				return
			}
			if !w.IsEnabled(w.jobServer.Config()) {
				logger.Info("Worker: Message export disabled after batch; returning job to pending")
				w.SetJobPending(logger, job)
				return
			}

			setJobDataEndOfBatch(job, data)

			if data.Finished {
				w.finishExport(rctx, logger, job, data.WarningCount)
				return
			}

			// also saves job.Data
			if err := w.setJobProgress(logger, job, getJobProgress(data.MessagesExported, data.TotalPostsExpected)); err != nil {
				return
			}

			// testEndOfBatchCb is only used by tests.
			if testEndOfBatchCb != nil {
				testEndOfBatchCb(w)
			}
		}
	}
}

func (w *MessageExportWorker) finishExport(rctx request.CTX, logger *mlog.Logger, job *model.Job, totalWarningCount int) {
	job.Data[shared.JobDataWarningCount] = strconv.Itoa(totalWarningCount)
	// we've exported everything up to the current time
	logger.Debug("FormatExport complete")

	job.Data[shared.JobDataIsDownloadable] = "true"

	if totalWarningCount > 0 {
		w.setJobWarning(logger, job)
	} else {
		w.setJobSuccess(logger, job)
	}
}

// initializes job data if it's missing, allows us to recover from failed or improperly configured jobs
func (w *MessageExportWorker) initJobData(rctx request.CTX, logger mlog.LoggerIFace, job *model.Job, now time.Time) error {
	if job.Data == nil {
		job.Data = make(map[string]string)
	}
	if _, exists := job.Data[shared.JobDataMessagesExported]; !exists {
		logger.Info("Worker: JobDataMessagesExported does not exist, starting at 0")
		job.Data[shared.JobDataMessagesExported] = "0"
	}
	if _, exists := job.Data[shared.JobDataExportType]; !exists {
		exportFormat := *w.jobServer.Config().MessageExportSettings.ExportFormat
		logger.Info("Worker: Defaulting to configured export format", mlog.String("export_format", exportFormat))
		job.Data[shared.JobDataExportType] = exportFormat
	}
	if _, exists := job.Data[shared.JobDataBatchSize]; !exists {
		batchSize := strconv.Itoa(*w.jobServer.Config().MessageExportSettings.BatchSize)
		logger.Info("Worker: Defaulting to configured batch size", mlog.String("batch_size", batchSize))
		job.Data[shared.JobDataBatchSize] = batchSize
	}
	if _, exists := job.Data[shared.JobDataChannelBatchSize]; !exists {
		channelBatchSize := strconv.Itoa(*w.jobServer.Config().MessageExportSettings.ChannelBatchSize)
		logger.Info("Worker: Defaulting to configured channel batch size", mlog.String("channel_batch_size", channelBatchSize))
		job.Data[shared.JobDataChannelBatchSize] = channelBatchSize
	}
	if _, exists := job.Data[shared.JobDataChannelHistoryBatchSize]; !exists {
		channelHistoryBatchSize := strconv.Itoa(*w.jobServer.Config().MessageExportSettings.ChannelHistoryBatchSize)
		logger.Info("Worker: Defaulting to configured channel history batch size", mlog.String("channel_history_batch_size", channelHistoryBatchSize))
		job.Data[shared.JobDataChannelHistoryBatchSize] = channelHistoryBatchSize
	}
	if _, exists := job.Data[shared.JobDataBatchNumber]; !exists {
		logger.Info("Worker: JobDataBatchNumber does not exist, starting at 0")
		job.Data[shared.JobDataBatchNumber] = "0"
	}

	// If this is a new job (JobEndTime doesn't exist), set it to now, because this is when the job has first started.
	// The logic is that a job exports messages up to the moment the job was started. If the job was picked up after
	// gracefully stopping, then run it until that original initial endTime.
	// However, if the job was cancelled or errored out, that job will not be picked up again, so this will be a new job
	// starting from the last successful batchStartTimestamp up until now. This is intentional (for now) because failed
	// jobs do not get rescheduled properly yet, and when they are run again it means that new day's worth of messages
	// need to be exported.
	if _, exists := job.Data[shared.JobDataJobEndTime]; !exists {
		millis := strconv.FormatInt(model.GetMillisForTime(now), 10)
		logger.Info("Worker: JobDataJobEndTime not found in previous job, using now", mlog.String("job_data_job_end_time", millis))
		job.Data[shared.JobDataJobEndTime] = millis
	}

	if _, exists := job.Data[shared.JobDataBatchStartTime]; !exists {
		previousJob, err := w.getPreviousNonCliJob(rctx)

		if err != nil {
			return err
		}
		if previousJob == nil {
			exportFromTimestamp := strconv.FormatInt(*w.jobServer.Config().MessageExportSettings.ExportFromTimestamp, 10)
			logger.Info("Worker: No previously successful job found, falling back to configured MessageExportSettings.ExportFromTimestamp", mlog.String("export_from_timestamp", exportFromTimestamp))
			job.Data[shared.JobDataBatchStartTime] = exportFromTimestamp
			job.Data[shared.JobDataJobStartTime] = exportFromTimestamp
			job.Data[shared.JobDataBatchStartId] = ""
			job.Data[shared.JobDataJobStartId] = job.Data[shared.JobDataBatchStartId]
			job.Data[shared.JobDataExportDir] = getJobExportDir(logger, job.Data, exportFromTimestamp, job.Data[shared.JobDataJobEndTime], job.Id)
			return nil
		}

		logger.Info("Worker: Implicitly resuming export from where previously successful job left off")
		if previousJob == nil {
			previousJob = &model.Job{}
		}
		if previousJob.Data == nil {
			previousJob.Data = make(map[string]string)
		}

		// Backwards compatibility for <10.5:
		if batchStartTimestamp, prevExists := previousJob.Data["batch_start_timestamp"]; prevExists {
			previousJob.Data[shared.JobDataBatchStartTime] = batchStartTimestamp
		}

		if _, prevExists := previousJob.Data[shared.JobDataBatchStartTime]; !prevExists {
			exportFromTimestamp := strconv.FormatInt(*w.jobServer.Config().MessageExportSettings.ExportFromTimestamp, 10)
			logger.Info("Worker: Previously successful job lacks job data, falling back to configured MessageExportSettings.ExportFromTimestamp", mlog.String("export_from_timestamp", exportFromTimestamp))
			job.Data[shared.JobDataBatchStartTime] = exportFromTimestamp
			job.Data[shared.JobDataJobStartTime] = exportFromTimestamp
		} else {
			job.Data[shared.JobDataBatchStartTime] = previousJob.Data[shared.JobDataBatchStartTime]
		}
		if _, prevExists := previousJob.Data[shared.JobDataBatchStartId]; !prevExists {
			logger.Info("Worker: Previously successful job lacks post ID, falling back to empty string")
			job.Data[shared.JobDataBatchStartId] = ""
		} else {
			job.Data[shared.JobDataBatchStartId] = previousJob.Data[shared.JobDataBatchStartId]
		}
		if value, ok := previousJob.Data[shared.JobDataFileDeleteStartTime]; ok {
			job.Data[shared.JobDataFileDeleteStartTime] = value
			job.Data[shared.JobDataFileDeleteStartId] = previousJob.Data[shared.JobDataFileDeleteStartId]
		}
		job.Data[shared.JobDataJobStartId] = job.Data[shared.JobDataBatchStartId]
	} else {
		logger.Info("Worker: JobDataBatchStartTime start time was already set",
			mlog.String(shared.JobDataBatchStartTime, job.Data[shared.JobDataBatchStartTime]))
	}

	if _, exists := job.Data[shared.JobDataJobStartTime]; !exists {
		// Just in case, if we don't have this (JobDataBatchStartTime was already set, but this wasn't) set it:
		job.Data[shared.JobDataJobStartTime] = job.Data[shared.JobDataBatchStartTime]
		logger.Info("Worker: JobDataJobStartTime start time was not set, using batch startTimestamp",
			mlog.String(shared.JobDataJobStartTime, job.Data[shared.JobDataJobStartTime]))
	}

	job.Data[shared.JobDataExportDir] = getJobExportDir(logger, job.Data, job.Data[shared.JobDataJobStartTime], job.Data[shared.JobDataJobEndTime], job.Id)
	return nil
}

// Read the resume cursor from the master. A lagging read replica could return
// an older successful job and cause an already exported period to run again.
func (w *MessageExportWorker) getPreviousNonCliJob(rctx request.CTX) (*model.Job, error) {
	var id string
	var raw []byte
	read := func(ctx context.Context, db *sql.DB) error {
		return db.QueryRowContext(ctx,
			`SELECT Id, Data FROM Jobs
		 WHERE Type=$1 AND Status IN ($2, $3)
		 AND COALESCE(Data::jsonb->>$4, '') <> 'mmctl'
		 ORDER BY CreateAt DESC, Id DESC LIMIT 1`,
			model.JobTypeMessageExport, model.JobStatusWarning, model.JobStatusSuccess,
			shared.JobDataInitiatedBy).Scan(&id, &raw)
	}
	var err error
	if w.reader != nil {
		err = w.reader.query(rctx.Context(), read)
	} else {
		err = read(rctx.Context(), w.jobServer.Store.GetInternalMasterDB())
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var data model.StringMap
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	return &model.Job{Id: id, Data: data}, nil
}

func extractJobData(logger *mlog.Logger, strmap map[string]string) (shared.JobData, error) {
	data, err := shared.StringMapToJobDataWithZeroValues(strmap)
	if err != nil {
		return data, err
	}

	// ExportPeriodStartTime is initialized to BatchStartTime because this is where we will start exporting. But unlike
	// BatchStartTime, it won't change as we process the batches.
	// If this is the first time this job has run, BatchStartTime will be the start of the entire job. If this job has
	// been resumed, then BatchStartTime will be the start of the newest batch. This is expected--the channel activity
	// and total posts will be calculated from ExportPeriodStartTime (anything earlier has already been exported in
	// previous batches).
	// Note: ExportPeriodStartTime is different from JobStartTime because JobStartTime won't change
	//	     if the job processes some batches, is stopped, and picked up again.
	data.ExportPeriodStartTime = data.BatchStartTime

	logger.Info("Worker: initial job variables set",
		mlog.String("export_type", data.ExportType),
		mlog.String("export_dir", data.ExportDir),
		mlog.Int("job_start_time", data.JobStartTime),
		mlog.Int("batch_start_time", data.BatchStartTime),
		mlog.Int("export_period_start_time", data.ExportPeriodStartTime),
		mlog.Int("job_end_time", data.JobEndTime),
		mlog.String("job_start_id", data.JobStartId),
		mlog.Int("batch_size", data.BatchSize),
		mlog.Int("channel_batch_size", data.ChannelBatchSize),
		mlog.Int("channel_history_batch_size", data.ChannelHistoryBatchSize),
		mlog.Int("batch_number", data.BatchNumber),
		mlog.Int("total_posts_exported", data.MessagesExported))

	return data, err
}

func setJobDataEndOfBatch(job *model.Job, data shared.JobData) {
	job.Data[shared.JobDataBatchStartTime] = strconv.FormatInt(data.BatchStartTime, 10)
	job.Data[shared.JobDataBatchStartId] = data.Cursor.LastPostId
	job.Data[shared.JobDataFileDeleteStartTime] = strconv.FormatInt(data.FileDeleteStartTime, 10)
	job.Data[shared.JobDataFileDeleteStartId] = data.FileDeleteStartId
	job.Data[shared.JobDataMessagesExported] = strconv.Itoa(data.MessagesExported)
	job.Data[shared.JobDataBatchNumber] = strconv.Itoa(data.BatchNumber)
}

// getJobExportDir will use the existing JobDataExportDir if available. If it's not available, this is the first run
// for the job, so we use the startTime and endTime passed in.
func getJobExportDir(logger mlog.LoggerIFace, data model.StringMap, startTime string, endTime string, jobID string) string {
	exportDir := data[shared.JobDataExportDir]
	if exportDir == "" {
		// The job ID separates overlapping exports of the same time interval.
		exportDir = path.Join(model.ComplianceExportPath, fmt.Sprintf("%s-%s-%s-%s", time.Now().Format(model.ComplianceExportDirectoryFormat), startTime, endTime, jobID))
		logger.Info("Worker: JobDataExportDir does not exist, using current datetime", mlog.String("job_data_export_dir", exportDir))
	}

	return exportDir
}

func getJobProgress(totalExportedPosts, totalPostsExpected int) int {
	if totalPostsExpected == 0 {
		return 0 // a file-deletion-only job has no posts in its progress estimate
	}
	return totalExportedPosts * 100 / totalPostsExpected
}

func (w *MessageExportWorker) setJobProgressMessage(progress int64, message string, logger mlog.LoggerIFace, job *model.Job) {
	job.Status = model.JobStatusInProgress
	job.Progress = progress
	if job.Data == nil {
		job.Data = make(map[string]string)
	}
	job.Data["progress_message"] = message

	if job.Data[messageExportAttemptKey] != "" {
		if err := w.updateOwnedJob(job, model.JobStatusInProgress, progress); err != nil {
			logger.Warn("Unable to save owned message export progress message", mlog.Err(err))
		}
		return
	}
	if _, err := w.jobServer.Store.Job().UpdateOptimistically(job, model.JobStatusInProgress); err != nil {
		logger.Error("Worker: Failed to update progress for job", mlog.Err(err))
	}
}

func (w *MessageExportWorker) setJobProgress(logger mlog.LoggerIFace, job *model.Job, progress int) error {
	if job.Data != nil {
		job.Data["progress_message"] = ""
	}

	if job.Data[messageExportAttemptKey] != "" {
		if err := w.updateOwnedJob(job, model.JobStatusInProgress, int64(progress)); err != nil {
			w.SetJobPending(logger, job)
			return err
		}
		return nil
	}
	if err := w.jobServer.SetJobProgress(job, int64(progress)); err != nil {
		logger.Error("Worker: Failed to update progress for job", mlog.Err(err))
		// A failed progress write leaves the previous cursor intact. Retry the
		// batch under a new claim instead of treating a transient store failure
		// as a permanent export failure.
		w.SetJobPending(logger, job)
		return err
	}
	// The core returns nil even when its optimistic update affects no row.
	// Check on the write database: GetJob reads a replica, which may lag behind
	// a progress update or cancellation request.
	current, err := w.jobServer.Store.Job().UpdateStatusOptimistically(job.Id, model.JobStatusInProgress, model.JobStatusInProgress)
	if err != nil {
		logger.Error("Worker: Failed to verify job after progress update", mlog.Err(err))
		// The last successfully saved cursor remains in the job. Return it to
		// pending for a new worker, or finish an intervening cancellation.
		w.SetJobPending(logger, job)
		return err
	}
	if current == nil {
		logger.Info("Worker: Job status changed during progress update", mlog.String("job_id", job.Id))
		w.SetJobPending(logger, job)
		return errJobNoLongerInProgress
	}

	return nil
}

func (w *MessageExportWorker) setJobSuccess(logger mlog.LoggerIFace, job *model.Job) {
	w.finishJob(logger, job, model.JobStatusSuccess)
}

func (w *MessageExportWorker) setJobWarning(logger mlog.LoggerIFace, job *model.Job) {
	w.finishJob(logger, job, model.JobStatusWarning)
}

func (w *MessageExportWorker) finishJob(logger mlog.LoggerIFace, job *model.Job, status string) {
	if job.Data != nil {
		job.Data["progress_message"] = ""
	}
	if job.Data[messageExportAttemptKey] != "" {
		if err := w.updateOwnedJob(job, status, 100); err != nil {
			logger.Warn("Unable to finish owned message export job", mlog.Err(err))
		}
		return
	}
	job.Status = status
	job.Progress = 100
	// Persist the final cursor and status in one compare-and-swap. A cancellation
	// request changes the status away from in_progress and wins this race.
	updated, err := w.jobServer.Store.Job().UpdateOptimistically(job, model.JobStatusInProgress)
	if err != nil {
		appErr := model.NewAppError("MessageExportWorker.finishJob", "app.job.update.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		w.setJobError(logger, job, appErr)
		return
	}
	if updated == nil {
		logger.Info("Worker: Job status changed before completion", mlog.String("job_id", job.Id))
		return
	}
	// The core method publishes the terminal status and updates job metrics.
	// Cancellation can no longer claim the job after the CAS above.
	var appErr *model.AppError
	if status == model.JobStatusWarning {
		appErr = w.jobServer.SetJobWarning(job)
	} else {
		appErr = w.jobServer.SetJobSuccess(job)
	}
	if appErr != nil {
		logger.Error("Worker: Failed to publish completed job", mlog.Err(appErr))
	}
}

func (w *MessageExportWorker) setJobError(logger mlog.LoggerIFace, job *model.Job, appError *model.AppError) {
	if job.Data != nil {
		job.Data["progress_message"] = ""
	}
	if job.Data[messageExportAttemptKey] != "" {
		if w.execution == nil || w.execution.ctx.Err() != nil {
			return
		}
		job.Data["error"] = appError.Message
		if appError.DetailedError != "" {
			job.Data["error"] += " — " + appError.DetailedError
		}
		if wrapped := appError.Unwrap(); wrapped != nil {
			job.Data["error"] += " — " + wrapped.Error()
		}
		if err := w.updateOwnedJob(job, model.JobStatusError, -1); err != nil {
			logger.Warn("Unable to save owned message export failure", mlog.Err(err))
		}
		return
	}
	logger.Error("Worker: Job error", mlog.Err(appError))
	if err := w.jobServer.SetJobError(job, appError); err != nil {
		logger.Error("Worker: Failed to set job error", mlog.Err(err), mlog.NamedErr("set_error", appError))
	}
}

func (w *MessageExportWorker) setJobCanceled(logger mlog.LoggerIFace, job *model.Job) {
	if job.Data != nil {
		job.Data["progress_message"] = ""
	}
	if _, err := w.transitionInterruptedJob(job, model.JobStatusCancelRequested, model.JobStatusCanceled); err != nil {
		logger.Error("Worker: Failed to mark job as canceled", mlog.Err(err))
	}
}

func (w *MessageExportWorker) SetJobPending(logger mlog.LoggerIFace, job *model.Job) {
	if job.Data != nil {
		job.Data["progress_message"] = ""
	}
	returnedToPending, err := w.transitionInterruptedJob(job, model.JobStatusInProgress, model.JobStatusPending)
	if err != nil {
		logger.Error("Worker: Failed to mark job as pending", mlog.Err(err))
		return
	}
	if returnedToPending {
		return
	}
	if _, err := w.transitionInterruptedJob(job, model.JobStatusCancelRequested, model.JobStatusCanceled); err != nil {
		logger.Error("Worker: Failed to finish canceled job", mlog.Err(err))
	}
}

func (w *MessageExportWorker) transitionInterruptedJob(job *model.Job, from, to string) (bool, error) {
	if job.Data[messageExportAttemptKey] != "" {
		return w.transitionClaim(job, from, to)
	}
	updated, err := w.jobServer.Store.Job().UpdateStatusOptimistically(job.Id, from, to)
	if err != nil || updated == nil {
		return false, err
	}
	if metrics := w.server.GetMetrics(); metrics != nil {
		metrics.DecrementJobActive(updated.Type)
	}
	jobJSON, err := json.Marshal(updated)
	if err != nil {
		return true, err
	}
	message := model.NewWebSocketEvent(model.WebsocketEventJobUpdated, "", "", "", nil, "")
	message.Add("job", string(jobJSON))
	message.GetBroadcast().ContainsSensitiveData = true
	message.GetBroadcast().RequiredPermissions = []string{model.PermissionReadComplianceExportJob.Id}
	w.server.Platform().Publish(message)
	return true, nil
}
