package inactive_users

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

type Service struct {
	app     *app.App
	initErr error
}

type deactivateRequest struct {
	Days int `json:"days"`
}

func Register(a *app.App) {
	service := &Service{app: a}
	if err := service.initSessionCleanup(); err != nil {
		service.initErr = err
		a.Log().Error("Unable to initialize inactive-user session cleanup", mlog.Err(err))
	} else {
		a.Srv().Go(service.runSessionCleanup)
	}
	handler := service.deactivateInactiveUsers
	a.Srv().Router.Handle(model.APIURLSuffix+"/users/deactivate/inactive", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodPost)
}

func (s *Service) deactivateInactiveUsers(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionSysconsoleWriteUserManagementUsers) {
		c.SetPermissionError(model.PermissionSysconsoleWriteUserManagementUsers)
		return
	}
	if s.app.License() == nil {
		c.Err = model.NewAppError("DeactivateInactiveUsers", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	if s.initErr != nil {
		c.Err = model.NewAppError("DeactivateInactiveUsers", "app.user.update_active_for_multiple_users.updating.app_error", nil,
			"session cleanup is unavailable", http.StatusServiceUnavailable)
		return
	}

	var req deactivateRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&req)
	if err == nil {
		var trailing any
		if trailingErr := decoder.Decode(&trailing); trailingErr != io.EOF {
			err = trailingErr
			if err == nil {
				err = errors.New("unexpected data after inactivity period")
			}
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("DeactivateInactiveUsers", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("days", err)
		return
	}
	if req.Days <= 0 {
		c.SetInvalidParam("days")
		return
	}

	auditRec := c.MakeAuditRecord(model.AuditEventUpdateUserActive, model.AuditStatusFail)
	defer c.LogAuditRec(auditRec)
	model.AddEventParameterToAuditRec(auditRec, "days", req.Days)

	count, appErr := s.deactivate(c.AppContext, req.Days)
	if appErr != nil {
		c.Err = appErr
		return
	}
	model.AddEventParameterToAuditRec(auditRec, "count", count)
	auditRec.Success()

	if err := json.NewEncoder(w).Encode(map[string]int64{"count": count}); err != nil {
		c.Logger.Warn("Error while writing inactive-user response", mlog.Err(err))
	}
}

func (s *Service) deactivate(rctx request.CTX, days int) (int64, *model.AppError) {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return 0, model.NewAppError("DeactivateInactiveUsers", "app.user.update_active_for_multiple_users.updating.app_error", nil,
			"master database connection is unavailable", http.StatusInternalServerError)
	}

	now := model.GetMillis()
	const millisecondsPerDay int64 = 24 * 60 * 60 * 1000
	// A duration longer than the time since the epoch cannot match any
	// nonnegative LastActivityAt. Avoid overflowing the multiplication, which
	// could otherwise turn an enormous "days" value into a recent threshold.
	if int64(days) > now/millisecondsPerDay {
		return 0, nil
	}
	threshold := now - int64(days)*millisecondsPerDay
	// Expire existing sessions and persist the cleanup intent in the same
	// statement as deactivation. A failed native revocation must not leave an
	// untracked or usable session in the database.
	rows, err := db.QueryContext(rctx.Context(), `
		WITH changed AS (UPDATE users AS u
		SET updateat = $1, deleteat = $1
		WHERE u.deleteat = 0
		  AND u.id IN (
			SELECT s.userid FROM status AS s WHERE s.lastactivityat < $2
		  )
		  AND NOT ('system_admin' = ANY(string_to_array(u.roles, ' ')))
		  AND NOT EXISTS (SELECT 1 FROM bots AS b WHERE b.userid = u.id)
		RETURNING u.id),
		expired AS (UPDATE Sessions AS s SET ExpiresAt=$1
			WHERE s.UserId IN (SELECT id FROM changed)
			  AND (s.ExpiresAt=0 OR s.ExpiresAt>$1)
			RETURNING s.Id)
		INSERT INTO WorkspaceInactiveUserSessionCleanup (UserId, CutoffAt, RetryAt)
		SELECT changed.id, $1, $3 FROM changed CROSS JOIN (SELECT count(*) FROM expired) AS session_expiry
		ON CONFLICT (UserId) DO UPDATE SET CutoffAt=EXCLUDED.CutoffAt, RetryAt=EXCLUDED.RetryAt
		RETURNING UserId
	`, now, threshold, now+sessionCleanupInterval.Milliseconds())
	if err != nil {
		return 0, model.NewAppError("DeactivateInactiveUsers", "app.user.update_active_for_multiple_users.updating.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	defer rows.Close()

	userIDs := make([]string, 0)
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return 0, model.NewAppError("DeactivateInactiveUsers", "app.user.update_active_for_multiple_users.updating.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		}
		userIDs = append(userIDs, userID)
	}
	if err := rows.Err(); err != nil {
		return 0, model.NewAppError("DeactivateInactiveUsers", "app.user.update_active_for_multiple_users.updating.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	if err := rows.Close(); err != nil {
		return 0, model.NewAppError("DeactivateInactiveUsers", "app.user.update_active_for_multiple_users.updating.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	if len(userIDs) > 0 {
		s.app.Srv().Store().User().ClearCaches()
		s.app.Publish(model.NewWebSocketEvent(model.WebsocketEventUserUpdated, "", "", "", nil, ""))
	}
	var cleanupErr *model.AppError
	// Cancellation of the HTTP request cannot abandon already committed users.
	for _, userID := range userIDs {
		// The SQL expiry is authoritative; invalidate cached sessions on every
		// node before trying the native deletion and device cleanup path.
		s.app.ClearSessionCacheForUser(userID)
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(rctx.Context()), 30*time.Second)
		if err := s.revokeInactiveSessions(rctx.WithContext(cleanupCtx), userID, now); err != nil {
			cleanupErr = model.NewAppError("DeactivateInactiveUsers", "app.session.remove.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
			rctx.Logger().Error("Inactive user deactivated but session cleanup remains pending", mlog.String("user_id", userID), mlog.Err(err))
		} else if err := s.deleteSessionCleanup(cleanupCtx, userID, now); err != nil {
			cleanupErr = model.NewAppError("DeactivateInactiveUsers", "app.user.update_active_for_multiple_users.updating.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
			rctx.Logger().Error("Inactive user session cleanup state could not be cleared", mlog.String("user_id", userID), mlog.Err(err))
		}
		cancel()
		s.app.SetStatusOffline(userID, false, true)
	}
	if cleanupErr != nil {
		return int64(len(userIDs)), cleanupErr
	}
	return int64(len(userIDs)), nil
}
