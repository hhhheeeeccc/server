package status_lock

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

// Register shadows the core status update route only while the licensed lock
// is enabled. The core route handles the request again when the lock is off.
func Register(a *app.App) {
	handler := updateUserStatus
	a.Srv().Router.Handle(model.APIURLSuffix+"/users/{user_id:[A-Za-z0-9]+}/status", &web.Handler{
		Srv: a.Srv(), HandleFunc: handler, HandlerName: web.GetHandlerName(handler),
		RequireSession: true, RequireMfa: true,
	}).MatcherFunc(func(_ *http.Request, _ *mux.RouteMatch) bool {
		return presenceLocked(a)
	}).Methods(http.MethodPut)
}

func userManagerCanEditStatus(a *app.App, rctx request.CTX, targetID string) bool {
	session := rctx.Session()
	if session == nil || !slices.Contains(session.GetUserRoles(), model.SystemUserManagerRoleId) {
		return false
	}
	memberships, err := a.Srv().Store().Team().GetTeamsForUser(rctx, session.UserId, "", false)
	if err != nil || len(memberships) == 0 {
		return false
	}
	targetTeams, err := a.Srv().Store().Team().GetUserTeamIds(targetID, true)
	if err != nil {
		return false
	}
	for _, membership := range memberships {
		if membership.SchemeAdmin && slices.Contains(targetTeams, membership.TeamId) {
			return true
		}
	}
	return false
}

func updateUserStatus(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !model.IsValidId(c.Params.UserId) {
		c.SetInvalidParam("user_id")
		return
	}
	session := c.AppContext.Session()
	managerCanEdit := userManagerCanEditStatus(c.App, c.AppContext, c.Params.UserId)
	if !c.IsSystemAdmin() && !managerCanEdit {
		c.Err = model.NewAppError("updateUserStatus", "api.user.update_user.personal_data_locked.app_error", nil, "", http.StatusForbidden)
		return
	}
	if !c.App.SessionHasPermissionToUser(c.AppContext, *session, c.Params.UserId) && !managerCanEdit {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	var status model.Status
	err := decoder.Decode(&status)
	if err == nil {
		var trailing any
		if trailingErr := decoder.Decode(&trailing); trailingErr != io.EOF {
			err = trailingErr
			if err == nil {
				err = errors.New("unexpected data after status")
			}
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("updateUserStatus", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("status", err)
		return
	}
	if status.UserId != c.Params.UserId {
		c.SetInvalidParam("user_id")
		return
	}
	switch status.Status {
	case model.StatusOnline, model.StatusOffline, model.StatusAway, model.StatusDnd:
	default:
		c.SetInvalidParam("status")
		return
	}

	current, statusErr := c.App.GetStatus(c.Params.UserId)
	if statusErr != nil {
		c.Logger.Warn("Failed to get current status", mlog.Err(statusErr))
	} else if current.Status == model.StatusOutOfOffice && status.Status != model.StatusOutOfOffice {
		if disableErr := c.App.DisableAutoResponder(c.AppContext, c.Params.UserId, c.IsSystemAdmin()); disableErr != nil {
			c.Logger.Warn("Failed to disable auto-responder", mlog.Err(disableErr))
		}
	}
	switch status.Status {
	case model.StatusOnline:
		c.App.SetStatusOnline(c.Params.UserId, true)
	case model.StatusOffline:
		c.App.SetStatusOffline(c.Params.UserId, true, false)
	case model.StatusAway:
		c.App.SetStatusAwayIfNeeded(c.Params.UserId, true)
	case model.StatusDnd:
		c.App.SetStatusDoNotDisturbTimed(c.Params.UserId, status.DNDEndTime)
	}
	statuses, appErr := c.App.GetUserStatusesByIds([]string{c.Params.UserId})
	if appErr != nil {
		c.Err = appErr
		return
	}
	if len(statuses) == 0 {
		c.Err = model.NewAppError("updateUserStatus", "api.status.user_not_found.app_error", nil, "", http.StatusNotFound)
		return
	}
	encoded, jsonErr := statuses[0].ToJSON()
	if jsonErr != nil {
		c.Err = model.NewAppError("updateUserStatus", "api.marshal_error", nil, "", http.StatusInternalServerError).Wrap(jsonErr)
		return
	}
	if _, err := w.Write(encoded); err != nil {
		c.Logger.Warn("Error while writing response", mlog.Err(err))
	}
}
