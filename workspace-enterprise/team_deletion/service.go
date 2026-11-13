package team_deletion

import (
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

type Service struct {
	app *app.App
}

func Register(a *app.App) {
	service := &Service{app: a}
	handler := service.deleteTeam

	a.Srv().Router.Handle(model.APIURLSuffix+"/teams/{team_id:[A-Za-z0-9]+}", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodDelete)
}

func (s *Service) deleteTeam(c *web.Context, w http.ResponseWriter, _ *http.Request) {
	c.RequireTeamId()
	if c.Err != nil {
		return
	}

	if !c.App.SessionHasPermissionToTeam(*c.AppContext.Session(), c.Params.TeamId, model.PermissionManageTeam) {
		c.SetPermissionError(model.PermissionManageTeam)
		return
	}

	auditRec := c.MakeAuditRecord(model.AuditEventDeleteTeam, model.AuditStatusFail)
	defer c.LogAuditRec(auditRec)
	if team, appErr := c.App.GetTeam(c.Params.TeamId); appErr == nil {
		model.AddEventParameterAuditableToAuditRec(auditRec, "team", team)
	}

	var appErr *model.AppError
	if c.Params.Permanent {
		if s.app.License() == nil {
			c.Err = model.NewAppError("DeleteTeamPermanently", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
			return
		}
		if model.SafeDereference(c.App.Config().ServiceSettings.EnableAPITeamDeletion) {
			appErr = c.App.PermanentDeleteTeamId(c.AppContext, c.Params.TeamId)
		} else {
			user, userErr := c.App.GetUser(c.AppContext, c.AppContext.Session().UserId)
			if userErr == nil && user != nil && user.IsSystemAdmin() {
				appErr = model.NewAppError("DeleteTeam", "api.user.delete_team.not_enabled.for_admin.app_error", nil, "teamId="+c.Params.TeamId, http.StatusUnauthorized)
			} else {
				appErr = model.NewAppError("DeleteTeam", "api.user.delete_team.not_enabled.app_error", nil, "teamId="+c.Params.TeamId, http.StatusUnauthorized)
			}
		}
	} else {
		appErr = c.App.SoftDeleteTeam(c.Params.TeamId)
	}
	if appErr != nil {
		c.Err = appErr
		return
	}

	auditRec.Success()
	web.ReturnStatusOK(w)
}
