package content_extraction

import (
	"context"
	"database/sql"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

func (w *worker) recoverJobs(ctx context.Context) {
	if ctx.Err() != nil || w.service.app.License() == nil {
		return
	}
	if err := w.recoverCandidates(ctx); err != nil && ctx.Err() == nil {
		w.service.app.Srv().Log().Warn("Unable to recover interrupted content extraction jobs", mlog.Err(err))
	}
}

func (w *worker) recoverCandidates(ctx context.Context) error {
	s := w.service
	reader := s.database("recovery")
	defer reader.Close()
	var candidates []*model.Job
	err := reader.query(ctx, func(ctx context.Context, db *sql.DB) error {
		rows, err := db.QueryContext(ctx, `SELECT `+jobColumns+` FROM Jobs
        WHERE Type=$1 AND Status IN ($2,$3) AND Data->>$4 ~ '^[a-z0-9]{26}$'
        AND (CreateAt,Id)>($5,$6) ORDER BY CreateAt,Id LIMIT 100`, model.JobTypeExtractContent,
			model.JobStatusInProgress, model.JobStatusCancelRequested, attemptKey, w.recoveryAt, w.recoveryID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			job, err := scanJob(rows)
			if err != nil {
				return err
			}
			candidates = append(candidates, job)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, job := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s.app.License() == nil {
			return nil
		}
		err := func() error {
			owner := s.execution()
			defer owner.Close()
			acquired, err := owner.acquire(ctx, job.Id)
			if err != nil || !acquired {
				return err
			}
			var recovered *model.Job
			err = owner.session.query(ctx, func(ctx context.Context, db *sql.DB) error {
				var err error
				recovered, err = scanJob(db.QueryRowContext(ctx, `UPDATE Jobs SET
                Status=CASE WHEN Status=$2 THEN $3 ELSE $4 END,LastActivityAt=$5,
                Data=CASE WHEN Status=$2 THEN Data||jsonb_build_object($10::text,'true'::text) ELSE Data-$10::text END
                WHERE Id=$1 AND Type=$6 AND Status IN ($2,$7) AND Data->>$8=$9 RETURNING `+jobColumns,
					job.Id, model.JobStatusInProgress, model.JobStatusPending, model.JobStatusCanceled, model.GetMillis(),
					model.JobTypeExtractContent, model.JobStatusCancelRequested, attemptKey, job.Data[attemptKey], pausedKey))
				return err
			})
			if err != nil {
				return err
			}
			if recovered != nil {
				s.publishJob(recovered)
			}
			return nil
		}()
		if err != nil {
			return err
		}
		w.recoveryAt, w.recoveryID = job.CreateAt, job.Id
	}
	if len(candidates) < 100 {
		w.recoveryAt, w.recoveryID = 0, ""
	}
	return ctx.Err()
}
