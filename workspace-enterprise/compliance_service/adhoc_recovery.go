package compliance_service

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

type adhocCursor struct {
	createdAt int64
	id        string
}

func (e *complianceExecution) loadAdhoc() (*model.Compliance, error) {
	var saved model.Compliance
	err := e.query(e.ctx, func(ctx context.Context, db *sql.DB) error {
		return db.QueryRowContext(ctx, `SELECT Id,CreateAt,UserId,Status,Count,"desc",Type,StartAt,EndAt,Keywords,Emails
            FROM Compliances WHERE Id=$1 AND Type=$2 AND `+e.ownedSQL, e.reportID, model.ComplianceTypeAdhoc).
			Scan(&saved.Id, &saved.CreateAt, &saved.UserId, &saved.Status, &saved.Count, &saved.Desc,
				&saved.Type, &saved.StartAt, &saved.EndAt, &saved.Keywords, &saved.Emails)
	})
	return &saved, err
}

func (s *Service) runAdhocScheduler(ctx context.Context, period time.Duration) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	var cursor adhocCursor
	for {
		if ctx.Err() != nil {
			return
		}
		next, err := s.runPendingAdhoc(ctx, cursor)
		if next.id != "" {
			cursor = next
		}
		if err != nil && ctx.Err() == nil {
			s.app.Log().Error("Unable to recover adhoc compliance report", mlog.Err(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Attempt at most one saved report per pass. Rotate past errors, busy owners
// and tied timestamps, so a poison report cannot starve later pending IDs.
func (s *Service) runPendingAdhoc(ctx context.Context, after adhocCursor) (adhocCursor, error) {
	if s.checkComplianceExportEnabled() != nil {
		return adhocCursor{}, nil
	}
	settings := s.app.Config().SqlSettings
	settings.DataSourceReplicas = nil // pending state must come from the primary
	reader := newComplianceReader(settings)
	var next adhocCursor
	err := reader.query(ctx, func(ctx context.Context, db *sql.DB) error {
		return db.QueryRowContext(ctx, `SELECT CreateAt,Id FROM Compliances
            WHERE Type=$1 AND Status IN ('created','running','failed')
            ORDER BY CASE WHEN (CreateAt,Id)>($2,$3) THEN 0 ELSE 1 END,CreateAt,Id LIMIT 1`,
			model.ComplianceTypeAdhoc, after.createdAt, after.id).Scan(&next.createdAt, &next.id)
	})
	// Release the discovery connection and permit before acquiring ownership.
	reader.Close()
	if errors.Is(err, sql.ErrNoRows) {
		return adhocCursor{}, nil
	}
	if err != nil {
		return adhocCursor{}, err
	}
	rctx := request.EmptyContext(s.app.Log()).WithLogFields(mlog.String("job_id", next.id))
	if appErr := s.runComplianceJob(ctx, rctx, &model.Compliance{Id: next.id}); appErr != nil {
		return next, appErr
	}
	return next, nil
}
