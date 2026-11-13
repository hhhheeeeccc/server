package disappearing_messages

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

// CheckPostAccess enforces the same durable deadline for server-side consumers
// that do not download through the native file HTTP middleware. Callers still
// own permission checks. Existing deadlines apply even after a license expires.
func CheckPostAccess(ctx context.Context, a *app.App, postID string) *model.AppError {
	registered, ok := services.Load(a.Srv())
	if !ok {
		return nil // no disappearing-message service was installed on this server
	}
	s := registered.(*Service)
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var expired bool
	err := s.repo.db.QueryRowContext(ctx, `SELECT expires_at<=floor(extract(epoch FROM clock_timestamp())*1000)::bigint FROM workspace_message_expiry.messages WHERE post_id=$1`, postID).Scan(&expired)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return model.NewAppError("CheckPostAccess", "app.post.get.app_error", nil, "", http.StatusServiceUnavailable).Wrap(err)
	}
	if expired {
		return model.NewAppError("CheckPostAccess", "app.post.get.app_error", nil, "", http.StatusNotFound)
	}
	return nil
}
