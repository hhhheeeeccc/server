package account_migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

const (
	sessionCleanupInterval    = time.Minute
	sessionCleanupWorkTimeout = 30 * time.Second
)

func (s *Service) initSessionCleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := s.app.Srv().Store().GetInternalMasterDB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(6492261406)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS WorkspaceMigrationSessionCleanup (
		UserId varchar(26) PRIMARY KEY, CutoffAt bigint NOT NULL, RetryAt bigint NOT NULL)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_workspace_migration_session_cleanup_retry
		ON WorkspaceMigrationSessionCleanup (RetryAt,UserId)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) deleteSessionCleanup(ctx context.Context, userID string, cutoff int64) error {
	_, err := s.app.Srv().Store().GetInternalMasterDB().ExecContext(ctx,
		`DELETE FROM WorkspaceMigrationSessionCleanup WHERE UserId=$1 AND CutoffAt=$2`, userID, cutoff)
	return err
}

func (s *Service) setSessionCleanupCutoff(ctx context.Context, userID string) (int64, error) {
	cutoff := model.GetMillis()
	err := s.app.Srv().Store().GetInternalMasterDB().QueryRowContext(ctx,
		`UPDATE WorkspaceMigrationSessionCleanup SET CutoffAt=$2
		WHERE UserId=$1 AND CutoffAt=0 RETURNING CutoffAt`, userID, cutoff).Scan(&cutoff)
	if errors.Is(err, sql.ErrNoRows) {
		// A worker may already have claimed or completed this job.
		err = s.app.Srv().Store().GetInternalMasterDB().QueryRowContext(ctx,
			`SELECT CutoffAt FROM WorkspaceMigrationSessionCleanup WHERE UserId=$1`, userID).Scan(&cutoff)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
	}
	return cutoff, err
}

func (s *Service) runSessionCleanup() {
	ctx := s.app.Srv().Platform().GoContext()
	ticker := time.NewTicker(sessionCleanupInterval)
	defer ticker.Stop()
	for {
		if err := s.retryPendingSessionCleanup(ctx); err != nil && ctx.Err() == nil {
			s.app.Log().Warn("Unable to retry authentication migration session cleanup", mlog.Err(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Select a bounded candidate list without keeping a connection or row lock
// during native session operations. Each candidate is claimed independently.
func (s *Service) pendingSessionCleanupUsers(ctx context.Context) ([]string, error) {
	rows, err := s.app.Srv().Store().GetInternalMasterDB().QueryContext(ctx,
		`SELECT UserId FROM WorkspaceMigrationSessionCleanup WHERE RetryAt <= $1 ORDER BY RetryAt,UserId LIMIT 50`, model.GetMillis())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]string, 0, 50)
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		users = append(users, userID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return users, nil
}

func (s *Service) retryPendingSessionCleanup(ctx context.Context) error {
	ctx, cancelCycle := context.WithTimeout(ctx, sessionCleanupInterval)
	defer cancelCycle()
	users, err := s.pendingSessionCleanupUsers(ctx)
	if err != nil {
		return err
	}
	db := s.app.Srv().Store().GetInternalMasterDB()

	for _, userID := range users {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := model.GetMillis()
		// RetryAt doubles as an expiring claim. Its exact value and cutoff
		// fence completion against a replacement intent or a later claim.
		// A canceled/unknown commit can leave only a bounded retry delay.
		lease := now + sessionCleanupInterval.Milliseconds()
		var cutoff int64
		var previousRetry int64
		err := db.QueryRowContext(ctx, `WITH candidate AS (
            SELECT UserId, RetryAt FROM WorkspaceMigrationSessionCleanup WHERE UserId=$1 AND RetryAt<=$2
            FOR UPDATE SKIP LOCKED
        ) UPDATE WorkspaceMigrationSessionCleanup AS cleanup SET RetryAt=$3, CutoffAt=CASE WHEN cleanup.CutoffAt=0 THEN $2 ELSE cleanup.CutoffAt END
          FROM candidate WHERE cleanup.UserId=candidate.UserId
          RETURNING cleanup.CutoffAt, candidate.RetryAt`, userID, now, lease).Scan(&cutoff, &previousRetry)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}

		workCtx, cancel := context.WithTimeout(ctx, sessionCleanupWorkTimeout)
		rctx := request.EmptyContext(s.app.Log()).WithContext(workCtx)
		cleanupErr := s.revokeMigratedSessions(rctx, userID, cutoff)
		cancel()
		if cleanupErr != nil {
			s.app.Log().Warn("Authentication migration session cleanup remains pending", mlog.String("user_id", userID), mlog.Err(cleanupErr))
			if _, err := db.ExecContext(ctx, `UPDATE WorkspaceMigrationSessionCleanup SET RetryAt=$4
                WHERE UserId=$1 AND CutoffAt=$2 AND RetryAt=$3`, userID, cutoff, lease, previousRetry); err != nil {
				return err
			}
			continue
		}
		_, err = db.ExecContext(ctx, `DELETE FROM WorkspaceMigrationSessionCleanup
            WHERE UserId=$1 AND CutoffAt=$2 AND RetryAt=$3`, userID, cutoff, lease)
		if err != nil {
			return err
		}

	}
	return nil
}

func (s *Service) revokeMigratedSessions(rctx request.CTX, userID string, cutoff int64) error {
	// RevokeSession can remove a database row before another session fails.
	// Clear cached sessions even when that later revocation must be retried.
	defer s.app.ClearSessionCacheForUser(userID)
	sessions, err := s.sessionsToRevoke(rctx, userID, cutoff)
	if err != nil {
		return fmt.Errorf("list sessions for %s: %w", userID, err)
	}
	var revokeErr error
	for _, session := range sessions {
		if err := rctx.Context().Err(); err != nil {
			return errors.Join(revokeErr, err)
		}
		if appErr := s.app.RevokeSession(rctx, session); appErr != nil {
			revokeErr = errors.Join(revokeErr, fmt.Errorf("revoke session %s: %w", session.Id, appErr))
		}
	}
	return revokeErr
}

func (s *Service) sessionsToRevoke(rctx request.CTX, userID string, cutoff int64) ([]*model.Session, error) {
	// GetSessions reads a replica, which may not contain sessions committed
	// before the migration. Never clear the cleanup intent from that snapshot.
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return nil, errors.New("master database connection is unavailable")
	}
	rows, err := db.QueryContext(rctx.Context(), `SELECT Id, Token, CreateAt, UserId, DeviceId, IsOAuth, Props
		FROM Sessions WHERE UserId=$1 AND CreateAt<=$2`, userID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []*model.Session
	for rows.Next() {
		session := &model.Session{}
		var props []byte
		if err := rows.Scan(&session.Id, &session.Token, &session.CreateAt, &session.UserId,
			&session.DeviceId, &session.IsOAuth, &props); err != nil {
			return nil, err
		}
		if len(props) > 0 {
			if err := json.Unmarshal(props, &session.Props); err != nil {
				return nil, err
			}
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sessions, nil
}
