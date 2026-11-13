package data_retention_job

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

var errRetentionFileChanged = errors.New("retained file metadata changed during object deletion")

// Complete only the metadata of an object whose removal has already succeeded.
// A failed execution must never resume policy batches on a replacement session.
// Its caller still receives the original failure even if this completion works.
func (w *Worker) completeDeletedFile(execution *retentionExecution, file *model.FileInfo) error {
	initialErr := execution.deleteFileMetadata(file)
	if initialErr == nil || errors.Is(initialErr, errRetentionFileChanged) {
		return initialErr
	}

	// Release the original lock if the failed statement left its session alive.
	// Never replace that session inside the old execution's database/sql pool.
	execution.Close()
	timeout := 30 * time.Second
	if w.queryTimeout > 0 && w.queryTimeout < timeout {
		timeout = w.queryTimeout
	}
	// Cancellation, shutdown and license removal stop new deletion work. They do
	// not undo an object removal already acknowledged by the file store, so this
	// bounded SQL-only completion must not inherit the execution's cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	completion, err := newRetentionExecution(ctx, w.databaseDSN, timeout)
	if err == nil {
		defer completion.Close()
		err = completion.acquireCompletionLock()
		if err == nil {
			err = completion.deleteFileMetadata(file)
		}
	}
	if err != nil {
		return errors.Join(initialErr, fmt.Errorf("complete removed file metadata under a new retention lock: %w", err))
	}
	return fmt.Errorf("removed file metadata completed under a new retention lock; original execution stopped: %w", initialErr)
}

func (s *retentionExecution) acquireCompletionLock() error {
	for {
		var acquired bool
		ctx, cancel := s.queryContext()
		err := s.db.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, retentionExecutionLock).Scan(&acquired)
		cancel()
		if err != nil {
			return err
		}
		if acquired {
			return nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return s.ctx.Err()
		case <-timer.C:
		}
	}
}

// Check the storage identity and version in the DELETE itself. A successor may
// have completed this exact file while the predecessor was awaiting its HTTP
// reply; an absent row is already complete. A changed row must be preserved.
func (s *retentionExecution) deleteFileMetadata(file *model.FileInfo) error {
	result, err := s.exec(`DELETE FROM FileInfo WHERE Id=$1 AND Path=$2
        AND COALESCE(PreviewPath,'')=$3 AND COALESCE(ThumbnailPath,'')=$4
        AND CreatorId=$5 AND CreateAt=$6 AND UpdateAt=$7 AND DeleteAt=$8`,
		file.Id, file.Path, file.PreviewPath, file.ThumbnailPath,
		file.CreatorId, file.CreateAt, file.UpdateAt, file.DeleteAt)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count > 0 {
		return err
	}
	ctx, cancel := s.queryContext()
	defer cancel()
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM FileInfo WHERE Id=$1)`, file.Id).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return errRetentionFileChanged
	}
	return nil
}
