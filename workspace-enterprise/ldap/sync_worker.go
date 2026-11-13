package ldap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
)

const syncCancellationPollInterval = 250 * time.Millisecond

type syncWorker struct {
	service   *Service
	jobServer *jobs.JobServer
	logger    mlog.LoggerIFace
	enabled   func(*model.Config) bool
	jobs      chan model.Job
	stop      chan struct{}
	stopped   chan struct{}
	mu        sync.Mutex
	running   bool
	cancel    context.CancelFunc
}

var _ model.Worker = (*syncWorker)(nil)

func newSyncWorker(service *Service, enabled func(*model.Config) bool) *syncWorker {
	server := service.app.Srv().Jobs
	return &syncWorker{
		service: service, jobServer: server, enabled: enabled,
		logger: server.Logger().With(mlog.String("worker_name", "WorkspaceLdapSync")),
		jobs:   make(chan model.Job), stop: make(chan struct{}), stopped: make(chan struct{}),
	}
}

func (w *syncWorker) Run() {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	select {
	case <-w.stopped:
		w.stop = make(chan struct{})
		w.stopped = make(chan struct{})
	default:
	}
	w.running = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running = false
		close(w.stopped)
		w.mu.Unlock()
	}()
	for {
		select {
		case <-w.stop:
			return
		case job := <-w.jobs:
			w.DoJob(&job)
		}
	}
}

func (w *syncWorker) Stop() {
	w.mu.Lock()
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	running := w.running
	stopped := w.stopped
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
	if running {
		<-stopped
	}
}

func (w *syncWorker) JobChannel() chan<- model.Job     { return w.jobs }
func (w *syncWorker) IsEnabled(cfg *model.Config) bool { return w.enabled(cfg) }

func (w *syncWorker) DoJob(job *model.Job) {
	logger := w.logger.With(jobs.JobLoggerFields(job)...)
	defer w.jobServer.HandleJobPanic(logger, job)
	claimed, appErr := w.jobServer.ClaimJob(job)
	if appErr != nil {
		logger.Warn("Unable to claim LDAP sync job", mlog.Err(appErr))
		return
	}
	if claimed == nil {
		return
	}
	job = claimed
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	w.mu.Lock()
	w.cancel = cancel
	select {
	case <-w.stop:
		cancel()
	default:
	}
	w.mu.Unlock()
	defer func() {
		cancel()
		w.mu.Lock()
		w.cancel = nil
		w.mu.Unlock()
	}()
	var requested atomic.Bool
	watchDone := make(chan struct{})
	go w.watchCancellation(ctx, job.Id, &requested, cancel, watchDone)
	defer close(watchDone)

	reAdd := model.SafeDereference(w.service.app.Config().LdapSettings.ReAddRemovedMembers) || job.Data["include_removed_members"] == "true"
	appErr = w.service.synchronize(request.EmptyContext(logger).WithContext(ctx), reAdd)

	if requested.Load() {
		if _, err := w.transitionJob(job, model.JobStatusCancelRequested, model.JobStatusCanceled); err != nil {
			logger.Error("Unable to mark LDAP sync job canceled", mlog.Err(err))
		}
		return
	}
	select {
	case <-w.stop:
		pending, err := w.transitionJob(job, model.JobStatusInProgress, model.JobStatusPending)
		if err != nil {
			logger.Error("Unable to return stopped LDAP sync job to pending", mlog.Err(err))
			return
		}
		if !pending {
			if _, err := w.transitionJob(job, model.JobStatusCancelRequested, model.JobStatusCanceled); err != nil {
				logger.Error("Unable to finish canceled LDAP sync job", mlog.Err(err))
			}
		}
		return
	default:
	}
	if appErr == nil && ctx.Err() != nil {
		appErr = model.NewAppError("LdapSync", "ent.ldap.sync_failed.app_error", nil, ctx.Err().Error(), http.StatusRequestTimeout)
	}
	if appErr != nil {
		w.finishError(logger, job, appErr)
		return
	}
	if err := w.jobServer.SetJobProgress(job, 100); err != nil {
		logger.Warn("Unable to update LDAP sync job progress", mlog.Err(err))
	}
	finished, err := w.transitionJob(job, model.JobStatusInProgress, model.JobStatusSuccess)
	if err != nil {
		logger.Error("Unable to mark LDAP sync job successful", mlog.Err(err))
	} else if !finished {
		if _, err := w.transitionJob(job, model.JobStatusCancelRequested, model.JobStatusCanceled); err != nil {
			logger.Error("Unable to finish canceled LDAP sync job", mlog.Err(err))
		}
	}
}

func (w *syncWorker) finishError(logger mlog.LoggerIFace, job *model.Job, appErr *model.AppError) {
	job.Status = model.JobStatusError
	job.Progress = -1
	if job.Data == nil {
		job.Data = model.StringMap{}
	}
	job.Data["error"] = appErr.Message
	if appErr.DetailedError != "" {
		job.Data["error"] += " — " + appErr.DetailedError
	}
	if wrapped := appErr.Unwrap(); wrapped != nil {
		job.Data["error"] += " — " + wrapped.Error()
	}
	updated, err := w.jobServer.Store.Job().UpdateOptimistically(job, model.JobStatusInProgress)
	if err != nil {
		logger.Error("Unable to mark LDAP sync job failed", mlog.Err(err))
		return
	}
	if updated != nil {
		if err := w.publishJobUpdate(updated); err != nil {
			logger.Error("Unable to publish failed LDAP sync job", mlog.Err(err))
		}
		return
	}
	if _, err := w.transitionJob(job, model.JobStatusCancelRequested, model.JobStatusCanceled); err != nil {
		logger.Error("Unable to finish canceled LDAP sync job", mlog.Err(err))
	}
}

func (w *syncWorker) transitionJob(job *model.Job, from, to string) (bool, error) {
	updated, err := w.jobServer.Store.Job().UpdateStatusOptimistically(job.Id, from, to)
	if err != nil || updated == nil {
		return false, err
	}
	return true, w.publishJobUpdate(updated)
}

func (w *syncWorker) publishJobUpdate(updated *model.Job) error {
	if metrics := w.service.app.Srv().GetMetrics(); metrics != nil {
		metrics.DecrementJobActive(updated.Type)
	}
	jobJSON, err := json.Marshal(updated)
	if err != nil {
		return err
	}
	message := model.NewWebSocketEvent(model.WebsocketEventJobUpdated, "", "", "", nil, "")
	message.Add("job", string(jobJSON))
	message.GetBroadcast().ContainsSensitiveData = true
	message.GetBroadcast().RequiredPermissions = []string{model.PermissionReadLdapSyncJob.Id}
	w.service.app.Publish(message)
	return nil
}

func (w *syncWorker) readJobStatus(ctx context.Context, id string) (string, error) {
	var status string
	err := w.jobServer.Store.GetInternalMasterDB().QueryRowContext(ctx,
		"SELECT Status FROM Jobs WHERE Id=$1", id).Scan(&status)
	return status, err
}

func (w *syncWorker) watchCancellation(ctx context.Context, jobID string, requested *atomic.Bool, cancel context.CancelFunc, done <-chan struct{}) {
	ticker := time.NewTicker(syncCancellationPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			pollCtx, pollCancel := context.WithTimeout(ctx, syncCancellationPollInterval)
			status, err := w.readJobStatus(pollCtx, jobID)
			pollCancel()
			if err == nil && status == model.JobStatusCancelRequested {
				requested.Store(true)
				cancel()
				return
			}
			if err != nil && !errors.Is(ctx.Err(), context.Canceled) {
				w.logger.Warn("Unable to poll LDAP sync cancellation", mlog.Err(err))
			}
		}
	}
}
