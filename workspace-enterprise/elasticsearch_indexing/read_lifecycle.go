package elasticsearch_indexing

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

type indexingInterruption struct {
	status string
	err    error
}

func (i *indexingInterruption) Error() string { return "indexing work interrupted: " + i.status }

func indexingWorkCause(ctx context.Context) *indexingInterruption {
	if ctx == nil {
		return nil
	}
	cause, _ := context.Cause(ctx).(*indexingInterruption)
	return cause
}

func (w *checkpointWorker) startIndexingWork(stop <-chan struct{}, id string) (context.Context, <-chan struct{}, func()) {
	ctx, cancel := context.WithCancelCause(context.Background())
	w.workContext = ctx
	canceled := make(chan struct{})
	var workers sync.WaitGroup
	workers.Go(func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			if state := w.batchInterruption(stop, canceled); state != "" {
				cancel(&indexingInterruption{status: state})
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-stop:
			case <-canceled:
			case <-tick.C:
			}
		}
	})
	if w.store.postgres {
		execution := w.store.execution
		workers.Go(func() {
			tick := time.NewTicker(5 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					// Do not bind the owning session to the read context: normal
					// Stop must retain its advisory lock until the checkpoint CAS.
					status, err := execution.status()
					if err != nil {
						cancel(&indexingInterruption{status: model.JobStatusError, err: err})
						return
					}
					if status == model.JobStatusCancelRequested {
						close(canceled)
						cancel(&indexingInterruption{status: model.JobStatusCanceled})
						return
					}
				}
			}
		})
	} else {
		workers.Go(func() { w.server.CancellationWatcher(request.EmptyContext(w.logger).WithContext(ctx), id, canceled) })
	}
	var once sync.Once
	return ctx, canceled, func() { once.Do(func() { cancel(nil); workers.Wait() }) }
}

func (s *indexingExecution) status() (string, error) {
	if s.lost.Load() || !s.held || s.token == "" {
		return "", errIndexingOwnershipLost
	}
	ctx, cancel := s.context(context.Background())
	defer cancel()
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT Status FROM Jobs WHERE Id=$1 AND Type=$2
		AND Status IN ($3,$4) AND Data->>$5=$6`, s.id, model.JobTypeElasticsearchPostIndexing,
		model.JobStatusInProgress, model.JobStatusCancelRequested, indexingAttemptKey, s.token).Scan(&status)
	if err != nil {
		s.lost.Store(true)
		if errors.Is(err, sql.ErrNoRows) {
			return "", errIndexingOwnershipLost
		}
		return "", errors.Join(errIndexingOwnershipLost, err)
	}
	return status, nil
}
