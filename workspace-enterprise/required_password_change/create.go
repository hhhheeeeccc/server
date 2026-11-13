package required_password_change

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/app/password/hashers"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

// The native creation pipeline retains validation, license/user limits, roles,
// preferences and hooks. Its unshared random bootstrap credential prevents a
// login until the private rotation state is durable and the supplied temporary
// password has been installed. Never relax the global password configuration.
func (s *Service) createByAdmin(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if c.App.License() == nil {
		c.Err = model.NewAppError("CreateTemporaryUser", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionSysconsoleWriteUserManagementUsers) {
		c.SetPermissionError(model.PermissionSysconsoleWriteUserManagementUsers)
		return
	}
	var input struct {
		Username    string          `json:"username"`
		Email       string          `json:"email"`
		Password    string          `json:"password"`
		FirstName   string          `json:"first_name"`
		LastName    string          `json:"last_name"`
		Locale      string          `json:"locale"`
		NotifyProps model.StringMap `json:"notify_props"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		c.SetInvalidParamWithErr("user", err)
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		c.SetInvalidParam("user")
		return
	}
	if input.Password == "" {
		c.SetInvalidParam("password")
		return
	}
	if len(input.Password) > model.PasswordMaximumLength {
		c.Err = model.NewAppError("CreateTemporaryUser", "model.user.is_valid.pwd_max_length.app_error", nil, "", http.StatusBadRequest)
		return
	}
	if s.initErr != nil {
		c.Err = stateError(s.initErr)
		return
	}
	hashed, err := hashers.Hash(input.Password)
	if err != nil {
		c.Err = model.NewAppError("CreateTemporaryUser", "api.user.update_password.password_hash.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		return
	}
	user := &model.User{Username: input.Username, Email: input.Email, Password: "Aa1!" + model.NewRandomString(model.PasswordMaximumLength-4), FirstName: input.FirstName, LastName: input.LastName, Locale: input.Locale, NotifyProps: input.NotifyProps, DisableWelcomeEmail: true}
	audit := c.MakeAuditRecord("createTemporaryUser", model.AuditStatusFail)
	defer c.LogAuditRec(audit)
	created, appErr := s.emailUsers.CreateLocalUser(c.AppContext, user)
	if appErr != nil {
		c.Err = appErr
		return
	}
	audit.AddMeta("user_id", created.Id)
	if appErr = s.setRequired(c, created, true); appErr != nil {
		c.Err = appErr
		return
	}
	if appErr = c.App.UpdateHashedPasswordByUserId(c.AppContext, created.Id, hashed); appErr != nil {
		c.Err = appErr
		return
	}
	// Reload the presentation hint; never return credentials or hash material.
	created, appErr = c.App.GetUser(c.AppContext, created.Id)
	if appErr != nil {
		c.Err = appErr
		return
	}
	created.Sanitize(nil)
	audit.Success()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(created)
}
