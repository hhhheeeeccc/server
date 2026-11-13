package profile_gif

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

type Service struct {
	app *app.App
}

func Register(a *app.App) {
	s := &Service{app: a}
	endpoint := s.upload
	a.Srv().Router.Handle(model.APIURLSuffix+"/users/{user_id:[A-Za-z0-9]+}/image/animated", &web.Handler{
		Srv: a.Srv(), HandleFunc: endpoint, HandlerName: web.GetHandlerName(endpoint),
		RequireSession: true,
		FileAPI:        true,
	}).Methods(http.MethodPost)
	a.Srv().Router.Use(s.correctImageType)
}

func (s *Service) upload(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if s.app.License() == nil {
		c.Err = model.NewAppError("UploadAnimatedProfileImage", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	c.RequireUserId()
	if c.Err != nil {
		return
	}
	if !c.App.SessionHasPermissionToUserOrBot(c.AppContext, *c.AppContext.Session(), c.Params.UserId) {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}
	if *c.App.Config().FileSettings.DriverName == "" {
		c.Err = model.NewAppError("UploadAnimatedProfileImage", "api.user.upload_profile_user.storage.app_error", nil, "", http.StatusNotImplemented)
		return
	}
	user, appErr := c.App.GetUser(c.AppContext, c.Params.UserId)
	if appErr != nil {
		c.Err = appErr
		return
	}
	if (user.IsLDAPUser() || (user.IsSAMLUser() && *c.App.Config().SamlSettings.EnableSyncWithLdap)) &&
		*c.App.Config().LdapSettings.PictureAttribute != "" {
		c.Err = model.NewAppError("UploadAnimatedProfileImage", "api.user.upload_profile_user.login_provider_attribute_set.app_error", nil, "", http.StatusConflict)
		return
	}
	if c.App.IsProfileImageLockedForUser(*c.AppContext.Session(), user) {
		c.Err = model.NewAppError("UploadAnimatedProfileImage", "api.user.upload_profile_user.profile_field_locked.app_error", nil, "", http.StatusConflict)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxSize+1024*1024)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		c.Err = model.NewAppError("UploadAnimatedProfileImage", "api.user.upload_profile_user.parse.app_error", nil, "", http.StatusBadRequest).Wrap(err)
		return
	}
	file, header, err := r.FormFile("image")
	if err != nil {
		c.Err = model.NewAppError("UploadAnimatedProfileImage", "api.user.upload_profile_user.no_file.app_error", nil, "", http.StatusBadRequest).Wrap(err)
		return
	}
	defer file.Close()
	if header.Size > MaxSize {
		c.Err = model.NewAppError("UploadAnimatedProfileImage", "api.user.upload_profile_user.too_large.app_error", nil, "", http.StatusRequestEntityTooLarge)
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxSize+1))
	if err != nil {
		c.Err = model.NewAppError("UploadAnimatedProfileImage", "api.user.upload_profile_user.decode.app_error", nil, "", http.StatusBadRequest).Wrap(err)
		return
	}
	processed, err := Process(data)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrLimit) {
			status = http.StatusRequestEntityTooLarge
		}
		c.Err = model.NewAppError("UploadAnimatedProfileImage", "api.user.upload_profile_user.decode.app_error", nil, "", status).Wrap(err)
		return
	}

	path := filepath.Join("users", user.Id, "profile.png")
	if _, appErr := c.App.WriteFile(bytes.NewReader(processed), path); appErr != nil {
		c.Err = appErr
		return
	}
	if err := c.App.Srv().Store().User().UpdateLastPictureUpdate(user.Id); err != nil {
		c.Err = model.NewAppError("UploadAnimatedProfileImage", "api.user.upload_profile_user.upload_profile.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		return
	}
	c.App.InvalidateCacheForUser(user.Id)
	updated, appErr := c.App.GetUser(c.AppContext, user.Id)
	if appErr == nil {
		updated.SanitizeProfile(c.App.Config().GetSanitizeOptions(), false)
		event := model.NewWebSocketEvent(model.WebsocketEventUserUpdated, "", "", "", nil, "")
		event.Add("user", updated)
		c.App.Publish(event)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"OK"}`))
}

// The built-in image endpoint labels every profile image as PNG. Correct that
// header when its stored bytes are an animated GIF, including for mobile clients.
func (s *Service) correctImageType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/image") && strings.HasPrefix(r.URL.Path, model.APIURLSuffix+"/users/") {
			next.ServeHTTP(&imageTypeWriter{ResponseWriter: w}, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type imageTypeWriter struct {
	http.ResponseWriter
}

func (w *imageTypeWriter) Write(data []byte) (int, error) {
	if bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")) {
		w.Header().Set("Content-Type", "image/gif")
	}
	return w.ResponseWriter.Write(data)
}
