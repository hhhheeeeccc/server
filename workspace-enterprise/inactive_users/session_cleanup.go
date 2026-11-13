package inactive_users

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
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(6492261407)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS WorkspaceInactiveUserSessionCleanup (
		UserId varchar(26) PRIMARY KEY, CutoffAt bigint NOT NULL, RetryAt bigint NOT NULL)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_workspace_inactive_user_session_cleanup_retry
		ON WorkspaceInactiveUserSessionCleanup (RetryAt,UserId)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) deleteSessionCleanup(ctx context.Context, userID string, cutoff int64) error {
	_, err := s.app.Srv().Store().GetInternalMasterDB().ExecContext(ctx,
		`DELETE FROM WorkspaceInactiveUserSessionCleanup WHERE UserId=$1 AND CutoffAt=$2`, userID, cutoff)
	return err
}

func (s *Service) runSessionCleanup() {
	ctx := s.app.Srv().Platform().GoContext()
	ticker := time.NewTicker(sessionCleanupInterval)
	defer ticker.Stop()
	for {
		if err := s.retryPendingSessionCleanup(ctx); err != nil && ctx.Err() == nil {
			s.app.Log().Warn("Unable to retry inactive-user session cleanup", mlog.Err(err))
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
		`SELECT UserId FROM WorkspaceInactiveUserSessionCleanup WHERE RetryAt <= $1 ORDER BY RetryAt,UserId LIMIT 50`, model.GetMillis())
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
	published := false
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

		err := db.QueryRowContext(ctx, `WITH candidate AS (
            SELECT UserId, RetryAt FROM WorkspaceInactiveUserSessionCleanup WHERE UserId=$1 AND RetryAt<=$2
            FOR UPDATE SKIP LOCKED
        ) UPDATE WorkspaceInactiveUserSessionCleanup AS cleanup SET RetryAt=$3
          FROM candidate WHERE cleanup.UserId=candidate.UserId
          RETURNING cleanup.CutoffAt `, userID, now, lease).Scan(&cutoff)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if !published {
			s.app.Srv().Store().User().ClearCaches()
			s.app.Publish(model.NewWebSocketEvent(model.WebsocketEventUserUpdated, "", "", "", nil, ""))
			published = true
		}
		workCtx, cancel := context.WithTimeout(ctx, sessionCleanupWorkTimeout)
		rctx := request.EmptyContext(s.app.Log()).WithContext(workCtx)
		cleanupErr := s.revokeInactiveSessions(rctx, userID, cutoff)
		cancel()
		if cleanupErr != nil {
			s.app.Log().Warn("Inactive-user session cleanup remains pending", mlog.String("user_id", userID), mlog.Err(cleanupErr))
			if _, err := db.ExecContext(ctx, `UPDATE WorkspaceInactiveUserSessionCleanup SET RetryAt=$4
                WHERE UserId=$1 AND CutoffAt=$2 AND RetryAt=$3`, userID, cutoff, lease, model.GetMillis()+sessionCleanupInterval.Milliseconds()); err != nil {
				return err
			}
			continue
		}
		result, err := db.ExecContext(ctx, `DELETE FROM WorkspaceInactiveUserSessionCleanup
            WHERE UserId=$1 AND CutoffAt=$2 AND RetryAt=$3`, userID, cutoff, lease)
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return err
		} else if affected > 0 {
			s.app.SetStatusOffline(userID, false, true)
		}
	}
	return nil
}

func (s *Service) revokeInactiveSessions(rctx request.CTX, userID string, cutoff int64) error {
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
	// Session().GetSessions reads a replica. An empty lagging result must not
	// clear the durable cleanup intent while sessions still exist on the master.
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
