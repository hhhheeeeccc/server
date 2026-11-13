package content_extraction

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

const attemptKey = "workspace_extraction_attempt_v1"
const jobColumns = `Id,Type,Priority,CreateAt,StartAt,LastActivityAt,Status,Progress,Data`

var errOwnershipLost = errors.New("content extraction no longer owns its database session and job attempt")

type jobExecution struct {
	session   *databaseSession
	id, token string
	pid       int
	held      bool
}

func (s *Service) execution() *jobExecution {
	session := s.database("owner")
	session.readOnly = false
	session.singleUse = true
	return &jobExecution{session: session}
}

func extractionLock(id string) int64 {
	hash := sha256.Sum256([]byte("workspace-extraction-attempt-v1:" + id))
	return int64(binary.BigEndian.Uint64(hash[:8]))
}

func (e *jobExecution) Close() { e.held = false; e.session.Close() }

func (e *jobExecution) acquire(ctx context.Context, id string) (bool, error) {
	var acquired bool
	err := e.session.query(ctx, func(ctx context.Context, db *sql.DB) error {
		return db.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1),pg_backend_pid()`, extractionLock(id)).Scan(&acquired, &e.pid)
	})
	e.held = err == nil && acquired
	if e.held {
		e.id = id
	}
	return e.held, err
}

type jobScanner interface{ Scan(...any) error }

func scanJob(row jobScanner) (*model.Job, error) {
	job := &model.Job{}
	var data []byte
	err := row.Scan(&job.Id, &job.Type, &job.Priority, &job.CreateAt, &job.StartAt, &job.LastActivityAt, &job.Status, &job.Progress, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &job.Data); err != nil {
			return nil, err
		}
	}
	return job, nil
}

func (e *jobExecution) claim(ctx context.Context) (*model.Job, error) {
	if !e.held {
		return nil, errOwnershipLost
	}
	var job *model.Job
	token := model.NewId()
	err := e.session.query(ctx, func(ctx context.Context, db *sql.DB) error {
		// A disconnected autocommit UPDATE can still commit after a row lock
		// is released. Keep the claim provisional until its result is read
		// and the owning worker is still running. An uncertain COMMIT is
		// recovered by the existing attempt token and advisory-lock sweep.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		job, err = scanJob(tx.QueryRowContext(ctx, `UPDATE Jobs SET Status=$2,StartAt=$3,LastActivityAt=$3,
        Data=COALESCE(NULLIF(Data,'null'::jsonb),'{}'::jsonb)||jsonb_build_object($4::text,$5::text)
        WHERE Id=$1 AND Type=$6 AND Status=$7
        AND (Data IS NULL OR jsonb_typeof(Data) IN ('null','object')) RETURNING `+jobColumns,
			e.id, model.JobStatusInProgress, model.GetMillis(), attemptKey, token, model.JobTypeExtractContent, model.JobStatusPending))
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return tx.Commit()
	})
	if job != nil && err == nil {
		e.token = token
	}
	return job, err
}

func (e *jobExecution) update(ctx context.Context, job *model.Job, from string) (*model.Job, error) {
	if !e.held || e.token == "" || e.id != job.Id || job.Data[attemptKey] != e.token {
		return nil, errOwnershipLost
	}
	data, err := json.Marshal(job.Data)
	if err != nil {
		return nil, err
	}
	var updated *model.Job
	err = e.session.query(ctx, func(ctx context.Context, db *sql.DB) error {
		var err error
		updated, err = scanJob(db.QueryRowContext(ctx, `UPDATE Jobs SET Status=$3,LastActivityAt=$4,Progress=$5,Data=$6::jsonb
        WHERE Id=$1 AND Status=$2 AND Type=$7 AND Data->>$8=$9 RETURNING `+jobColumns,
			job.Id, from, job.Status, model.GetMillis(), job.Progress, string(data), model.JobTypeExtractContent, attemptKey, e.token))
		return err
	})
	return updated, err
}

func (e *jobExecution) progress(ctx context.Context, job *model.Job) error {
	job.Status = model.JobStatusInProgress
	updated, err := e.update(ctx, job, model.JobStatusInProgress)
	if err == nil && updated == nil {
		return errOwnershipLost
	}
	return err
}

func (s *Service) publishJob(job *model.Job) {
	data, err := json.Marshal(job)
	if err != nil {
		s.app.Srv().Log().Error("Unable to encode content extraction job update", mlog.Err(err))
		return
	}
	event := model.NewWebSocketEvent(model.WebsocketEventJobUpdated, "", "", "", nil, "")
	event.Add("job", string(data))
	event.GetBroadcast().ContainsSensitiveData = true
	event.GetBroadcast().RequiredPermissions = []string{model.PermissionReadJobs.Id}
	s.app.Publish(event)
}
