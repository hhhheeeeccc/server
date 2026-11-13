package nova_sms

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const settingsRoute = model.APIURLSuffix + "/workspace/nova/settings"

func (s *Service) registerRoutes() {
	for _, route := range []struct {
		method  string
		handler func(*web.Context, http.ResponseWriter, *http.Request)
	}{
		{http.MethodGet, s.getSettings},
		{http.MethodPut, s.updateSettings},
	} {
		s.app.Srv().Router.Handle(settingsRoute, &web.Handler{
			Srv: s.app.Srv(), HandleFunc: route.handler, HandlerName: web.GetHandlerName(route.handler),
			RequireSession: true, RequireMfa: true,
		}).Methods(route.method)
	}
}

func (s *Service) authorize(c *web.Context) bool {
	if s.app.License() == nil || !c.IsSystemAdmin() {
		c.Err = model.NewAppError("NovaSettings", "api.context.permissions.app_error", nil, "", http.StatusForbidden)
		return false
	}
	if s.initErr != nil {
		c.Err = storageError(s.initErr)
		return false
	}
	return true
}

func (s *Service) getSettings(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !s.authorize(c) {
		return
	}
	settings, err := s.readSettings(r.Context())
	if err != nil {
		c.Err = storageError(err)
		return
	}
	if err := json.NewEncoder(w).Encode(settings); err != nil {
		c.Logger.Warn("Unable to write Nova settings response", mlog.Err(err))
	}
}

func (s *Service) updateSettings(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !s.authorize(c) {
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	var input settingsUpdate
	if err := decoder.Decode(&input); err != nil {
		c.SetInvalidParam("settings")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF || !validUpdate(input) {
		c.SetInvalidParam("settings")
		return
	}
	current, err := s.readSettings(r.Context())
	if err != nil {
		c.Err = storageError(err)
		return
	}
	if !current.HasSecretKey && input.SecretKey == "" {
		c.SetInvalidParam("secret_key")
		return
	}
	updated, err := s.saveSettings(r.Context(), input)
	if errors.Is(err, errConflict) {
		c.Err = model.NewAppError("NovaSettings", "workspace.nova.settings_conflict", nil, "", http.StatusConflict)
		return
	}
	if err != nil {
		c.Err = storageError(err)
		return
	}
	auditRec := c.MakeAuditRecord("updateNovaSettings", model.AuditStatusSuccess)
	c.LogAuditRec(auditRec) // Never include the request body or secret in audit metadata.
	if err := json.NewEncoder(w).Encode(updated); err != nil {
		c.Logger.Warn("Unable to write updated Nova settings response", mlog.Err(err))
	}
}

func storageError(_ error) *model.AppError {
	return model.NewAppError("NovaSettings", "workspace.nova.settings_unavailable", nil, "", http.StatusServiceUnavailable)
}
