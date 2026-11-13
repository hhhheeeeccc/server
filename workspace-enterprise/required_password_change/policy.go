package required_password_change

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const policyRoute = model.APIURLSuffix + "/workspace/required_password_change/policy"

type passwordPolicy struct {
	RequireOnAdminReset bool  `json:"require_on_admin_reset"`
	Version             int64 `json:"version"`
}

func (s *Service) readPolicy(ctx context.Context) (passwordPolicy, error) {
	if s.initErr != nil {
		return passwordPolicy{}, s.initErr
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var p passwordPolicy
	err := s.db.QueryRowContext(ctx, `SELECT RequireOnAdminReset, Version FROM WorkspacePasswordPolicy WHERE Id=1`).Scan(&p.RequireOnAdminReset, &p.Version)
	return p, err
}

func (s *Service) getPolicy(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !s.authorizePolicy(c, false) {
		return
	}
	p, err := s.readPolicy(r.Context())
	if err != nil {
		c.Err = stateError(err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p)
}

func (s *Service) updatePolicy(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !s.authorizePolicy(c, true) {
		return
	}
	var input struct {
		RequireOnAdminReset *bool  `json:"require_on_admin_reset"`
		Version             *int64 `json:"version"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		c.SetInvalidParamWithErr("policy", err)
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF || input.RequireOnAdminReset == nil || input.Version == nil || *input.Version < 0 {
		c.SetInvalidParam("policy")
		return
	}
	if s.initErr != nil {
		c.Err = stateError(s.initErr)
		return
	}
	audit := c.MakeAuditRecord("updateRequiredPasswordPolicy", model.AuditStatusFail)
	defer c.LogAuditRec(audit)
	audit.AddMeta("require_on_admin_reset", *input.RequireOnAdminReset)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	p := passwordPolicy{RequireOnAdminReset: *input.RequireOnAdminReset}
	err := s.db.QueryRowContext(ctx, `UPDATE WorkspacePasswordPolicy SET RequireOnAdminReset=$1,Version=Version+1 WHERE Id=1 AND Version=$2 RETURNING Version`, p.RequireOnAdminReset, *input.Version).Scan(&p.Version)
	if errors.Is(err, sql.ErrNoRows) {
		c.Err = model.NewAppError("PasswordPolicy", "ent.required_password_change.policy_conflict", nil, "", http.StatusConflict)
		return
	}
	if err != nil {
		c.Err = stateError(err)
		return
	}
	audit.Success()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p)
}

func (s *Service) authorizePolicy(c *web.Context, write bool) bool {
	if c.App.License() == nil {
		c.Err = model.NewAppError("PasswordPolicy", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return false
	}
	permission := model.PermissionSysconsoleReadSiteUsersAndTeams
	if write {
		permission = model.PermissionSysconsoleWriteSiteUsersAndTeams
	}
	if !write && c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionSysconsoleWriteUserManagementUsers) {
		return true
	}
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), permission) {
		c.SetPermissionError(permission)
		return false
	}
	return true
}
