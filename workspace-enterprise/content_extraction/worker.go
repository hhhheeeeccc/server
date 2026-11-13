package content_extraction

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
)

type workerRun struct {
	cancel  context.CancelFunc
	ctx     context.Context
	done    chan struct{}
	started bool
}

func newWorkerRun() *workerRun {
	ctx, cancel := context.WithCancel(context.Background())
	return &workerRun{ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

const pausedKey = "workspace_extraction_paused"

type worker struct {
	service        *Service
	candidates     chan model.Job
	mu             sync.Mutex
	run            *workerRun
	recoveryAt     int64
	recoveryID     string
	cacheSchemaDSN string
}

func (w *worker) IsEnabled(*model.Config) bool { return true }
func (w *worker) JobChannel() chan<- model.Job { return w.candidates }

// JobServer serializes lifecycle calls. Keep Run/Stop reusable when that
// server is stopped and started again, and wait for each monitor to exit.
func (w *worker) Run() {
	w.mu.Lock()
	r := w.run
	if r.started {
		w.mu.Unlock()
		return
	}
	r.started = true
	ctx := r.ctx
	w.mu.Unlock()
	defer close(r.done)
	w.maintain(ctx)
	recoveryTick := time.NewTicker(30 * time.Second)
	defer recoveryTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-recoveryTick.C:
			w.maintain(ctx)
		case job := <-w.candidates:
			if ctx.Err() != nil {
				return
			}
			w.doJob(ctx, job)
		}
	}
}

func (w *worker) Stop() {
	w.mu.Lock()
	r := w.run
	r.cancel()
	w.mu.Unlock()
	<-r.done
	w.mu.Lock()
	if w.run == r {
		w.run = newWorkerRun()
	}
	w.mu.Unlock()
}

func (w *worker) doJob(parent context.Context, candidate model.Job) {
	s := w.service
	if s.app.License() != nil {
		if err := w.prepareCacheOutbox(parent); err != nil {
			if parent.Err() == nil {
				s.app.Srv().Log().Warn("Unable to prepare content extraction cache outbox", mlog.Err(err))
			}
			return
		}
	}
	if candidate.Data[pausedKey] == "true" && s.gate(candidate.Data["catchup"] == "true") != nil {
		// The native watcher sends non-blockingly, oldest first. Returning
		// immediately for the same paused candidate can starve every later
		// candidate on its unbuffered channel. Select one eligible successor
		// from the primary instead; ClaimJob still arbitrates ownership.
		var err error
		candidate, err = w.nextEligible(parent)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) && parent.Err() == nil {
				s.app.Srv().Log().Warn("Unable to select content extraction job", mlog.Err(err))
			}
			return
		}
	}
	logger := s.app.Srv().Log().With(jobs.JobLoggerFields(&candidate)...)
	owner := s.execution()
	defer owner.Close()
	acquired, err := owner.acquire(parent, candidate.Id)
	if err != nil || !acquired {
		if err != nil && parent.Err() == nil {
			logger.Warn("Unable to acquire content extraction job ownership", mlog.Err(err))
		}
		return
	}
	job, err := owner.claim(parent)
	if err != nil {
		logger.Warn("Unable to claim content extraction job", mlog.Err(err))
		return
	}
	if job == nil {
		return
	}
	if metrics := s.app.Srv().GetMetrics(); metrics != nil {
		metrics.IncrementJobActive(job.Type)
		defer metrics.DecrementJobActive(job.Type)
	}
	s.publishJob(job)
	defer func() {
		if caught := recover(); caught != nil {
			logger.Error("Unhandled panic in content extraction job", mlog.Any("panic", caught), mlog.String("stack", string(debug.Stack())))
			w.finish(logger, owner, job, model.JobStatusError, fmt.Errorf("content extraction panic: %v", caught))
			panic(caught)
		}
	}()
	if job.Data == nil {
		job.Data = model.StringMap{}
	}
	delete(job.Data, "error")
	wasPaused := job.Data[pausedKey] == "true"
	delete(job.Data, pausedKey)
	catchup := job.Data["catchup"] == "true"
	// A job initially submitted without permission fails, rather than being
	// silently parked. A grant revoked during execution instead leaves pending.
	if err := s.gate(catchup); err != nil {
		if wasPaused {
			w.finish(logger, owner, job, model.JobStatusPending, nil)
			return
		}
		job.Data["processed"], job.Data["errors"] = "0", "0"
		w.finish(logger, owner, job, model.JobStatusError, err)
		return
	}
	ctx, stop := w.watch(parent, owner, catchup)
	defer stop()
	err = s.execute(ctx, logger, job, owner)
	cause := context.Cause(ctx)
	stop()
	status := model.JobStatusSuccess
	if err != nil {
		status = model.JobStatusError
	}
	var interrupted *interruption
	switch {
	case errors.As(cause, &interrupted) || errors.As(err, &interrupted):
		status, err = interrupted.status, interrupted.err
	case parent.Err() != nil || s.gate(catchup) != nil:
		status, err = model.JobStatusPending, nil
	case cause != nil:
		status, err = model.JobStatusError, cause
	}
	w.finish(logger, owner, job, status, err)
}

func (w *worker) nextEligible(ctx context.Context) (model.Job, error) {
	s := w.service
	reader := s.database("selector")
	defer reader.Close()
	job := model.Job{Type: model.JobTypeExtractContent}
	err := reader.query(ctx, func(ctx context.Context, db *sql.DB) error {
		return db.QueryRowContext(ctx,
			`SELECT Id FROM Jobs WHERE Type=$1 AND Status=$2
		 AND (COALESCE(Data->>$3, '')<>'true'
		      OR ($4 AND (COALESCE(Data->>'catchup', '')<>'true' OR $5)))
		 ORDER BY CreateAt ASC, Id ASC LIMIT 1`, model.JobTypeExtractContent,
			model.JobStatusPending, pausedKey, s.app.License() != nil,
			model.SafeDereference(s.app.Config().FileSettings.ExtractContent)).Scan(&job.Id)
	})
	return job, err
}

type interruption struct {
	status string
	err    error
}

func (i *interruption) Error() string { return i.status }

func (w *worker) watch(parent context.Context, owner *jobExecution, catchup bool) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		reader := w.service.database("control")
		defer reader.Close()
		gateTick := time.NewTicker(100 * time.Millisecond)
		defer gateTick.Stop()
		statusTick := time.NewTicker(time.Second)
		defer statusTick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-gateTick.C:
				if w.service.gate(catchup) != nil {
					cancel(&interruption{status: model.JobStatusPending})
					return
				}
			case <-statusTick.C:
				// Read cancellation from the primary, never a stale replica.
				var status, token string
				var held bool
				key := uint64(extractionLock(owner.id))
				err := reader.query(ctx, func(ctx context.Context, db *sql.DB) error {
					return db.QueryRowContext(ctx, `SELECT Status,COALESCE(Data->>$2,''),
                    EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND pid=$3
                        AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
                        AND classid=$4::oid AND objid=$5::oid AND objsubid=1 AND granted)
                    FROM Jobs WHERE Id=$1`, owner.id, attemptKey, owner.pid, key>>32, key&0xffffffff).Scan(&status, &token, &held)
				})
				if err != nil {
					cancel(fmt.Errorf("read content extraction job status: %w", err))
					return
				}
				if status == model.JobStatusCancelRequested || status == model.JobStatusCanceled {
					cancel(&interruption{status: model.JobStatusCanceled})
					return
				}
				if token != owner.token || !held {
					cancel(errOwnershipLost)
					return
				}
				if status != model.JobStatusInProgress {
					cancel(fmt.Errorf("content extraction job is no longer in progress: %s", status))
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(nil); <-done }
}

func (w *worker) finish(logger mlog.LoggerIFace, owner *jobExecution, job *model.Job, status string, executionErr error) {
	job.Status = status
	job.Progress = 0
	if status == model.JobStatusPending {
		job.Data[pausedKey] = "true"
	} else {
		delete(job.Data, pausedKey)
	}
	if status == model.JobStatusSuccess {
		job.Progress = 100
	} else if status == model.JobStatusError {
		job.Progress = -1
		if executionErr != nil {
			job.Data["error"] = executionErr.Error()
		}
	}
	// Stop cancels execution, but still gets one independent bounded window
	// to persist pending. Both status-CAS attempts share this total deadline.
	ctx, cancel := context.WithTimeout(context.Background(), owner.session.timeout)
	defer cancel()
	updated, err := owner.update(ctx, job, model.JobStatusInProgress)
	if err == nil && updated == nil {
		// A committed cancellation wins even if it arrived after the last
		// poll or while the final success/error/pending update was executing.
		job.Status, job.Progress = model.JobStatusCanceled, 0
		delete(job.Data, "error")
		delete(job.Data, pausedKey)
		updated, err = owner.update(ctx, job, model.JobStatusCancelRequested)
	}
	if err != nil {
		logger.Error("Unable to finalize content extraction job", mlog.Err(err))
		return
	}
	if updated == nil {
		return
	}
	w.service.publishJob(updated)
}
