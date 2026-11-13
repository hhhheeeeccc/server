package data_retention_job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	ejobs "github.com/mattermost/mattermost/server/v8/einterfaces/jobs"
)

const workerName = "WorkspaceDataRetention"

// Shared across all nodes using the same PostgreSQL database. The session lock
// and deletion statements must use the same backend for the entire execution.
const retentionExecutionLock int64 = 6492261406

type Builder struct {
	jobServer    *jobs.JobServer
	app          *app.App
	databaseDSN  string
	queryTimeout time.Duration
}

type Worker struct {
	jobServer    *jobs.JobServer
	app          *app.App
	jobs         chan model.Job
	stop         chan bool
	stopped      chan bool
	lifecycleMu  sync.Mutex
	running      bool
	logger       mlog.LoggerIFace
	databaseDSN  string
	queryTimeout time.Duration
}

type stopReason uint8

const (
	continueWork stopReason = iota
	jobCanceled
	workerStopped
)

var _ ejobs.DataRetentionJobInterface = (*Builder)(nil)
var _ model.Worker = (*Worker)(nil)

func New(server *app.Server) ejobs.DataRetentionJobInterface {
	application := app.New(app.ServerConnector(server.Channels()))
	// Follow the database selected at startup, rather than a saved DSN that
	// has not yet been applied to the running application's store.
	settings := application.Config().Clone().SqlSettings
	return &Builder{
		jobServer:    server.Jobs,
		app:          application,
		databaseDSN:  model.SafeDereference(settings.DataSource),
		queryTimeout: time.Duration(model.SafeDereference(settings.QueryTimeout)) * time.Second,
	}
}

func (b *Builder) MakeWorker() model.Worker {
	return &Worker{
		jobServer:    b.jobServer,
		app:          b.app,
		jobs:         make(chan model.Job),
		stop:         make(chan bool, 1),
		stopped:      make(chan bool, 1),
		logger:       b.jobServer.Logger().With(mlog.String("worker_name", workerName)),
		databaseDSN:  b.databaseDSN,
		queryTimeout: b.queryTimeout,
	}
}

func (b *Builder) MakeScheduler() ejobs.Scheduler {
	return jobs.NewDailyScheduler(b.jobServer, model.JobTypeDataRetention, deletionStartTime, retentionEnabled)
}

func (w *Worker) Run() {
	w.lifecycleMu.Lock()
	if w.running {
		w.lifecycleMu.Unlock()
		return
	}
	select {
	case <-w.stopped:
		w.stop = make(chan bool, 1)
		w.stopped = make(chan bool, 1)
	default:
	}
	w.running = true
	w.lifecycleMu.Unlock()

	w.logger.Debug("Worker started")
	defer func() {
		w.logger.Debug("Worker finished")
		w.lifecycleMu.Lock()
		w.running = false
		close(w.stopped)
		w.lifecycleMu.Unlock()
	}()
	w.recoverInterruptedJobs()
	recoveryTick := time.NewTicker(30 * time.Second)
	defer recoveryTick.Stop()

	for {
		select {
		case <-w.stop:
			return
		case <-recoveryTick.C:
			w.recoverInterruptedJobs()
		case job := <-w.jobs:
			if w.doJob(&job) {
				return
			}
		}
	}
}

func (w *Worker) Stop() {
	w.logger.Debug("Worker stopping")
	w.lifecycleMu.Lock()
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	stopped := w.stopped
	w.lifecycleMu.Unlock()
	<-stopped
}

func (w *Worker) JobChannel() chan<- model.Job {
	return w.jobs
}

func (w *Worker) IsEnabled(cfg *model.Config) bool {
	return retentionEnabled(cfg)
}

func (w *Worker) doJob(job *model.Job) bool {
	logger := w.logger.With(jobs.JobLoggerFields(job)...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelRequested := make(chan struct{}, 1)
	rctx := request.EmptyContext(logger).WithContext(ctx)
	go w.jobServer.CancellationWatcher(rctx, job.Id, cancelRequested)

	// Claim and deletion share the session lock. A lost session must not claim
	// a job through the application's independently reconnecting SQL pool.
	execution, reason, err := w.acquireExecutionLock(rctx, cancelRequested)
	if reason != continueWork {
		return reason == workerStopped
	}
	if err != nil {
		logger.Warn("Unable to acquire retention job session", mlog.Err(err))
		return false
	}
	defer execution.Close()
	claimedJob, err := execution.claimJob(job.Id)
	if err != nil {
		logger.Warn("Unable to claim data retention job", mlog.Err(err))
		return false
	}
	if claimedJob == nil {
		return false
	}
	job = claimedJob
	defer w.handleJobPanic(logger, job)
	if metrics := w.app.Srv().GetMetrics(); metrics != nil {
		metrics.IncrementJobActive(job.Type)
		defer metrics.DecrementJobActive(job.Type)
	}
	if err := w.publishJobStatus(job); err != nil {
		logger.Error("Unable to publish claimed retention job", mlog.Err(err))
	}

	reason, err = w.executeLocked(rctx, logger, cancelRequested, execution)
	if reason == continueWork {
		reason = w.wait(cancelRequested, 0)
	}
	if reason != continueWork {
		return w.finishInterruption(logger, job, reason)
	}
	if err != nil {
		jobErr := model.NewAppError("WorkspaceDataRetention", "app.job.error", nil, "", http.StatusInternalServerError).Wrap(err)
		w.finishError(logger, job, jobErr)
		return false
	}

	if err := w.setJobProgress(job, 100); err != nil {
		logger.Error("Unable to set data retention job progress", mlog.Err(err))
		w.finishError(logger, job, model.NewAppError("WorkspaceDataRetention", "app.job.error", nil, "", http.StatusInternalServerError).Wrap(err))
		return false
	}
	finished, finishErr := w.transitionJob(job, model.JobStatusInProgress, model.JobStatusSuccess)
	if finishErr != nil {
		logger.Error("Unable to mark data retention job as successful", mlog.Err(finishErr))
	} else if !finished {
		if _, cancelErr := w.transitionJob(job, model.JobStatusCancelRequested, model.JobStatusCanceled); cancelErr != nil {
			logger.Error("Unable to finish canceled data retention job", mlog.Err(cancelErr))
		}
	}
	return false
}

func (w *Worker) handleJobPanic(logger mlog.LoggerIFace, job *model.Job) {
	if recovered := recover(); recovered != nil {
		logger.Error("Unhandled panic in retention job", mlog.Any("panic", recovered), mlog.String("stack", string(debug.Stack())))
		w.finishError(logger, job, model.NewAppError("WorkspaceDataRetention", "app.job.error", nil, "job panicked", http.StatusInternalServerError))
		panic(recovered)
	}
}

func (w *Worker) finishInterruption(logger mlog.LoggerIFace, job *model.Job, reason stopReason) bool {
	if reason == workerStopped {
		pending, err := w.transitionJob(job, model.JobStatusInProgress, model.JobStatusPending)
		if err != nil {
			logger.Error("Unable to return stopped data retention job to pending", mlog.Err(err))
			return true
		}
		if pending {
			return true
		}
	}
	if _, err := w.transitionJob(job, model.JobStatusCancelRequested, model.JobStatusCanceled); err != nil {
		logger.Error("Unable to mark data retention job as canceled", mlog.Err(err))
	}
	return reason == workerStopped
}

func (w *Worker) finishError(logger mlog.LoggerIFace, job *model.Job, jobErr *model.AppError) {
	job.Status = model.JobStatusError
	job.Progress = -1
	if job.Data == nil {
		job.Data = model.StringMap{}
	}
	job.Data["error"] = jobErr.Message
	if jobErr.DetailedError != "" {
		job.Data["error"] += " — " + jobErr.DetailedError
	}
	if wrapped := jobErr.Unwrap(); wrapped != nil {
		job.Data["error"] += " — " + wrapped.Error()
	}
	updated, err := w.updateJobAttempt(job, model.JobStatusInProgress, model.JobStatusError, job.Progress, job.Data)
	if err != nil {
		logger.Error("Unable to mark data retention job as failed", mlog.Err(err))
		return
	}
	if updated != nil {
		if err := w.publishJobStatus(updated); err != nil {
			logger.Error("Unable to publish failed data retention job", mlog.Err(err))
		}
		return
	}
	if _, err := w.transitionJob(job, model.JobStatusCancelRequested, model.JobStatusCanceled); err != nil {
		logger.Error("Unable to finish canceled data retention job", mlog.Err(err))
	}
}

func (w *Worker) transitionJob(job *model.Job, from, to string) (bool, error) {
	var updated *model.Job
	var err error
	if job.Data[retentionAttemptKey] == "" {
		updated, err = w.jobServer.Store.Job().UpdateStatusOptimistically(job.Id, from, to)
	} else {
		updated, err = w.updateJobAttempt(job, from, to, job.Progress, job.Data)
	}
	if err != nil || updated == nil {
		return false, err
	}
	return true, w.publishJobStatus(updated)
}

func (w *Worker) publishJobStatus(job *model.Job) error {
	jobJSON, err := json.Marshal(job)
	if err != nil {
		return err
	}
	message := model.NewWebSocketEvent(model.WebsocketEventJobUpdated, "", "", "", nil, "")
	message.Add("job", string(jobJSON))
	message.GetBroadcast().ContainsSensitiveData = true
	message.GetBroadcast().RequiredPermissions = []string{model.PermissionReadDataRetentionJob.Id}
	w.app.Publish(message)
	return nil
}

func (w *Worker) execute(rctx request.CTX, logger mlog.LoggerIFace, cancelRequested <-chan struct{}) (stopReason, error) {
	if err := w.requireLicense(); err != nil {
		return continueWork, err
	}
	execution, reason, err := w.acquireExecutionLock(rctx, cancelRequested)
	if reason != continueWork || err != nil {
		return reason, err
	}
	defer execution.Close()
	return w.executeLocked(rctx, logger, cancelRequested, execution)
}

func (w *Worker) executeLocked(rctx request.CTX, logger mlog.LoggerIFace, cancelRequested <-chan struct{}, execution *retentionExecution) (stopReason, error) {
	if err := w.requireLicense(); err != nil {
		return continueWork, err
	}

	policy, appErr := w.app.GetGlobalRetentionPolicy()
	if appErr != nil {
		return continueWork, appErr
	}

	cfg := w.app.Config()
	batchConfig := makeBatchConfig(cfg, policy, model.GetMillis())
	delay := time.Duration(model.SafeDereference(cfg.DataRetentionSettings.TimeBetweenBatchesMilliseconds)) * time.Millisecond

	if policy.MessageDeletionEnabled {
		reason, err := w.runPolicyBatches(logger, cancelRequested, delay, "posts", batchConfig, execution.postBatch)
		if reason != continueWork || err != nil {
			return reason, err
		}
		reason, err = w.deleteOrphanedReactions(cancelRequested, delay, model.SafeDereference(cfg.DataRetentionSettings.RetentionIdsBatchSize), execution)
		if reason != continueWork || err != nil {
			return reason, err
		}
		if reason = w.wait(cancelRequested, 0); reason != continueWork {
			return reason, nil
		}
		if err := w.requireLicense(); err != nil {
			return continueWork, err
		}
		if err = execution.deleteOrphanedPreferences(int(batchConfig.Limit)); err != nil {
			return continueWork, fmt.Errorf("delete orphaned preferences: %w", err)
		}

		reason, err = w.runPolicyBatches(logger, cancelRequested, delay, "threads", batchConfig, execution.threadBatch)
		if reason != continueWork || err != nil {
			return reason, err
		}
		reason, err = w.runPolicyBatches(logger, cancelRequested, delay, "thread memberships", batchConfig, execution.membershipBatch)
		if reason != continueWork || err != nil {
			return reason, err
		}
		reason, err = w.deleteOrphanedMemberships(cancelRequested, delay, batchConfig.Limit, execution)
		if reason != continueWork || err != nil {
			return reason, err
		}
		reason, err = w.runPolicyBatches(logger, cancelRequested, delay, "channel member history", batchConfig, execution.historyBatch)
		if reason != continueWork || err != nil {
			return reason, err
		}
		if reason = w.wait(cancelRequested, 0); reason != continueWork {
			return reason, nil
		}
		if err := w.requireLicense(); err != nil {
			return continueWork, err
		}
		if err = execution.deleteOrphanedPolicies(int(batchConfig.Limit)); err != nil {
			return continueWork, fmt.Errorf("delete orphaned retention policy rows: %w", err)
		}
	}

	if policy.FileDeletionEnabled {
		return w.deleteExpiredFiles(rctx, logger, cancelRequested, delay, policy.FileRetentionCutoff, int(batchConfig.Limit), execution)
	}
	return continueWork, nil
}

func (w *Worker) requireLicense() error {
	license := w.app.License()
	if license == nil || license.Features == nil || !model.SafeDereference(license.Features.DataRetention) {
		return fmt.Errorf("data retention is not enabled by the installed license")
	}
	return nil
}

func (w *Worker) acquireExecutionLock(rctx request.CTX, cancelRequested <-chan struct{}) (*retentionExecution, stopReason, error) {
	if reason := w.wait(cancelRequested, 0); reason != continueWork {
		return nil, reason, nil
	}
	execution, err := newRetentionExecution(rctx.Context(), w.databaseDSN, w.queryTimeout)
	if err != nil {
		return nil, continueWork, err
	}
	acquired := false
	defer func() {
		if !acquired {
			execution.Close()
		}
	}()

	// Cancellation while waiting must not cancel later metadata cleanup after
	// removal of an object has already begun. Stop the watcher before returning.
	waitCtx, cancel := context.WithCancel(rctx.Context())
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-cancelRequested:
			cancel()
		case <-w.stop:
			cancel()
		case <-waitCtx.Done():
		}
	}()
	defer func() { cancel(); <-watcherDone }()
	for {
		if reason := w.wait(cancelRequested, 0); reason != continueWork {
			return nil, reason, nil
		}
		queryCtx, stopQuery := execution.queryContextFor(waitCtx)
		err = execution.db.QueryRowContext(queryCtx, `SELECT pg_try_advisory_lock($1)`, retentionExecutionLock).Scan(&acquired)
		stopQuery()
		if err != nil {
			if reason := w.wait(cancelRequested, 0); reason != continueWork {
				return nil, reason, nil
			}
			return nil, continueWork, fmt.Errorf("acquire retention execution session: %w", err)
		}
		if acquired {
			return execution, continueWork, nil
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if reason := w.wait(cancelRequested, 0); reason != continueWork {
				return nil, reason, nil
			}
			return nil, continueWork, waitCtx.Err()
		case <-timer.C:
		}
	}
}

type policyBatchFunc func(model.RetentionPolicyBatchConfigs, model.RetentionPolicyCursor) (int64, model.RetentionPolicyCursor, error)

func (w *Worker) runPolicyBatches(logger mlog.LoggerIFace, cancelRequested <-chan struct{}, delay time.Duration, name string, cfg model.RetentionPolicyBatchConfigs, deleteBatch policyBatchFunc) (stopReason, error) {
	cursor := model.RetentionPolicyCursor{}
	for {
		if reason := w.wait(cancelRequested, 0); reason != continueWork {
			return reason, nil
		}
		if err := w.requireLicense(); err != nil {
			return continueWork, err
		}

		count, nextCursor, err := deleteBatch(cfg, cursor)
		if err != nil {
			return continueWork, fmt.Errorf("delete retained %s: %w", name, err)
		}
		logger.Info("Deleted data retention batch", mlog.String("record_type", name), mlog.Int("count", int(count)))
		if retentionCursorDone(nextCursor) {
			return continueWork, nil
		}
		if count == 0 && nextCursor == cursor {
			return continueWork, fmt.Errorf("data retention %s cursor made no progress", name)
		}
		cursor = nextCursor
		if reason := w.wait(cancelRequested, delay); reason != continueWork {
			return reason, nil
		}
	}
}

func (w *Worker) deleteOrphanedReactions(cancelRequested <-chan struct{}, delay time.Duration, limit int, execution *retentionExecution) (stopReason, error) {
	if limit <= 0 {
		limit = model.DataRetentionSettingsDefaultRetentionIdsBatchSize
	}
	for {
		if reason := w.wait(cancelRequested, 0); reason != continueWork {
			return reason, nil
		}
		if err := w.requireLicense(); err != nil {
			return continueWork, err
		}
		rows, err := execution.reactionIDs(limit)
		if err != nil {
			return continueWork, fmt.Errorf("get retained post ids: %w", err)
		}
		if len(rows) == 0 {
			return continueWork, nil
		}
		for _, row := range rows {
			if reason := w.wait(cancelRequested, 0); reason != continueWork {
				return reason, nil
			}
			if err := w.requireLicense(); err != nil {
				return continueWork, err
			}
			if err = execution.deleteReactions(row); err != nil {
				return continueWork, fmt.Errorf("delete orphaned reactions: %w", err)
			}
		}
		if reason := w.wait(cancelRequested, delay); reason != continueWork {
			return reason, nil
		}
	}
}

func (w *Worker) deleteOrphanedMemberships(cancelRequested <-chan struct{}, delay time.Duration, limit int64, execution *retentionExecution) (stopReason, error) {
	if limit <= 0 {
		limit = model.DataRetentionSettingsDefaultBatchSize
	}
	for {
		if reason := w.wait(cancelRequested, 0); reason != continueWork {
			return reason, nil
		}
		if err := w.requireLicense(); err != nil {
			return continueWork, err
		}
		count, err := execution.orphanedMembershipBatch(limit)
		if err != nil {
			return continueWork, fmt.Errorf("delete orphaned thread memberships: %w", err)
		}
		if count < limit {
			return continueWork, nil
		}
		if reason := w.wait(cancelRequested, delay); reason != continueWork {
			return reason, nil
		}
	}
}

func (w *Worker) deleteExpiredFiles(rctx request.CTX, logger mlog.LoggerIFace, cancelRequested <-chan struct{}, delay time.Duration, cutoff int64, limit int, execution *retentionExecution) (stopReason, error) {
	if cutoff <= 0 {
		return continueWork, nil
	}
	if limit <= 0 {
		limit = model.DataRetentionSettingsDefaultBatchSize
	}

	for {
		if reason := w.wait(cancelRequested, 0); reason != continueWork {
			return reason, nil
		}
		if err := w.requireLicense(); err != nil {
			return continueWork, err
		}
		fileIDs, err := execution.expiredFileIDs(cutoff, limit)
		if err != nil {
			return continueWork, fmt.Errorf("list files for retention: %w", err)
		}

		deleted := 0
		for _, fileID := range fileIDs {
			if reason := w.wait(cancelRequested, 0); reason != continueWork {
				return reason, nil
			}
			if err := w.requireLicense(); err != nil {
				return continueWork, err
			}
			file, err := w.jobServer.Store.FileInfo().GetFromMaster(fileID)
			if err != nil {
				return continueWork, fmt.Errorf("load retained file %s: %w", fileID, err)
			}
			if reason := w.wait(cancelRequested, 0); reason != continueWork {
				return reason, nil
			}
			if err := w.requireLicense(); err != nil {
				return continueWork, err
			}
			// A session check prevents starting another object deletion after
			// an observed SQL failure. External storage operations already in
			// flight cannot be fenced by a PostgreSQL session lock.
			if err := execution.checkSession(); err != nil {
				return continueWork, fmt.Errorf("check retention execution before object deletion: %w", err)
			}
			removed, removalErr := w.removeRetainedFile(rctx, file)
			if !removed {
				return continueWork, fmt.Errorf("remove retained file %s: %w", file.Id, removalErr)
			}
			if err = w.completeDeletedFile(execution, file); err != nil {
				return continueWork, errors.Join(removalErr, fmt.Errorf("delete retained file metadata %s: %w", file.Id, err))
			}
			if removalErr != nil {
				return continueWork, fmt.Errorf("retained file %s removal confirmed after a storage error; execution stopped: %w", file.Id, removalErr)
			}
			deleted++
		}
		logger.Info("Deleted data retention batch", mlog.String("record_type", "files"), mlog.Int("count", deleted))
		if deleted < limit {
			return continueWork, nil
		}
		if reason := w.wait(cancelRequested, delay); reason != continueWork {
			return reason, nil
		}
	}
}

func (w *Worker) wait(cancelRequested <-chan struct{}, delay time.Duration) stopReason {
	if delay <= 0 {
		select {
		case <-cancelRequested:
			return jobCanceled
		case <-w.stop:
			return workerStopped
		default:
			return continueWork
		}
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-cancelRequested:
		return jobCanceled
	case <-w.stop:
		return workerStopped
	case <-timer.C:
		return continueWork
	}
}

func retentionEnabled(cfg *model.Config) bool {
	if cfg == nil {
		return false
	}
	settings := cfg.DataRetentionSettings
	return model.SafeDereference(settings.EnableMessageDeletion) || model.SafeDereference(settings.EnableFileDeletion)
}

func deletionStartTime(cfg *model.Config) *time.Time {
	if cfg == nil || cfg.DataRetentionSettings.DeletionJobStartTime == nil {
		return nil
	}
	start, err := time.Parse("15:04", *cfg.DataRetentionSettings.DeletionJobStartTime)
	if err != nil {
		return nil
	}
	return &start
}

func makeBatchConfig(cfg *model.Config, policy *model.GlobalRetentionPolicy, now int64) model.RetentionPolicyBatchConfigs {
	batchSize := model.DataRetentionSettingsDefaultBatchSize
	preservePinnedPosts := false
	if cfg != nil {
		if configuredBatchSize := model.SafeDereference(cfg.DataRetentionSettings.BatchSize); configuredBatchSize > 0 {
			batchSize = configuredBatchSize
		}
		preservePinnedPosts = model.SafeDereference(cfg.DataRetentionSettings.PreservePinnedPosts)
	}

	batchConfig := model.RetentionPolicyBatchConfigs{
		Limit:               int64(batchSize),
		PreservePinnedPosts: preservePinnedPosts,
	}
	if policy != nil && policy.MessageDeletionEnabled {
		batchConfig.Now = now
		batchConfig.GlobalPolicyEndTime = policy.MessageRetentionCutoff
	}
	return batchConfig
}

func retentionCursorDone(cursor model.RetentionPolicyCursor) bool {
	return cursor.ChannelPoliciesDone && cursor.TeamPoliciesDone && cursor.GlobalPoliciesDone
}
