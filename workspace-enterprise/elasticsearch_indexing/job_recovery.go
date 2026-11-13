package elasticsearch_indexing

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

func (w *checkpointWorker) recoverInterruptedJobs(stop <-chan struct{}) {
	if err := w.recoverJobs(stop); err != nil {
		w.logger.Warn("Unable to recover interrupted indexing jobs", mlog.Err(err))
	}
}

func (w *checkpointWorker) recoverJobs(stop <-chan struct{}) error {
	if w.master == nil || !w.IsEnabled(w.liveConfig()) {
		return nil
	}
	select {
	case <-stop:
		return context.Canceled
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() { cancel(); <-finished }()
	candidates, err := w.recoveryCandidates(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !w.IsEnabled(w.liveConfig()) {
			return nil
		}
		if err := w.recoverJob(ctx, candidate); err != nil {
			return err
		}
		w.recoveryAfterAt, w.recoveryAfterID = candidate.CreateAt, candidate.Id
	}
	if len(candidates) < 100 {
		w.recoveryAfterAt, w.recoveryAfterID = 0, ""
	}
	return nil
}

func (w *checkpointWorker) recoveryCandidates(ctx context.Context) ([]*model.Job, error) {
	// Candidate reads need the same interruptible sockets as an owning
	// execution. A timeout on the application's pool cannot close pq's silent
	// startup/cancellation sockets without affecting unrelated application SQL.
	execution, err := newIndexingExecution(w.databaseDSN, w.queryTimeout)
	if err != nil {
		return nil, err
	}
	defer execution.Close()
	ctx, cancel := execution.context(ctx)
	defer cancel()
	// The primary is authoritative for candidate selection. Age alone is not
	// proof of a dead execution. Keyset pagination prevents 100 live claims
	// from permanently hiding a later abandoned claim.
	rows, err := execution.db.QueryContext(ctx, `SELECT `+indexingJobColumns+` FROM Jobs
        WHERE Type=$1 AND Status IN ($2,$3) AND Data->>$4 ~ '^[a-z0-9]{26}$'
        AND (CreateAt,Id)>($5,$6) ORDER BY CreateAt,Id LIMIT 100`, model.JobTypeElasticsearchPostIndexing,
		model.JobStatusInProgress, model.JobStatusCancelRequested, indexingAttemptKey, w.recoveryAfterAt, w.recoveryAfterID)
	if err != nil {
		return nil, err
	}
	var candidates []*model.Job
	for rows.Next() {
		job, err := scanIndexingJob(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		candidates = append(candidates, job)
	}
	return candidates, errors.Join(rows.Err(), rows.Close())
}

func (w *checkpointWorker) recoverJob(ctx context.Context, job *model.Job) error {
	execution, err := newIndexingExecution(w.databaseDSN, w.queryTimeout)
	if err != nil {
		return err
	}
	defer execution.Close()
	acquired, err := execution.acquire(ctx, job.Id)
	if err != nil || !acquired {
		return err
	}
	if !w.IsEnabled(w.liveConfig()) {
		return nil
	}
	queryCtx, cancel := execution.context(ctx)
	defer cancel()
	// Both predicate and lock are required: another recoverer may have
	// already canceled/requeued/reclaimed the selected snapshot of this Job.
	recovered, err := scanIndexingJob(execution.db.QueryRowContext(queryCtx, `UPDATE Jobs SET
        Status=CASE WHEN Status=$2 THEN $3 ELSE $4 END,LastActivityAt=$5
        WHERE Id=$1 AND Type=$6 AND Status IN ($2,$7) AND Data->>$8=$9 RETURNING `+indexingJobColumns,
		job.Id, model.JobStatusInProgress, model.JobStatusPending, model.JobStatusCanceled, model.GetMillis(),
		model.JobTypeElasticsearchPostIndexing, model.JobStatusCancelRequested, indexingAttemptKey, job.Data[indexingAttemptKey]))
	if err != nil || recovered == nil {
		return err
	}
	w.logger.Info("Recovered interrupted indexing job", mlog.String("job_id", job.Id), mlog.String("status", recovered.Status))
	return w.publishRecoveredJob(recovered)
}

func (w *checkpointWorker) publishRecoveredJob(job *model.Job) error {
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	event := model.NewWebSocketEvent(model.WebsocketEventJobUpdated, "", "", "", nil, "")
	event.Add("job", string(raw))
	event.GetBroadcast().ContainsSensitiveData = true
	event.GetBroadcast().RequiredPermissions = []string{model.PermissionReadElasticsearchPostIndexingJob.Id}
	// Recovery changes a claim from a previous process; the current process
	// did not increment its active-job gauge and must not decrement it here.
	w.publish(event)
	return nil
}
