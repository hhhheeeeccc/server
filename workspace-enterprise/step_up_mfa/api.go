package step_up_mfa

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const policyRoute = model.APIURLSuffix + "/workspace/step_up_mfa/policy"

type policyUpdateRequest struct {
	TeamDeletion       *bool  `json:"team_deletion"`
	UserDeletion       *bool  `json:"user_deletion"`
	UserPasswordChange *bool  `json:"user_password_change"`
	UserMfaRemoval     *bool  `json:"user_mfa_removal"`
	ConfigDisable      *bool  `json:"config_disable"`
	GraceMinutes       *int   `json:"grace_minutes"`
	Version            *int64 `json:"version"`
	Enabled            *bool  `json:"enabled"` // accepted from older webapp bundles, ignored
}

func (s *Service) registerRoutes() {
	for _, route := range []struct {
		method  string
		handler func(*web.Context, http.ResponseWriter, *http.Request)
	}{
		{http.MethodGet, s.getPolicy},
		{http.MethodPut, s.updatePolicy},
	} {
		s.app.Srv().Router.Handle(policyRoute, &web.Handler{
			Srv:            s.app.Srv(),
			HandleFunc:     route.handler,
			HandlerName:    web.GetHandlerName(route.handler),
			RequireSession: true,
			RequireMfa:     true,
		}).Methods(route.method)
	}
}

func (s *Service) getPolicy(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !s.authorizePolicy(c, false) {
		return
	}
	current, err := s.readPolicy(r.Context())
	if err != nil {
		c.Err = policyStorageError(err)
		return
	}
	if err = json.NewEncoder(w).Encode(current); err != nil {
		c.Logger.Warn("Unable to write step-up MFA policy", mlog.Err(err))
	}
}

func (s *Service) updatePolicy(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !s.authorizePolicy(c, true) {
		return
	}
	requested, err := parsePolicyUpdate(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("UpdateStepUpMFAPolicy", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("policy", err)
		return
	}

	auditRec := c.MakeAuditRecord("updateStepUpMFAPolicy", model.AuditStatusFail)
	defer c.LogAuditRec(auditRec)
	auditRec.AddMeta("policy", requested.PolicySettings)

	updated, err := s.savePolicy(r.Context(), requested)
	switch {
	case errors.Is(err, errPolicyConflict):
		c.Err = model.NewAppError("UpdateStepUpMFAPolicy", "ent.step_up_mfa.policy_conflict", nil, "", http.StatusConflict)
		return
	case err != nil:
		c.Err = policyStorageError(err)
		return
	}
	auditRec.Success()
	if err = json.NewEncoder(w).Encode(updated); err != nil {
		c.Logger.Warn("Unable to write updated step-up MFA policy", mlog.Err(err))
	}
}

func parsePolicyUpdate(body io.Reader) (PolicyResponse, error) {
	var input policyUpdateRequest
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return PolicyResponse{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err != nil {
			return PolicyResponse{}, err
		}
		return PolicyResponse{}, errors.New("expected one JSON object")
	}
	if input.TeamDeletion == nil || input.UserDeletion == nil || input.UserPasswordChange == nil ||
		input.UserMfaRemoval == nil || input.ConfigDisable == nil || input.GraceMinutes == nil ||
		input.Version == nil || *input.GraceMinutes < 0 || *input.GraceMinutes > 1440 || *input.Version < 0 {
		return PolicyResponse{}, errors.New("incomplete or invalid policy")
	}
	requested := PolicyResponse{PolicySettings: PolicySettings{
		TeamDeletion: *input.TeamDeletion, UserDeletion: *input.UserDeletion,
		UserPasswordChange: *input.UserPasswordChange, UserMfaRemoval: *input.UserMfaRemoval,
		ConfigDisable: *input.ConfigDisable, GraceMinutes: *input.GraceMinutes,
	}, Version: *input.Version}
	return requested, nil
}

func (s *Service) authorizePolicy(c *web.Context, write bool) bool {
	if s.app.License() == nil {
		c.Err = model.NewAppError("StepUpMFAPolicy", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return false
	}
	permission := model.PermissionSysconsoleReadAuthenticationMfa
	if write {
		permission = model.PermissionSysconsoleWriteAuthenticationMfa
	}
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), permission) {
		c.SetPermissionError(permission)
		return false
	}
	return true
}

func policyStorageError(err error) *model.AppError {
	return model.NewAppError("StepUpMFAPolicy", "ent.step_up_mfa.policy_unavailable", nil, "", http.StatusServiceUnavailable).Wrap(err)
}
