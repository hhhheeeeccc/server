package compliance_service

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

const defaultBatchSize = 30000

type Service struct {
	app       *app.App
	startOnce sync.Once
}

type complianceBatchFetcher func(model.ComplianceExportCursor, int) ([]*model.CompliancePost, model.ComplianceExportCursor, error)

var _ einterfaces.ComplianceInterface = (*Service)(nil)

func New(a *app.App) einterfaces.ComplianceInterface {
	return &Service{app: a}
}

func (s *Service) StartComplianceDailyJob() {
	// Core calls this hook even when daily reports are disabled. Track both
	// workers so server shutdown cancels them before closing the SQL store.
	s.startOnce.Do(func() {
		ctx := s.app.Srv().Platform().GoContext()
		s.app.Srv().Go(func() { s.runDailyScheduler(ctx, 5*time.Minute) })
		s.app.Srv().Go(func() { s.runAdhocScheduler(ctx, 5*time.Minute) })
	})
}

func (s *Service) runDailyScheduler(ctx context.Context, period time.Duration) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	var lastRetriedEndAt int64
	for {
		if ctx.Err() != nil {
			return
		}
		end := time.Now().UTC().Truncate(24 * time.Hour)
		attemptedEndAt, err := s.runDailyCycle(ctx, end, lastRetriedEndAt)
		if attemptedEndAt != 0 {
			lastRetriedEndAt = attemptedEndAt
		}
		if err != nil && ctx.Err() == nil {
			s.app.Log().Error("Unable to run daily compliance reports", mlog.Err(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Retry one older report on each scheduler pass. Rotate across pending days so
// one repeatedly failing export cannot prevent later pending reports.
func (s *Service) runPendingDailyInterval(ctx context.Context, before time.Time, afterEndAt int64) (int64, error) {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return 0, errors.New("master database unavailable")
	}
	var endAt int64
	err := db.QueryRowContext(ctx, `SELECT EndAt FROM Compliances
		WHERE Type=$1 AND EndAt<$2 AND EndAt-StartAt=$3 AND EndAt%$3=0
		AND Status IN ($4,$5,$6)
		ORDER BY CASE WHEN EndAt>$7 THEN 0 ELSE 1 END, EndAt LIMIT 1`, model.ComplianceTypeDaily,
		before.UnixMilli(), int64((24*time.Hour)/time.Millisecond), model.ComplianceStatusCreated,
		model.ComplianceStatusRunning, model.ComplianceStatusFailed, afterEndAt).Scan(&endAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return endAt, s.runDailyInterval(ctx, time.UnixMilli(endAt).UTC())
}

func (s *Service) runDailyInterval(ctx context.Context, end time.Time) error {
	if !s.dailyEnabled() {
		return nil
	}
	execution, acquired, err := s.acquireDailyExecution(ctx)
	if err != nil || !acquired {
		return err
	}
	defer execution.Close()
	job := &model.Compliance{
		Desc:    end.Add(-24 * time.Hour).Format(time.DateOnly),
		Type:    model.ComplianceTypeDaily,
		StartAt: end.Add(-24 * time.Hour).UnixMilli(),
		EndAt:   end.UnixMilli(),
	}
	var saved *model.Compliance
	var run bool
	err = execution.query(execution.ctx, func(ctx context.Context, db *sql.DB) error {
		var err error
		saved, run, err = findOrCreateDailyJob(ctx, db, job)
		return err
	})
	if err != nil || !run {
		return err
	}
	rctx := request.EmptyContext(s.app.Log()).WithLogFields(saved.LoggerFields()...)
	if appErr := s.runComplianceJobWithExecution(execution.ctx, rctx, saved, execution); appErr != nil {
		return errors.Join(appErr, context.Cause(execution.ctx))
	}
	return nil
}

func findOrCreateDailyJob(ctx context.Context, db *sql.DB, job *model.Compliance) (*model.Compliance, bool, error) {
	var existing model.Compliance
	err := db.QueryRowContext(ctx, `SELECT Id,CreateAt,UserId,Status,Count,"desc",Type,StartAt,EndAt,Keywords,Emails
		FROM Compliances WHERE Type=$1 AND StartAt=$2 AND EndAt=$3 ORDER BY CreateAt LIMIT 1`,
		model.ComplianceTypeDaily, job.StartAt, job.EndAt).Scan(&existing.Id, &existing.CreateAt,
		&existing.UserId, &existing.Status, &existing.Count, &existing.Desc, &existing.Type,
		&existing.StartAt, &existing.EndAt, &existing.Keywords, &existing.Emails)
	if err == nil {
		return &existing, existing.Status != model.ComplianceStatusFinished && existing.Status != model.ComplianceStatusRemoved, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	created, inserted, err := createDailyJobOnce(ctx, db, job)
	if err != nil || inserted {
		return created, inserted, err
	}
	// A concurrent creator not using the session lock may have inserted it.
	return findOrCreateDailyJob(ctx, db, job)
}

// The transaction lock coordinates daily schedulers on all nodes using the
// same database. Checking the master avoids a stale replica creating a second
// report for the same UTC interval.
func createDailyJobOnce(ctx context.Context, db *sql.DB, job *model.Compliance) (*model.Compliance, bool, error) {
	if db == nil {
		return nil, false, errors.New("master database unavailable")
	}
	if job == nil || job.Type != model.ComplianceTypeDaily {
		return nil, false, errors.New("daily compliance job required")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(11283544, 2)`); err != nil {
		return nil, false, err
	}
	created, inserted, err := createDailyJobInTransaction(ctx, tx, job)
	if err != nil || !inserted {
		return created, inserted, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return created, true, nil
}

// The caller holds the shared daily-creation transaction lock. The schedule
// cursor can therefore commit atomically with every missing report it covers.
func createDailyJobInTransaction(ctx context.Context, tx *sql.Tx, job *model.Compliance) (*model.Compliance, bool, error) {
	var existingID string
	err := tx.QueryRowContext(ctx, `SELECT Id FROM Compliances WHERE Type=$1 AND StartAt=$2 AND EndAt=$3 LIMIT 1`,
		model.ComplianceTypeDaily, job.StartAt, job.EndAt).Scan(&existingID)
	if err == nil {
		return nil, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	job.PreSave()
	if appErr := job.IsValid(); appErr != nil {
		return nil, false, appErr
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO Compliances (Id,CreateAt,UserId,Status,Count,"desc",Type,StartAt,EndAt,Keywords,Emails)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, job.Id, job.CreateAt, job.UserId,
		job.Status, job.Count, job.Desc, job.Type, job.StartAt, job.EndAt, job.Keywords, job.Emails)
	if err != nil {
		return nil, false, err
	}
	return job, true, nil
}

func (s *Service) RunComplianceJob(rctx request.CTX, job *model.Compliance) *model.AppError {
	// Adhoc reports run asynchronously after the HTTP response. Their lifetime
	// follows the server, because the request context expires before export ends.
	return s.runComplianceJob(s.app.Srv().Platform().GoContext(), rctx, job)
}

func (s *Service) runComplianceJob(ctx context.Context, rctx request.CTX, job *model.Compliance) *model.AppError {
	if job == nil {
		return model.NewAppError("Compliance.RunComplianceJob", "app.compliance.save.saving.app_error", nil, "compliance job is nil", http.StatusBadRequest)
	}
	if err := s.checkComplianceExportEnabled(); err != nil {
		return model.NewAppError("Compliance.RunComplianceJob", "ent.compliance.licence_disable.app_error", nil, "", http.StatusNotImplemented)
	}
	execution, acquired, err := s.acquireAdhocExecution(ctx, job.Id)
	if err != nil {
		return complianceAppError("unable to own adhoc compliance report", err)
	}
	if !acquired {
		// Another compatible process is already responsible for this ID.
		return nil
	}
	defer execution.Close()
	saved, err := execution.loadAdhoc()
	if err != nil {
		return complianceAppError("unable to load saved adhoc compliance report", err)
	}
	// Never let delayed callers replace authoritative filters or replay a
	// confirmed result. Only the saved report, under its own lock, can run.
	*job = *saved
	if saved.Status == model.ComplianceStatusFinished || saved.Status == model.ComplianceStatusRemoved {
		return nil
	}
	return s.runComplianceJobWithExecution(execution.ctx, rctx, job, execution)
}

func (s *Service) runComplianceJobWithExecution(ctx context.Context, rctx request.CTX, job *model.Compliance, execution *complianceExecution) *model.AppError {
	if job == nil {
		return model.NewAppError("Compliance.RunComplianceJob", "app.compliance.save.saving.app_error", nil, "compliance job is nil", http.StatusBadRequest)
	}
	if execution == nil {
		return complianceAppError("compliance execution required", errComplianceOwnershipLost)
	}

	cfg := s.app.Config()
	license := s.app.Srv().License()
	if cfg == nil || !model.SafeDereference(cfg.ComplianceSettings.Enable) ||
		license == nil || license.Features == nil || !model.SafeDereference(license.Features.Compliance) {
		return model.NewAppError("Compliance.RunComplianceJob", "ent.compliance.licence_disable.app_error", nil, "", http.StatusNotImplemented)
	}

	job.Status = model.ComplianceStatusRunning
	job.Count = 0
	update := func() error {
		return execution.update(job)
	}
	if err := update(); err != nil {
		return complianceAppError("unable to mark compliance report as running", err)
	}

	fail := func(cause error) *model.AppError {
		job.Status = model.ComplianceStatusFailed
		if updateErr := update(); updateErr != nil {
			rctx.Logger().Error("Unable to mark compliance report as failed", mlog.Err(updateErr))
		}
		return complianceAppError("unable to create compliance report", cause)
	}

	outputDir := filepath.Join(model.SafeDereference(cfg.ComplianceSettings.Directory), "compliance")
	if outputDir != execution.directory {
		return fail(errors.New("compliance directory changed after acquiring the export file lock"))
	}
	outputDir = execution.directory
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return fail(err)
	}
	if err := execution.cleanTemporary(job); err != nil {
		return fail(err)
	}

	tempFile, err := os.CreateTemp(outputDir, job.JobName()+"-*.tmp")
	if err != nil {
		return fail(err)
	}
	tempName := tempFile.Name()
	defer os.Remove(tempName)

	batchSize := model.SafeDereference(cfg.ComplianceSettings.BatchSize)
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	reader := newComplianceReader(cfg.SqlSettings)
	defer reader.Close()

	count, exportErr := writeComplianceArchive(tempFile, job.JobName()+".csv", batchSize, func(cursor model.ComplianceExportCursor, limit int) ([]*model.CompliancePost, model.ComplianceExportCursor, error) {
		if err := ctx.Err(); err != nil {
			return nil, cursor, err
		}
		return s.fetchComplianceBatch(ctx, reader, job, cursor, limit)
	})
	if exportErr != nil {
		_ = tempFile.Close()
		return fail(exportErr)
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fail(err)
	}
	if err := tempFile.Close(); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := s.checkComplianceExportEnabled(); err != nil {
		return fail(err)
	}
	if outputDir != filepath.Join(model.SafeDereference(s.app.Config().ComplianceSettings.Directory), "compliance") {
		return fail(errors.New("compliance directory changed during export"))
	}
	if err := execution.check(); err != nil {
		return fail(err)
	}

	finalName := filepath.Join(outputDir, job.JobName()+".zip")
	if err := os.Rename(tempName, finalName); err != nil {
		return fail(err)
	}

	job.Count = count
	job.Status = model.ComplianceStatusFinished
	if err := update(); err != nil {
		return complianceAppError("unable to mark compliance report as finished", err)
	}
	return nil
}

func (s *Service) checkComplianceExportEnabled() error {
	cfg := s.app.Config()
	license := s.app.Srv().License()
	if cfg == nil || !model.SafeDereference(cfg.ComplianceSettings.Enable) ||
		license == nil || license.Features == nil || !model.SafeDereference(license.Features.Compliance) {
		return errors.New("compliance export is disabled or unlicensed")
	}
	return nil
}

func (s *Service) fetchComplianceBatch(ctx context.Context, reader *complianceReader, job *model.Compliance, cursor model.ComplianceExportCursor, limit int) ([]*model.CompliancePost, model.ComplianceExportCursor, error) {
	if err := s.checkComplianceExportEnabled(); err != nil {
		return nil, cursor, err
	}
	return reader.batch(ctx, job, cursor, limit)
}

func writeComplianceArchive(w io.Writer, csvName string, batchSize int, fetch complianceBatchFetcher) (int, error) {
	if batchSize <= 0 {
		return 0, errors.New("compliance batch size must be positive")
	}

	zipWriter := zip.NewWriter(w)
	csvFile, err := zipWriter.Create(csvName)
	if err != nil {
		return 0, err
	}
	csvWriter := csv.NewWriter(csvFile)
	if err := csvWriter.Write(model.CompliancePostHeader()); err != nil {
		return 0, err
	}

	cursor := model.ComplianceExportCursor{}
	count := 0
	for !cursor.ChannelsQueryCompleted || !cursor.DirectMessagesQueryCompleted {
		previous := cursor
		posts, next, err := fetch(cursor, batchSize)
		if err != nil {
			return count, err
		}
		if next == previous {
			return count, errors.New("compliance export cursor made no progress")
		}
		for _, post := range posts {
			if post == nil {
				continue
			}
			if err := csvWriter.Write(post.Row()); err != nil {
				return count, err
			}
			count++
		}
		cursor = next
	}

	csvWriter.Flush()
	if err := csvWriter.Error(); err != nil {
		return count, err
	}
	if err := zipWriter.Close(); err != nil {
		return count, err
	}
	return count, nil
}

func complianceAppError(message string, cause error) *model.AppError {
	return model.NewAppError(
		"Compliance.RunComplianceJob",
		"app.compliance.save.saving.app_error",
		nil,
		fmt.Sprintf("%s: %v", message, cause),
		http.StatusInternalServerError,
	).Wrap(cause)
}
