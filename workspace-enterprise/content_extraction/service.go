package content_extraction

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/mattermost/mattermost/server/v8/channels/jobs/extract_content"
)

const batchSize = 1000

type Service struct {
	app *app.App
}

// Register must run after initJobs and before workers/schedulers start. The
// central IPFiltering constructor has this ordering in the supported core.
// Other database drivers retain their native worker until independently tested.
func Register(a *app.App) {
	if a.Srv().Jobs == nil {
		panic("content_extraction registration must follow job initialization")
	}
	if model.SafeDereference(a.Config().SqlSettings.DriverName) != model.DatabaseDriverPostgres {
		return
	}
	s := New(a)
	a.Srv().Jobs.RegisterJobType(model.JobTypeExtractContent, s.MakeWorker(), s.MakeScheduler())
}

func New(a *app.App) *Service { return &Service{app: a} }

func (s *Service) MakeScheduler() jobs.Scheduler {
	return &scheduler{Scheduler: extract_content.MakeScheduler(s.app.Srv().Jobs), service: s}
}

// Keep the worker's transport alive when a license is installed or removed at
// runtime. All execution and scheduling is gated on the current license.
func (s *Service) MakeWorker() model.Worker {
	return &worker{service: s, candidates: make(chan model.Job), run: newWorkerRun()}
}

func (s *Service) gate(catchup bool) error {
	if s.app.License() == nil {
		return model.NewAppError("EnterpriseExtractContent", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
	}
	if catchup && !model.SafeDereference(s.app.Config().FileSettings.ExtractContent) {
		return errors.New("automatic content extraction is disabled")
	}
	return nil
}

func (s *Service) execute(ctx context.Context, logger mlog.LoggerIFace, job *model.Job, owner *jobExecution) error {
	if job.Data == nil {
		job.Data = model.StringMap{}
	}
	catchup := job.Data["catchup"] == "true"
	if _, saved := job.Data[checkpointKey]; !saved {
		job.Data["processed"], job.Data["errors"] = "0", "0"
	}
	if err := s.gate(catchup); err != nil {
		return err
	}
	from, to, err := jobRange(job.Data, catchup)
	if err != nil {
		return err
	}
	checkpoint, err := loadCheckpoint(job, from, to, catchup)
	if err != nil {
		return err
	}
	checkpoint.counters(job.Data)
	if err := owner.progress(ctx, job); err != nil {
		return err
	}
	cursorTime, cursorID := checkpoint.At, checkpoint.ID
	rctx := request.EmptyContext(logger).WithContext(ctx)
	reader := s.database("reader")
	defer reader.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.gate(catchup); err != nil {
			return err
		}
		files, err := s.readBatch(rctx.Context(), reader, cursorTime, cursorID, to, catchup)
		if err != nil {
			return err
		}
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.gate(catchup); err != nil {
				return err
			}
			counted := !ignoredExtension(file.Extension)
			var text string
			var extractErr error
			if counted {
				text, extractErr = s.extractContent(rctx, file, catchup)
			}
			if ctx.Err() != nil {
				// An interrupted attempt is neither a completed file nor an
				// extraction failure. Its native converter may still be reading.
				return ctx.Err()
			}
			if s.gate(catchup) != nil {
				return &interruption{status: model.JobStatusPending}
			}
			var interrupted *interruption
			if errors.As(extractErr, &interrupted) {
				return extractErr
			}
			intent, err := owner.completeFile(ctx, job, &checkpoint, file, text, counted, extractErr != nil)
			if errors.Is(err, errOwnershipLost) || errors.As(err, &interrupted) {
				return err
			}
			if err != nil || extractErr != nil {
				logger.Warn("Failed to extract file content", mlog.Err(errors.Join(extractErr, err)), mlog.String("file_info_id", file.Id))
			}
			if intent.postID != "" {
				// The durable intent survives a lost acknowledgement or process.
				s.app.Srv().Store().FileInfo().InvalidateFileInfosForPostCache(intent.postID, false)
				if err := owner.ackCacheIntent(ctx, intent); err != nil {
					return err
				}
			}
			cursorTime, cursorID = checkpoint.At, checkpoint.ID
		}
		if len(files) < batchSize {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if checkpoint.Errors > 0 {
		return fmt.Errorf("content extraction failed for %d of %d files", checkpoint.Errors, checkpoint.Processed)
	}
	return nil
}

// Persist the exact millisecond window before extracting any file. A crashed
// catch-up must not silently move past its original 24-hour lower boundary.
const rangeFromKey = "workspace_extraction_from_v1"
const rangeToKey = "workspace_extraction_to_v1"

func jobRange(data model.StringMap, catchup bool) (int64, int64, error) {
	if data[rangeFromKey] != "" || data[rangeToKey] != "" {
		from, firstErr := strconv.ParseInt(data[rangeFromKey], 10, 64)
		to, lastErr := strconv.ParseInt(data[rangeToKey], 10, 64)
		if firstErr != nil || lastErr != nil || from < 0 || to < from {
			return 0, 0, errors.New("invalid saved content extraction range")
		}
		return from, to, nil
	}
	from, to, err := extractionRange(data, catchup, model.GetMillis())
	if err != nil {
		return 0, 0, err
	}
	data[rangeFromKey], data[rangeToKey] = strconv.FormatInt(from, 10), strconv.FormatInt(to, 10)
	return from, to, nil
}

// Read only the metadata needed by the native extractor. Catch-up filtering is
// done in SQL, so completed document bodies are not loaded or rescanned in Go.
func (s *Service) readBatch(ctx context.Context, reader *databaseSession, from int64, id string, to int64, catchup bool) ([]*model.FileInfo, error) {
	query := `SELECT Id, CreateAt, Name, Extension, MimeType, Path, UpdateAt, Size FROM FileInfo
		WHERE DeleteAt=0 AND (CreateAt, Id)>($1, $2) AND CreateAt <= $3`
	if catchup {
		query += ` AND (Content IS NULL OR Content='')`
	}
	query += ` ORDER BY CreateAt ASC, Id ASC LIMIT $4`
	files := make([]*model.FileInfo, 0, batchSize)
	err := reader.query(ctx, func(ctx context.Context, db *sql.DB) error {
		rows, err := db.QueryContext(ctx, query, from, id, to, batchSize)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			file := new(model.FileInfo)
			if err := rows.Scan(&file.Id, &file.CreateAt, &file.Name, &file.Extension, &file.MimeType, &file.Path, &file.UpdateAt, &file.Size); err != nil {
				return err
			}
			files = append(files, file)
		}
		return rows.Err()
	})
	return files, err
}

func extractionRange(data model.StringMap, catchup bool, now int64) (int64, int64, error) {
	if catchup {
		return now - (24 * time.Hour).Milliseconds(), now, nil
	}
	from, to := int64(0), now
	for _, bound := range []struct {
		name  string
		value *int64
	}{{"from", &from}, {"to", &to}} {
		if value, ok := data[bound.name]; ok {
			seconds, err := strconv.ParseInt(value, 10, 64)
			if err != nil || seconds < 0 || seconds > (1<<63-1)/1000 {
				return 0, 0, fmt.Errorf("invalid content extraction %s", bound.name)
			}
			*bound.value = seconds * 1000
		}
	}
	if from > to {
		return 0, 0, errors.New("content extraction from must not exceed to")
	}
	return from, to, nil
}

func ignoredExtension(extension string) bool {
	switch strings.ToLower(extension) {
	case "png", "jpg", "jpeg", "gif", "wmv", "mpg", "mpeg", "mp3", "mp4", "ogg", "ogv", "mov", "apk", "svg", "webm", "mkv":
		return true
	}
	return false
}

type scheduler struct {
	jobs.Scheduler
	service *Service
}

func (s *scheduler) Enabled(cfg *model.Config) bool {
	return s.service.app.License() != nil && s.Scheduler.Enabled(cfg)
}

func (s *scheduler) ScheduleJob(rctx request.CTX, cfg *model.Config, pending bool, previous *model.Job) (*model.Job, *model.AppError) {
	if s.service.app.License() == nil {
		return nil, model.NewAppError("EnterpriseExtractContent", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
	}
	return s.Scheduler.ScheduleJob(rctx, cfg, pending, previous)
}
