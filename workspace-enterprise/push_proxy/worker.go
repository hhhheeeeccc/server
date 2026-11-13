package push_proxy

import (
	"context"
	"errors"
	"sync"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
)

// Worker ensures that a configured gateway has its first shared token. URL
// changes rotate through GenerateAuthToken; this worker never rotates a token
// that already exists.
type Worker struct {
	service   *Service
	jobServer *jobs.JobServer
	jobs      chan model.Job
	mu        sync.Mutex
	stop      chan struct{}
	done      chan struct{}
	running   bool
}

var _ model.Worker = (*Worker)(nil)

func (w *Worker) IsEnabled(cfg *model.Config) bool { return w.service.enabled(cfg) }

func (w *Worker) JobChannel() chan<- model.Job { return w.jobs }

func (w *Worker) Run() {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	// A stop requested before the goroutine starts must still be observed.
	// Reset channels only after a previous run has actually exited.
	select {
	case <-w.done:
		w.stop = make(chan struct{})
		w.done = make(chan struct{})
	default:
	}
	w.running = true
	stop, done := w.stop, w.done
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running = false
		close(done)
		w.mu.Unlock()
	}()
	for {
		select {
		case <-stop:
			return
		case job := <-w.jobs:
			w.doJob(&job)
		}
	}
}

func (w *Worker) Stop() {
	w.mu.Lock()
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	running := w.running
	done := w.done
	w.mu.Unlock()
	if running {
		<-done
	}
}

func (w *Worker) doJob(job *model.Job) {
	logger := w.jobServer.Logger().With(jobs.JobLoggerFields(job)...)
	defer w.jobServer.HandleJobPanic(logger, job)
	claimed, appErr := w.jobServer.ClaimJob(job)
	if appErr != nil {
		logger.Warn("Unable to claim push token job", mlog.Err(appErr))
		return
	}
	if claimed == nil {
		return
	}
	job = claimed
	if !w.service.enabled(w.service.app.Config()) {
		w.failJob(logger, job, errors.New("push gateway was disabled before token generation"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	store, err := w.service.tokenStore(ctx)
	if err == nil {
		err = store.EnsureInitialToken(ctx)
	}
	if err != nil {
		w.failJob(logger, job, err)
		return
	}
	if appErr = w.jobServer.SetJobProgress(job, 100); appErr != nil {
		w.failJob(logger, job, appErr)
		return
	}
	if appErr = w.jobServer.SetJobSuccess(job); appErr != nil {
		logger.Error("Unable to record push token job success", mlog.Err(appErr))
		return
	}
	logger.Info("Push gateway token is ready")
}

func (w *Worker) failJob(logger mlog.LoggerIFace, job *model.Job, err error) {
	logger.Error("Push gateway token job failed", mlog.Err(err))
	if appErr := w.jobServer.SetJobError(job, tokenError("PushProxyWorker", err)); appErr != nil {
		logger.Error("Unable to record push token job failure", mlog.Err(appErr))
	}
}
