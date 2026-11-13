package required_password_change

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/mattermost/enterprise/user_no_email"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/app/password/hashers"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const propKey = "require_password_change"

type Service struct {
	app        *app.App
	db         *sql.DB
	initErr    error
	emailUsers *user_no_email.Service
}

type passwordRequest struct {
	CurrentPassword       string `json:"current_password"`
	NewPassword           string `json:"new_password"`
	RequirePasswordChange *bool  `json:"require_password_change"`
}

func Register(a *app.App) {
	s := &Service{app: a, db: a.Srv().Store().GetInternalMasterDB(), emailUsers: user_no_email.NewLocalCreator(a)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	s.initErr = s.initState(ctx)
	cancel()
	if s.initErr != nil {
		a.Log().Error("Unable to initialize required password state", mlog.Err(s.initErr))
	}
	a.Srv().Router.Use(s.enforce)
	s.register(http.MethodPut, model.APIURLSuffix+"/workspace/users/{user_id:[A-Za-z0-9]+}/temporary_password", s.resetByAdmin)
	s.register(http.MethodPut, model.APIURLSuffix+"/workspace/users/me/required_password", s.completeByUser)
	s.register(http.MethodGet, policyRoute, s.getPolicy)
	s.register(http.MethodPut, policyRoute, s.updatePolicy)
	s.register(http.MethodPost, model.APIURLSuffix+"/workspace/users", s.createByAdmin)
}

func (s *Service) register(method, path string, handler func(*web.Context, http.ResponseWriter, *http.Request)) {
	s.app.Srv().Router.Handle(path, &web.Handler{
		Srv:            s.app.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(method)
}

func (s *Service) resetByAdmin(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if c.App.License() == nil {
		c.Err = model.NewAppError("ResetTemporaryPassword", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	userID := mux.Vars(r)["user_id"]
	if !model.IsValidId(userID) || userID == c.AppContext.Session().UserId {
		c.SetInvalidParam("user_id")
		return
	}
	user, appErr := c.App.GetUser(c.AppContext, userID)
	if appErr != nil {
		c.Err = appErr
		return
	}
	if user.IsSystemAdmin() {
		if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionManageSystem) {
			c.SetPermissionError(model.PermissionManageSystem)
			return
		}
	} else if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionSysconsoleWriteUserManagementUsers) {
		c.SetPermissionError(model.PermissionSysconsoleWriteUserManagementUsers)
		return
	}
	request, ok := decodeRequest(c, w, r)
	if !ok {
		return
	}
	if request.NewPassword == "" {
		c.SetInvalidParam("new_password")
		return
	}
	if user.IsBot || user.IsRemote() || user.IsMagicLinkEnabled() {
		c.Err = model.NewAppError("ResetTemporaryPassword", "api.user.update_password.valid_account.app_error", nil, "", http.StatusBadRequest)
		return
	}
	if len(request.NewPassword) > model.PasswordMaximumLength {
		c.Err = model.NewAppError("ResetTemporaryPassword", "model.user.is_valid.pwd_max_length.app_error", nil, "", http.StatusBadRequest)
		return
	}
	policy, policyErr := s.readPolicy(r.Context())
	if policyErr != nil {
		c.Err = stateError(policyErr)
		return
	}
	required := policy.RequireOnAdminReset || (request.RequirePasswordChange != nil && *request.RequirePasswordChange)
	// Relax complexity only for a password that the user must replace.
	if !required {
		if appErr := c.App.IsPasswordValid(c.AppContext, request.NewPassword); appErr != nil {
			c.Err = appErr
			return
		}
	}
	if required {
		if err := s.setRequired(c, user, true); err != nil {
			c.Err = err
			return
		}
	}
	if err := s.updateAdminPassword(c, user, request.NewPassword, required); err != nil {
		// A notification error can occur after the password was saved. Keep the
		// requested requirement in place until the password operation succeeds.
		c.Err = err
		return
	}
	if !required {
		if err := s.setRequired(c, user, false); err != nil {
			c.Err = err
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"OK"}`))
}

func (s *Service) updateAdminPassword(c *web.Context, user *model.User, password string, required bool) *model.AppError {
	hashedPassword, err := hashers.Hash(password)
	if err != nil {
		return model.NewAppError("ResetTemporaryPassword", "api.user.update_password.password_hash.app_error", nil, "user_id="+user.Id, http.StatusInternalServerError).Wrap(err)
	}
	if appErr := c.App.UpdateHashedPasswordByUserId(c.AppContext, user.Id, hashedPassword); appErr != nil {
		return appErr
	}

	if required || *c.App.Config().ServiceSettings.TerminateSessionsOnPasswordChange {
		sessions, appErr := c.App.GetSessions(c.AppContext, user.Id)
		if appErr != nil {
			return appErr
		}
		for _, session := range sessions {
			if appErr := c.App.RevokeSessionById(c.AppContext, session.Id); appErr != nil {
				return appErr
			}
		}
	}

	method := c.AppContext.T("api.user.reset_password.method")
	if strings.TrimSpace(user.Email) == "" {
		return nil
	}
	c.App.Srv().Go(func() {
		if err := c.App.Srv().EmailService.SendPasswordChangeEmail(user.Email, method, user.Locale, c.App.GetSiteURL()); err != nil {
			c.AppContext.Logger().Error("Failed to send password change email", mlog.Err(err))
		}
	})
	return nil
}

func (s *Service) completeByUser(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if c.App.License() == nil {
		c.Err = model.NewAppError("CompleteRequiredPasswordChange", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	request, ok := decodeRequest(c, w, r)
	if !ok {
		return
	}
	if request.CurrentPassword == "" || request.NewPassword == "" {
		c.SetInvalidParam("password")
		return
	}
	user, appErr := c.App.GetUser(c.AppContext, c.AppContext.Session().UserId)
	if appErr != nil {
		c.Err = appErr
		return
	}
	required, version, stateErr := s.requiredState(r.Context(), user.Id)
	if stateErr != nil {
		c.Err = stateError(stateErr)
		return
	}
	if !required {
		c.Err = model.NewAppError("CompleteRequiredPasswordChange", "api.user.update_password.valid_account.app_error", nil, "", http.StatusConflict)
		return
	}
	if err := c.App.UpdatePasswordAsUser(c.AppContext, user.Id, request.CurrentPassword, request.NewPassword); err != nil {
		c.Err = err
		return
	}
	// A concurrent administrative reset must not be cleared by completion of
	// an earlier requirement.
	if err := s.clearRequired(r.Context(), user.Id, version); err != nil {
		c.Err = stateError(err)
		return
	}
	if err := s.setRequiredProperty(c, user, false, version); err != nil {
		c.Err = err
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"OK"}`))
}

func decodeRequest(c *web.Context, w http.ResponseWriter, r *http.Request) (passwordRequest, bool) {
	var request passwordRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil {
		c.SetInvalidParamWithErr("password", err)
		return request, false
	}
	return request, true
}

func (s *Service) setRequired(c *web.Context, user *model.User, required bool) *model.AppError {
	if s.initErr != nil {
		return stateError(s.initErr)
	}
	ctx, cancel := context.WithTimeout(c.AppContext.Context(), 3*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO WorkspaceRequiredPasswordChange (UserId, Required, Version) VALUES ($1, $2, $3)
 ON CONFLICT (UserId) DO UPDATE SET Required=EXCLUDED.Required, Version=EXCLUDED.Version`, user.Id, required, model.NewId())
	if err != nil {
		return stateError(err)
	}
	return s.setRequiredProperty(c, user, required)
}

// Props is a presentation hint only. Authorization always reads private state.
func (s *Service) setRequiredProperty(c *web.Context, user *model.User, required bool, expectedVersion ...string) *model.AppError {
	// Merge only this property's JSON atomically for every account. This also
	// supports accounts without email and preserves concurrent unrelated Props.
	var props []byte
	var updateAt int64
	version := ""
	if len(expectedVersion) > 0 {
		version = expectedVersion[0]
	}
	err := c.App.Srv().Store().GetInternalMasterDB().QueryRowContext(c.AppContext.Context(), `
  UPDATE users SET props = CASE WHEN $2 THEN
   COALESCE(props::jsonb, '{}'::jsonb) || jsonb_build_object($3::text, 'true'::text)
   ELSE COALESCE(props::jsonb, '{}'::jsonb) - $3::text END,
   updateat = $4
  WHERE id = $1 AND ($5::text = '' OR EXISTS (
   SELECT 1 FROM WorkspaceRequiredPasswordChange state WHERE state.UserId=users.id
   AND state.Version=$5 AND state.Required=false))
  RETURNING props, updateat
 `, user.Id, required, propKey, model.GetMillis(), version).Scan(&props, &updateAt)
	if err != nil {
		return model.NewAppError("SetRequiredPasswordChange", "app.user.save.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	updated := user.DeepCopy()
	updated.Props = nil
	if err := json.Unmarshal(props, &updated.Props); err != nil {
		return model.NewAppError("SetRequiredPasswordChange", "app.user.save.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	updated.UpdateAt = updateAt
	c.App.InvalidateCacheForUser(user.Id)
	updated.Sanitize(nil)
	event := model.NewWebSocketEvent(model.WebsocketEventUserUpdated, "", "", user.Id, nil, "")
	event.Add("user", updated)
	c.App.Publish(event)
	return nil
}

func withRequiredPasswordChange(existing model.StringMap, required bool) model.StringMap {
	props := model.CopyStringMap(existing)
	if props == nil {
		props = model.StringMap{}
	}
	if required {
		props[propKey] = "true"
	} else {
		delete(props, propKey)
	}
	return props
}
