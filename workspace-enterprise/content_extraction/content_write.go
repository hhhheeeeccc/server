package content_extraction

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/platform/services/docextractor"
)

const maxContentBytes = 1024 * 1024 // Match the native App extraction cap.

func (s *Service) extractContent(rctx request.CTX, file *model.FileInfo, catchup bool) (string, error) {
	if file.IsImage() {
		return "", nil
	}
	logger := rctx.Logger().With(mlog.String("file_info_id", file.Id))
	logger.Debug("Extracting content from file", mlog.String("extension", file.Extension))
	reader, appErr := s.app.FileReader(file.Path)
	if appErr != nil {
		return "", fmt.Errorf("open file for content extraction: %w", appErr)
	}
	// Keep the native converter and its reader ownership: a detached converter
	// may still use this reader after its canceled caller has returned.
	settings := s.app.Config().FileSettings
	text, err := docextractor.Extract(logger, file.Name, reader, docextractor.ExtractSettings{
		Ctx: rctx.Context(), ArchiveRecursion: *settings.ArchiveRecursion,
		MaxFileSize: *settings.MaxFileSize, Timeout: time.Duration(*settings.ExtractContentTimeout) * time.Second,
		ReaderCloser: reader,
	})
	if err != nil {
		return "", fmt.Errorf("extract file content: %w", err)
	}
	if err := rctx.Context().Err(); err != nil {
		return "", err
	}
	if err := s.gate(catchup); err != nil {
		return "", err
	}
	if text == "" {
		return "", nil
	}
	if len(text) > maxContentBytes {
		end := maxContentBytes
		for end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
		text = text[:end]
	}
	// Reject malformed converter output before an owning SQL mutation. A
	// PostgreSQL encoding error would close that lease just like a lost reply.
	if text == "" || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return "", errors.New("extracted content is not valid PostgreSQL UTF-8 text")
	}
	return text, nil
}

func (e *jobExecution) completeFile(ctx context.Context, job *model.Job, checkpoint *fileCheckpoint, file *model.FileInfo, text string, counted, failed bool) (cacheIntent, error) {
	if !e.held || e.token == "" || e.id != job.Id || job.Data[attemptKey] != e.token {
		return cacheIntent{}, errOwnershipLost
	}
	next := *checkpoint
	next.At, next.ID = file.CreateAt, file.Id
	if counted {
		next.Processed++
	}
	if failed {
		next.Errors++
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return cacheIntent{}, err
	}
	intent := cacheIntent{version: model.NewId()}
	var owned, written bool
	var postID, status string
	var savedData []byte
	// Use the same physical connection that owns the advisory lock. No other
	// participating owner can claim this job while this statement is executing.
	// Lock the matching Job before touching FileInfo, then commit its cursor,
	// counters, content and cache intent together. Concurrent cancellation waits
	// for this bounded SQL statement; an earlier cancellation prevents the write.
	err = e.session.query(ctx, func(ctx context.Context, db *sql.DB) error {
		return db.QueryRowContext(ctx, `WITH current_attempt AS MATERIALIZED (
            SELECT Status FROM Jobs WHERE Id=$1 AND Type=$3 AND Data->>$5=$6 FOR UPDATE
        ), owned AS MATERIALIZED (
            SELECT 1 FROM current_attempt WHERE Status=$4
        ), updated AS (
            UPDATE FileInfo f SET Content=$2 WHERE f.Id=$7 AND f.DeleteAt=0
            AND f.CreateAt=$8 AND f.UpdateAt=$9 AND f.Path=$10 AND f.Name=$11
            AND f.Extension=$12 AND f.MimeType=$13 AND f.Size=$14
            AND $2<>'' AND EXISTS(SELECT 1 FROM owned)
            RETURNING COALESCE(f.PostId,'') AS postid
		), queued AS (
            INSERT INTO WorkspaceExtractionCacheOutbox (PostId,Version,QueuedAt)
            SELECT postid,$15,$16 FROM updated WHERE postid<>''
            ON CONFLICT (PostId) DO UPDATE SET Version=EXCLUDED.Version
        ), outcome AS MATERIALIZED (
            SELECT $17::jsonb || jsonb_build_object('errors', ($17::jsonb->>'errors')::bigint
                + CASE WHEN $2<>'' AND NOT EXISTS(SELECT 1 FROM updated) THEN 1 ELSE 0 END) AS checkpoint
        ), checkpointed AS (
            UPDATE Jobs SET LastActivityAt=$16, Data=Data||jsonb_build_object(
                $18::text,(SELECT checkpoint::text FROM outcome),
                'processed',(SELECT checkpoint->>'processed' FROM outcome),
                'errors',(SELECT checkpoint->>'errors' FROM outcome))
            WHERE Id=$1 AND Type=$3 AND Status=$4 AND Data->>$5=$6 AND EXISTS(SELECT 1 FROM owned)
            RETURNING Data
        ) SELECT EXISTS(SELECT 1 FROM owned),EXISTS(SELECT 1 FROM updated),
            COALESCE((SELECT postid FROM updated),''),COALESCE((SELECT Status FROM current_attempt),''),
            COALESCE((SELECT Data FROM checkpointed),'{}'::jsonb)`,
			e.id, text, model.JobTypeExtractContent, model.JobStatusInProgress, attemptKey, e.token,
			file.Id, file.CreateAt, file.UpdateAt, file.Path, file.Name, file.Extension, file.MimeType, file.Size,
			intent.version, model.GetMillis(), string(encoded), checkpointKey,
		).Scan(&owned, &written, &postID, &status, &savedData)
	})
	if err != nil {
		e.Close()
		return cacheIntent{}, errors.Join(errOwnershipLost, fmt.Errorf("commit extracted file outcome: %w", err))
	}
	if !owned {
		if status == model.JobStatusCancelRequested || status == model.JobStatusCanceled {
			// The same token still owns a healthy locked session. Keep it
			// available to persist cancellation through the terminal CAS.
			return cacheIntent{}, &interruption{status: model.JobStatusCanceled}
		}
		e.Close()
		return cacheIntent{}, errOwnershipLost
	}
	var data model.StringMap
	if err := json.Unmarshal(savedData, &data); err != nil || data[attemptKey] != e.token || data[checkpointKey] == "" {
		e.Close()
		return cacheIntent{}, errOwnershipLost
	}
	if text != "" && !written {
		next.Errors++
	}
	job.Data = data
	*checkpoint = next
	if text != "" && !written {
		return cacheIntent{}, errors.New("file metadata changed or file was deleted during content extraction")
	}
	intent.postID = postID
	return intent, nil
}
