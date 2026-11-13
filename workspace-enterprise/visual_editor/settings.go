package visual_editor

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

func (s *Service) authorize(c *web.Context, write bool) bool {
	if s.app.License() == nil {
		c.Err = model.NewAppError("VisualEditorSettings", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return false
	}
	permission := model.PermissionSysconsoleReadExperimentalFeatureFlags
	if write {
		permission = model.PermissionSysconsoleWriteExperimentalFeatureFlags
	}
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), permission) {
		c.SetPermissionError(permission)
		return false
	}
	return true
}

func (s *Service) getSettings(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !s.authorize(c, false) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.current())
}

func (s *Service) putSettings(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !s.authorize(c, true) {
		return
	}
	audit := c.MakeAuditRecord("updateVisualEditorSettings", model.AuditStatusFail)
	defer c.LogAuditRec(audit)
	var requested struct {
		Enabled *bool `json:"Enabled"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&requested) != nil || requested.Enabled == nil {
		c.SetInvalidParam("Enabled")
		return
	}
	if decoder.Decode(new(any)) != io.EOF {
		c.SetInvalidParam("settings")
		return
	}
	settings := Settings{Enabled: *requested.Enabled}
	if err := s.update(settings); err != nil {
		c.Logger.Error("Unable to persist visual editor settings", mlog.Err(err))
		c.Err = model.NewAppError("VisualEditorSettings", "api.context.internal_server_error.app_error", nil, "", http.StatusInternalServerError)
		return
	}
	audit.Success()
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(settings)
}

// Persist before publishing. Serialize writes and notifications so concurrent
// updates cannot leave clients on an older value than the settings file.
func (s *Service) update(settings Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := persistSettings(s.filename, settings); err != nil {
		return err
	}
	s.settings = settings
	if s.notify != nil {
		s.notify(settings)
	}
	return nil
}

func persistSettings(filename string, settings Settings) error {
	file, err := os.CreateTemp(filepath.Dir(filename), ".visual-editor-*.json")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "    ")
	if err := encoder.Encode(settings); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, filename)
}
