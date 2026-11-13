package compliance_service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

const (
	dailyScheduleKey   = "WorkspaceComplianceDailyThroughV1"
	dailyMillis        = int64(24 * time.Hour / time.Millisecond)
	maxDailyScheduling = 31
)

func (s *Service) dailyEnabled() bool {
	cfg, license := s.app.Config(), s.app.Srv().License()
	return cfg != nil && model.SafeDereference(cfg.ComplianceSettings.Enable) &&
		model.SafeDereference(cfg.ComplianceSettings.EnableDaily) && license != nil &&
		license.Features != nil && model.SafeDereference(license.Features.Compliance)
}

// Prepare a bounded set of missing days, then keep the existing export cadence:
// yesterday plus one rotating older report. Creating the backlog must not run
// all its file exports synchronously during server startup.
func (s *Service) runDailyCycle(ctx context.Context, end time.Time, afterEndAt int64) (int64, error) {
	if !s.dailyEnabled() {
		return 0, nil
	}
	if err := s.prepareDailyIntervals(ctx, end); err != nil {
		return 0, err
	}
	if !s.dailyEnabled() {
		return 0, nil
	}
	currentErr := s.runDailyInterval(ctx, end)
	if ctx.Err() != nil {
		return 0, errors.Join(currentErr, ctx.Err())
	}
	if !s.dailyEnabled() {
		return 0, currentErr
	}
	attempted, olderErr := s.runPendingDailyInterval(ctx, end, afterEndAt)
	return attempted, errors.Join(currentErr, olderErr)
}

// The watermark describes scheduled intervals, not completed exports. A first
// activation starts with yesterday only; it cannot infer past obligations.
// Subsequent activations fill every UTC day after that durable baseline,
// including days spent disabled or unlicensed, once scheduling is enabled again.
func (s *Service) prepareDailyIntervals(ctx context.Context, end time.Time) (resultErr error) {
	defer func() {
		// pq can report SQLSTATE 57014 instead of wrapping ctx.Err(). Keep
		// its database detail while allowing callers to identify cancellation.
		if cause := ctx.Err(); cause != nil && resultErr != nil && !errors.Is(resultErr, cause) {
			resultErr = errors.Join(resultErr, cause)
		}
	}()
	if !s.dailyEnabled() {
		return nil
	}
	cutoff := end.UnixMilli()
	if cutoff <= dailyMillis || cutoff%dailyMillis != 0 || end.Nanosecond() != 0 {
		return errors.New("daily compliance schedule requires a UTC midnight after 1970-01-02")
	}
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return errors.New("master database unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Share the creation lock with individual report creation on every node.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(11283544, 2)`); err != nil {
		return err
	}
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT Value FROM Systems WHERE Name=$1 FOR UPDATE`, dailyScheduleKey).Scan(&raw)
	through := cutoff - dailyMillis
	switch {
	case err == nil:
		through, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || through <= dailyMillis || through%dailyMillis != 0 || through > cutoff {
			return errors.New("invalid or future daily compliance schedule watermark")
		}
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if through == cutoff {
		return nil
	}
	for scheduled := 0; scheduled < maxDailyScheduling && through < cutoff; scheduled++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !s.dailyEnabled() {
			return errors.New("daily compliance scheduling was disabled")
		}
		next := through + dailyMillis
		start := time.UnixMilli(through).UTC()
		job := &model.Compliance{Desc: start.Format(time.DateOnly), Type: model.ComplianceTypeDaily,
			StartAt: through, EndAt: next}
		if _, _, err := createDailyJobInTransaction(ctx, tx, job); err != nil {
			return fmt.Errorf("schedule daily compliance report %s: %w", job.Desc, err)
		}
		through = next
	}
	if !s.dailyEnabled() {
		return errors.New("daily compliance scheduling was disabled")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO Systems (Name,Value) VALUES ($1,$2)
        ON CONFLICT (Name) DO UPDATE SET Value=EXCLUDED.Value`, dailyScheduleKey, strconv.FormatInt(through, 10)); err != nil {
		return err
	}
	return tx.Commit()
}
