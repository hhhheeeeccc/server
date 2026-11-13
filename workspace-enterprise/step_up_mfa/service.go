package step_up_mfa

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

const (
	codeHeader       = "X-MFA-Code"
	verifiedAtProp   = "workspace_step_up_mfa_verified_at"
	maxInspectedBody = 1 << 20
)

type Service struct {
	app     *app.App
	db      *sql.DB
	initErr error
}

func loadPolicySettings() PolicySettings {
	minutes := 30
	if raw := os.Getenv("WORKSPACE_STEP_UP_MFA_GRACE_MINUTES"); raw != "" {
		minutes, _ = strconv.Atoi(raw)
	}
	return PolicySettings{
		TeamDeletion:       enabledByDefault("WORKSPACE_STEP_UP_MFA_TEAM_DELETION"),
		UserDeletion:       enabledByDefault("WORKSPACE_STEP_UP_MFA_USER_DELETION"),
		UserPasswordChange: enabledByDefault("WORKSPACE_STEP_UP_MFA_USER_PASSWORD_CHANGE"),
		UserMfaRemoval:     enabledByDefault("WORKSPACE_STEP_UP_MFA_USER_MFA_REMOVAL"),
		ConfigDisable:      enabledByDefault("WORKSPACE_STEP_UP_MFA_CONFIG_DISABLE"),
		GraceMinutes:       max(0, min(minutes, 1440)),
	}
}

func enabledByDefault(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return true
	}
	enabled, err := strconv.ParseBool(value)
	return err != nil || enabled
}

func Register(a *app.App) *Service {
	s := &Service{
		app: a,
		db:  a.Srv().Store().GetInternalMasterDB(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), policyTimeout)
	s.initErr = s.initPolicy(ctx, loadPolicySettings())
	cancel()
	if s.initErr != nil {
		a.Log().Error("Unable to initialize step-up MFA policy", mlog.Err(s.initErr))
	}
	s.registerRoutes()
	a.Srv().Router.Use(s.enforce)
	return s
}

func (s *Service) enforce(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.app.License() == nil || !model.SafeDereference(s.app.Config().ServiceSettings.EnableMultifactorAuthentication) || !isCandidate(r) {
			next.ServeHTTP(w, r)
			return
		}
		token, _ := app.ParseAuthTokenFromRequest(r)
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		session, appErr := s.app.GetSession(token)
		if appErr != nil || session == nil || session.Local {
			next.ServeHTTP(w, r)
			return
		}
		rctx := request.EmptyContext(s.app.Log()).WithContext(r.Context()).With(app.RequestContextWithMaster).WithSession(session)
		current, err := s.readPolicy(r.Context())
		if err != nil {
			writeError(w, model.NewAppError("StepUpMFA", "ent.step_up_mfa.policy_unavailable", nil, err.Error(), http.StatusServiceUnavailable))
			return
		}
		required, forceFresh, inspectErr := s.requiresVerification(rctx, session, r, current.PolicySettings)
		if inspectErr != nil {
			writeError(w, inspectErr)
			return
		}
		if !required {
			next.ServeHTTP(w, r)
			return
		}
		user, appErr := s.app.GetUser(rctx, session.UserId)
		if appErr != nil {
			writeError(w, appErr)
			return
		}
		if !user.MfaActive || !model.SafeDereference(s.app.Config().ServiceSettings.EnableMultifactorAuthentication) {
			next.ServeHTTP(w, r)
			return
		}
		now := time.Now().Unix()
		verifiedAt, _ := strconv.ParseInt(session.Props[verifiedAtProp], 10, 64)
		grace := time.Duration(current.GraceMinutes) * time.Minute
		if !forceFresh && grace > 0 && verifiedAt > 0 && verifiedAt <= now && now-verifiedAt <= int64(grace.Seconds()) {
			next.ServeHTTP(w, r)
			return
		}
		code := r.Header.Get(codeHeader)
		if code == "" {
			writeError(w, model.NewAppError("StepUpMFA", "ent.step_up_mfa.required", nil, "MFA verification is required", http.StatusForbidden))
			return
		}
		if appErr = s.app.CheckUserMfa(rctx, user, code); appErr != nil {
			writeError(w, appErr)
			return
		}
		if appErr = s.app.SetExtraSessionProps(session, map[string]string{verifiedAtProp: strconv.FormatInt(now, 10)}); appErr != nil {
			writeError(w, appErr)
			return
		}
		s.app.ClearSessionCacheForUser(session.UserId)
		next.ServeHTTP(w, r)
	})
}

func writeError(w http.ResponseWriter, appErr *model.AppError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(appErr.StatusCode)
	_ = json.NewEncoder(w).Encode(appErr)
}

func apiParts(path string) []string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "api" && parts[i+1] == "v4" {
			return parts[i+2:]
		}
	}
	return nil
}

func isCandidate(r *http.Request) bool {
	parts := apiParts(r.URL.Path)
	if len(parts) == 4 && parts[0] == "workspace" && parts[1] == "users" && parts[3] == "temporary_password" && model.IsValidId(parts[2]) {
		parts = []string{"users", parts[2], "password"}
	}
	if len(parts) == 2 && parts[0] == "teams" && model.IsValidId(parts[1]) {
		return r.Method == http.MethodDelete
	}
	if len(parts) == 2 && parts[0] == "users" && model.IsValidId(parts[1]) {
		return r.Method == http.MethodDelete
	}
	if len(parts) == 3 && parts[0] == "users" && model.IsValidId(parts[1]) {
		return r.Method == http.MethodPut && (parts[2] == "active" || parts[2] == "password" || parts[2] == "mfa")
	}
	if r.Method == http.MethodPut && (len(parts) == 1 && parts[0] == "config" || len(parts) == 2 && parts[0] == "config" && parts[1] == "patch") {
		return true
	}
	if r.Method == http.MethodPut && len(parts) == 3 && parts[0] == "workspace" && parts[1] == "step_up_mfa" && parts[2] == "policy" {
		return true
	}
	return false
}

func (s *Service) requiresVerification(rctx request.CTX, session *model.Session, r *http.Request, policy PolicySettings) (bool, bool, *model.AppError) {
	parts := apiParts(r.URL.Path)
	if len(parts) == 4 && parts[0] == "workspace" && parts[1] == "users" && parts[3] == "temporary_password" && model.IsValidId(parts[2]) {
		parts = []string{"users", parts[2], "password"}
	}
	if len(parts) == 3 && parts[0] == "workspace" && parts[1] == "step_up_mfa" && parts[2] == "policy" {
		return s.app.SessionHasPermissionTo(*session, model.PermissionSysconsoleWriteAuthenticationMfa), true, nil
	}
	if len(parts) == 2 && parts[0] == "teams" {
		return policy.TeamDeletion && s.app.SessionHasPermissionToTeam(*session, parts[1], model.PermissionManageTeam), false, nil
	}
	if len(parts) == 2 && parts[0] == "users" {
		if !policy.UserDeletion || session.UserId == parts[1] || !s.app.SessionHasPermissionToUserOrBot(rctx, *session, parts[1]) {
			return false, false, nil
		}
		target, appErr := s.app.GetUser(rctx, parts[1])
		if appErr != nil || target.IsSystemAdmin() && !s.app.SessionHasPermissionTo(*session, model.PermissionManageSystem) {
			return false, false, nil // The original route reports missing or forbidden targets.
		}
		return true, false, nil
	}
	if len(parts) == 3 && parts[0] == "users" {
		if session.UserId == parts[1] {
			return false, false, nil
		}
		target, appErr := s.app.GetUser(rctx, parts[1])
		if appErr != nil {
			return false, false, nil // Let the original route report missing or forbidden users.
		}
		switch parts[2] {
		case "password":
			if !policy.UserPasswordChange {
				return false, false, nil
			}
			permission := model.PermissionSysconsoleWriteUserManagementUsers
			if target.IsSystemAdmin() {
				permission = model.PermissionManageSystem
			}
			return s.app.SessionHasPermissionTo(*session, permission), false, nil
		case "active":
			if !policy.UserDeletion {
				return false, false, nil
			}
			if !s.app.SessionHasPermissionTo(*session, model.PermissionSysconsoleWriteUserManagementUsers) ||
				target.IsSystemAdmin() && !s.app.SessionHasPermissionTo(*session, model.PermissionManageSystem) {
				return false, false, nil
			}
			var body struct {
				Active *bool `json:"active"`
			}
			if err := inspectBody(r, &body); err != nil {
				return false, false, err
			}
			return body.Active != nil && !*body.Active, false, nil
		case "mfa":
			// The native route rejects OAuth sessions before changing MFA.
			// Do not challenge a request that cannot reach the operation.
			if !policy.UserMfaRemoval || session.IsOAuth {
				return false, false, nil
			}
			if !s.app.SessionHasPermissionToUser(rctx, *session, parts[1]) {
				return false, false, nil
			}
			var body struct {
				Activate *bool `json:"activate"`
			}
			if err := inspectBody(r, &body); err != nil {
				return false, false, err
			}
			return body.Activate != nil && !*body.Activate, false, nil
		}
	}
	if !policy.ConfigDisable ||
		!s.app.SessionHasPermissionTo(*session, model.PermissionSysconsoleWriteAuthenticationMfa) ||
		!model.SafeDereference(s.app.Config().ServiceSettings.EnableMultifactorAuthentication) {
		return false, false, nil
	}
	var body struct {
		ServiceSettings struct {
			EnableMultifactorAuthentication *bool
		}
	}
	if err := inspectBody(r, &body); err != nil {
		return false, false, err
	}
	if body.ServiceSettings.EnableMultifactorAuthentication == nil {
		return len(parts) == 1, false, nil // A full config PUT defaults an omitted setting to false.
	}
	return !*body.ServiceSettings.EnableMultifactorAuthentication, false, nil
}

func inspectBody(r *http.Request, dst any) *model.AppError {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInspectedBody+1))
	if err != nil {
		return model.NewAppError("StepUpMFA", "ent.step_up_mfa.invalid_request", nil, "", http.StatusBadRequest)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) > maxInspectedBody {
		return model.NewAppError("StepUpMFA", "ent.step_up_mfa.request_too_large", nil, "", http.StatusRequestEntityTooLarge)
	}
	if err = json.Unmarshal(body, dst); err != nil {
		return model.NewAppError("StepUpMFA", "ent.step_up_mfa.invalid_request", nil, "", http.StatusBadRequest)
	}
	return nil
}
