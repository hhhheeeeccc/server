package user_lookup

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

type Service struct {
	app *app.App
}

const maxUserIDsBodyBytes = 1 << 20

func Register(a *app.App) {
	service := &Service{app: a}
	handler := service.getUsersByIDs
	a.Srv().Router.Handle(model.APIURLSuffix+"/users/ids", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).MatcherFunc(func(_ *http.Request, _ *mux.RouteMatch) bool {
		return a.License() != nil
	}).Methods(http.MethodPost)
}

func (s *Service) getUsersByIDs(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if s.app.License() == nil {
		c.Err = model.NewAppError("GetUsersByIDs", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}

	var userIDs []string
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUserIDsBodyBytes))
	if err := decoder.Decode(&userIDs); err != nil || userIDs == nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("GetUsersByIDs", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.Err = model.NewAppError("GetUsersByIDs", model.PayloadParseError, nil, "", http.StatusBadRequest).Wrap(err)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("GetUsersByIDs", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		if err == nil {
			err = errors.New("unexpected trailing JSON value")
		}
		c.Err = model.NewAppError("GetUsersByIDs", model.PayloadParseError, nil, "", http.StatusBadRequest).Wrap(err)
		return
	}
	userIDs = model.RemoveDuplicateStrings(userIDs)

	options := &store.UserGetByIdsOpts{IsAdmin: c.IsSystemAdmin()}
	if sinceText := r.URL.Query().Get("since"); sinceText != "" {
		since, parseErr := strconv.ParseInt(sinceText, 10, 64)
		if parseErr != nil {
			c.SetInvalidParamWithErr("since", parseErr)
			return
		}
		options.Since = since
	}
	if len(userIDs) == 0 {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte("[]")); err != nil {
			c.Logger.Warn("Error while writing response", mlog.Err(err))
		}
		return
	}

	restrictions, appErr := c.App.GetViewUsersRestrictions(c.AppContext, c.AppContext.Session().UserId)
	if appErr != nil {
		c.Err = appErr
		return
	}
	options.ViewRestrictions = restrictions

	users, appErr := c.App.GetUsersByIds(c.AppContext, userIDs, options)
	if appErr != nil {
		c.Err = appErr
		return
	}
	if err := json.NewEncoder(w).Encode(users); err != nil {
		c.Logger.Warn("Error while writing response", mlog.Err(err))
	}
}
