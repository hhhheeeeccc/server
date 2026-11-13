package content_extraction

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

type cacheIntent struct{ postID, version string }

// Only the worker goroutine reads or changes cacheSchemaDSN. Recheck the
// schema when the primary changes, or when a replay fails. Never hold an
// extraction job's owning session while preparing this package's schema.
func (w *worker) prepareCacheOutbox(ctx context.Context) error {
	dsn := *w.service.app.Config().SqlSettings.DataSource
	if w.cacheSchemaDSN == dsn {
		return nil
	}
	session := w.service.database("cache_schema")
	session.readOnly = false
	defer session.Close()
	err := session.query(ctx, func(ctx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, extractionLock("cache-outbox-schema-v1")); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS WorkspaceExtractionCacheOutbox (
            PostId varchar(26) PRIMARY KEY, Version varchar(26) NOT NULL, QueuedAt bigint NOT NULL)`); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_workspace_extraction_cache_outbox_queue
            ON WorkspaceExtractionCacheOutbox (QueuedAt,PostId)`); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err == nil {
		w.cacheSchemaDSN = dsn
	}
	return err
}

// A newer content commit for this post replaces the version. A delayed
// acknowledgement of an older invalidation must leave that new intent intact.
func (s *databaseSession) ackCacheIntent(ctx context.Context, intent cacheIntent) error {
	return s.query(ctx, func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, `DELETE FROM WorkspaceExtractionCacheOutbox WHERE PostId=$1 AND Version=$2`, intent.postID, intent.version)
		return err
	})
}

func (e *jobExecution) ackCacheIntent(ctx context.Context, intent cacheIntent) error {
	if err := e.session.ackCacheIntent(ctx, intent); err != nil {
		e.Close()
		return fmt.Errorf("%w: acknowledge attachment cache invalidation: %v", errOwnershipLost, err)
	}
	return nil
}

// Replay one bounded batch, without depending on a job being recoverable or
// reextracting its files. All SQL uses one private cancellable connection.
// The native cache API is synchronous and has no cancellation/ACK contract;
// return from that call is the same delivery boundary as the native App path.
func (s *Service) replayCacheOutbox(ctx context.Context) error {
	session := s.database("cache_replay")
	session.readOnly = false
	defer session.Close()
	var intents []cacheIntent
	err := session.query(ctx, func(ctx context.Context, db *sql.DB) error {
		rows, err := db.QueryContext(ctx, `SELECT PostId,Version FROM WorkspaceExtractionCacheOutbox ORDER BY QueuedAt,PostId LIMIT 100`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var intent cacheIntent
			if err := rows.Scan(&intent.postID, &intent.version); err != nil {
				return err
			}
			intents = append(intents, intent)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.app.License() == nil {
			return nil
		}
		s.app.Srv().Store().FileInfo().InvalidateFileInfosForPostCache(intent.postID, false)
		if err := session.ackCacheIntent(ctx, intent); err != nil {
			return err
		}
	}
	return nil
}

func (w *worker) maintain(ctx context.Context) {
	if ctx.Err() != nil || w.service.app.License() == nil {
		return
	}
	if err := w.prepareCacheOutbox(ctx); err != nil {
		if ctx.Err() == nil {
			w.service.app.Srv().Log().Warn("Unable to prepare content extraction cache outbox", mlog.Err(err))
		}
	} else if err := w.service.replayCacheOutbox(ctx); err != nil {
		w.cacheSchemaDSN = ""
		if ctx.Err() == nil {
			w.service.app.Srv().Log().Warn("Unable to replay content extraction cache invalidations", mlog.Err(err))
		}
	}
	w.recoverJobs(ctx)
}
