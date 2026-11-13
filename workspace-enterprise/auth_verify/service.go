package auth_verify

import (
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const endpoint = model.APIURLSuffix + "/auth/verify"

type Service struct {
	app *app.App
}

// Register exposes a small session-verification endpoint for trusted reverse
// proxies. Authentication and MFA enforcement are delegated to Mattermost's
// standard web handler so the endpoint follows the same rules as protected API
// routes.
func Register(a *app.App) {
	service := &Service{app: a}
	handler := &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     service.verify,
		HandlerName:    web.GetHandlerName(service.verify),
		RequireSession: true,
		RequireMfa:     true,
	}

	a.Srv().Router.Handle(endpoint, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Authentication failures must not be cached either.
		w.Header().Set("Cache-Control", "no-store")
		handler.ServeHTTP(w, r)
	})).Methods(http.MethodGet)
}

func (s *Service) verify(c *web.Context, w http.ResponseWriter, _ *http.Request) {
	if s.app.License() == nil {
		c.Err = model.NewAppError("VerifyAuth", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
