package required_password_change

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

// Private server-owned rows survive profile PUT/PATCH, session replacement and
// restarts. Retain completed rows so migration cannot resurrect a stale hint.
func (s *Service) initState(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(6492261410)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS WorkspaceRequiredPasswordChange (
 UserId varchar(26) PRIMARY KEY, Required boolean NOT NULL, Version varchar(26) NOT NULL)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO WorkspaceRequiredPasswordChange (UserId, Required, Version)
 SELECT id, true, id FROM users WHERE props::jsonb->>'require_password_change'='true'
 ON CONFLICT (UserId) DO NOTHING`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS WorkspacePasswordPolicy (
 Id integer PRIMARY KEY CHECK (Id=1), RequireOnAdminReset boolean NOT NULL, Version bigint NOT NULL DEFAULT 0)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO WorkspacePasswordPolicy (Id,RequireOnAdminReset) VALUES (1,true) ON CONFLICT (Id) DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) requiredState(ctx context.Context, userID string) (bool, string, error) {
	if s.initErr != nil {
		return false, "", s.initErr
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var required bool
	var version string
	err := s.db.QueryRowContext(ctx, `SELECT Required, Version FROM WorkspaceRequiredPasswordChange WHERE UserId=$1`, userID).Scan(&required, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	return required, version, err
}

func (s *Service) clearRequired(ctx context.Context, userID, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result, err := s.db.ExecContext(ctx, `UPDATE WorkspaceRequiredPasswordChange SET Required=false WHERE UserId=$1 AND Version=$2 AND Required=true`, userID, version)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("password requirement changed during completion")
	}
	return nil
}

func stateError(err error) *model.AppError {
	return model.NewAppError("RequiredPasswordChange", "api.user.update_password.valid_account.app_error", nil, "Required password state unavailable", http.StatusServiceUnavailable).Wrap(err)
}

// Limit exemptions to resources needed to display and finish rotation, enroll
// mandatory MFA, or end the session. No profile mutation or normal API is exempt.
func rotationExempt(r *http.Request, userID string) bool {
	path := r.URL.Path
	start := strings.Index(path, model.APIURLSuffix+"/")
	if strings.Contains(path[:max(0, start)], "/plugins/") {
		return false
	}
	if start < 0 {
		return (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.Contains(path, "/plugins/")
	} // static assets and page entry points
	path = strings.TrimPrefix(path[start:], model.APIURLSuffix)
	if r.Method == http.MethodPut && path == "/workspace/users/me/required_password" {
		return true
	}
	if r.Method == http.MethodPost && path == "/users/logout" {
		return true
	}
	if r.Method == http.MethodGet {
		switch path {
		case "/users/me", "/users/" + userID, "/config/client", "/license/client", "/plugins/webapp":
			return true
		}
	}
	// MFA enrollment belongs to the authenticated user only; the native handlers
	// continue to enforce the current-password/code checks.
	if r.Method == http.MethodPost && path == "/users/"+userID+"/mfa/generate" {
		return true
	}
	if r.Method == http.MethodPut && path == "/users/"+userID+"/mfa" {
		return true
	}
	return false
}

func (s *Service) enforce(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.app.License() == nil {
			next.ServeHTTP(w, r)
			return
		}
		token, _ := app.ParseAuthTokenFromRequest(r)
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		session, appErr := s.app.GetSession(token)
		if appErr != nil || session == nil || session.Local || rotationExempt(r, session.UserId) {
			next.ServeHTTP(w, r)
			return
		}
		required, _, err := s.requiredState(r.Context(), session.UserId)
		if err == nil && !required {
			next.ServeHTTP(w, r)
			return
		}
		var denied *model.AppError
		if err != nil {
			denied = stateError(err)
		} else {
			denied = model.NewAppError("RequiredPasswordChange", "ent.required_password_change.required", nil, "Complete required password change before using this account", http.StatusForbidden)
			denied.Message = "Complete the required password change before using this account."
			if strings.HasPrefix(strings.ToLower(r.Header.Get("Accept-Language")), "ar") {
				denied.Message = "يجب إكمال تغيير كلمة المرور المطلوب قبل استخدام الحساب."
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(denied.StatusCode)
		_ = json.NewEncoder(w).Encode(denied)
	})
}
