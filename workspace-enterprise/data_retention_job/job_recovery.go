package data_retention_job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

// Only claims made by this protocol can be recovered automatically. An older
// worker may have claimed a job before acquiring the execution lock.
const retentionAttemptKey = "workspace_retention_attempt_v1"

const retentionJobColumns = `Id,Type,Priority,CreateAt,StartAt,LastActivityAt,Status,Progress,Data`

var errRetentionAttemptLost = errors.New("retention job belongs to another attempt or is no longer running")

type retentionJobScanner interface{ Scan(...any) error }

func scanRetentionJob(row retentionJobScanner) (*model.Job, error) {
	job := &model.Job{}
	var data []byte
	err := row.Scan(&job.Id, &job.Type, &job.Priority, &job.CreateAt, &job.StartAt,
		&job.LastActivityAt, &job.Status, &job.Progress, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) != 0 {
		if err := json.Unmarshal(data, &job.Data); err != nil {
			return nil, err
		}
	}
	return job, nil
}

// The same non-reconnecting session owns the execution lock and this claim.
// All later status writes require this attempt, including error and panic paths.
func (s *retentionExecution) claimJob(id string) (*model.Job, error) {
	ctx, cancel := s.queryContext()
	defer cancel()
	return scanRetentionJob(s.db.QueryRowContext(ctx, `UPDATE Jobs SET Status=$2,
        StartAt=$3,LastActivityAt=$3,Data=COALESCE(NULLIF(Data,'null'::jsonb),'{}'::jsonb)
        || jsonb_build_object($4::text,$5::text)
        WHERE Id=$1 AND Type=$6 AND Status=$7
        AND (Data IS NULL OR jsonb_typeof(Data) IN ('null','object')) RETURNING `+retentionJobColumns,
		id, model.JobStatusInProgress, model.GetMillis(), retentionAttemptKey, model.NewId(),
		model.JobTypeDataRetention, model.JobStatusPending))
}

// This bounded metadata-only connection may run after the execution session
// died. The token predicate prevents an obsolete worker from finishing a new
// claim of the same Job, even when that new claim is also in_progress.
func (w *Worker) updateJobAttempt(job *model.Job, from, to string, progress int64, data model.StringMap) (*model.Job, error) {
	if job.Data[retentionAttemptKey] == "" {
		// Existing callers that finish a manually claimed legacy job retain the
		// prior behavior. Production doJob claims always carry an attempt token.
		return w.jobServer.Store.Job().UpdateOptimistically(job, from)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := newRetentionExecution(ctx, w.databaseDSN, w.queryTimeout)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	queryCtx, stopQuery := session.queryContext()
	defer stopQuery()
	return scanRetentionJob(session.db.QueryRowContext(queryCtx, `UPDATE Jobs SET
        Status=$3,LastActivityAt=$4,Progress=$5,Data=$6::jsonb
        WHERE Id=$1 AND Status=$2 AND Type=$7 AND Data->>$8=$9 RETURNING `+retentionJobColumns,
		job.Id, from, to, model.GetMillis(), progress, string(encoded), model.JobTypeDataRetention,
		retentionAttemptKey, job.Data[retentionAttemptKey]))
}

func (w *Worker) setJobProgress(job *model.Job, progress int64) error {
	if job.Data[retentionAttemptKey] == "" {
		if appErr := w.jobServer.SetJobProgress(job, progress); appErr != nil {
			return appErr
		}
		return nil
	}
	updated, err := w.updateJobAttempt(job, model.JobStatusInProgress, model.JobStatusInProgress, progress, job.Data)
	if err != nil {
		return err
	}
	if updated == nil {
		return errRetentionAttemptLost
	}
	*job = *updated
	return w.publishJobStatus(updated)
}

func (w *Worker) recoverInterruptedJobs() {
	// Some lifecycle tests construct a worker without an application. Actual
	// registered workers always have one; licensing also gates recovery itself.
	if w.app == nil || w.databaseDSN == "" || !retentionEnabled(w.app.Config()) || w.requireLicense() != nil {
		return
	}
	if err := w.recoverJobs(); err != nil {
		w.logger.Warn("Unable to recover interrupted retention jobs", mlog.Err(err))
	}
}

func (w *Worker) recoverJobs() error {
	select {
	case <-w.stop:
		return nil
	default:
	}
	if !retentionEnabled(w.app.Config()) || w.requireLicense() != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-w.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() { cancel(); <-watcherDone }()
	session, err := newRetentionExecution(ctx, w.databaseDSN, w.queryTimeout)
	if err != nil {
		return err
	}
	defer session.Close()
	var acquired bool
	queryCtx, stopQuery := session.queryContext()
	err = session.db.QueryRowContext(queryCtx, `SELECT pg_try_advisory_lock($1)`, retentionExecutionLock).Scan(&acquired)
	stopQuery()
	if err != nil || !acquired {
		return err
	}
	if w.requireLicense() != nil || !retentionEnabled(w.app.Config()) {
		return nil
	}
	queryCtx, stopQuery = session.queryContext()
	defer stopQuery()
	rows, err := session.db.QueryContext(queryCtx, `UPDATE Jobs SET
        Status=CASE WHEN Status=$1 THEN $2 ELSE $3 END,
        Progress=CASE WHEN Status=$1 THEN 0 ELSE Progress END,LastActivityAt=$4
        WHERE Id IN (SELECT Id FROM Jobs WHERE Type=$5 AND Status IN ($1,$6)
            AND Data->>$7 ~ '^[a-z0-9]{26}$' ORDER BY CreateAt LIMIT 100)
        AND Type=$5 AND Status IN ($1,$6) RETURNING `+retentionJobColumns,
		model.JobStatusInProgress, model.JobStatusPending, model.JobStatusCanceled, model.GetMillis(),
		model.JobTypeDataRetention, model.JobStatusCancelRequested, retentionAttemptKey)
	if err != nil {
		return err
	}
	var recovered []*model.Job
	for rows.Next() {
		job, err := scanRetentionJob(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		recovered = append(recovered, job)
	}
	iterationErr, closeErr := rows.Err(), rows.Close()
	if err := errors.Join(iterationErr, closeErr); err != nil {
		return err
	}
	for _, job := range recovered {
		// Gauges count claims in this process, not jobs owned by a dead process.
		if err := w.publishJobStatus(job); err != nil {
			return err
		}
	}
	return nil
}
