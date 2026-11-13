package ldap

import (
	"encoding/json"
	"net/http"

	"github.com/mattermost/enterprise/internal/advisorylock"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

type Service struct {
	app *app.App

	// Coordinate on a separate connection so native account/group operations
	// can still use the application's SQL pool, including a one-connection pool.
	syncLocker *advisorylock.Locker
}

type groupResponse struct {
	ID           *string `json:"mattermost_group_id,omitempty"`
	DisplayName  string  `json:"name"`
	RemoteID     string  `json:"primary_key"`
	HasSyncables *bool   `json:"has_syncables,omitempty"`
}

var _ einterfaces.LdapInterface = (*Service)(nil)

func New(a *app.App) einterfaces.LdapInterface {
	service := newService(a)
	service.registerRoutes()
	return service
}

func newService(a *app.App) *Service {
	return &Service{
		app: a, syncLocker: advisorylock.New(model.SafeDereference(a.Config().SqlSettings.DataSource)),
	}
}

func (s *Service) registerRoutes() {
	handler := s.getGroups
	s.app.Srv().Router.Handle(model.APIURLSuffix+"/ldap/groups", &web.Handler{
		Srv:            s.app.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodGet)
}

func (s *Service) getGroups(c *web.Context, w http.ResponseWriter, _ *http.Request) {
	if s.app.License() == nil {
		c.Err = model.NewAppError("GetLDAPGroups", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionSysconsoleReadUserManagementGroups) {
		c.SetPermissionError(model.PermissionSysconsoleReadUserManagementGroups)
		return
	}

	opts := model.LdapGroupSearchOpts{Q: c.Params.Q, IsLinked: c.Params.IsLinked, IsConfigured: c.Params.IsConfigured}
	groups, total, appErr := s.GetAllGroupsPage(c.AppContext, c.Params.Page, c.Params.PerPage, opts)
	if appErr != nil {
		c.Err = appErr
		return
	}

	responseGroups := make([]*groupResponse, 0, len(groups))
	for _, group := range groups {
		item := &groupResponse{DisplayName: group.DisplayName, RemoteID: group.GetRemoteId()}
		if model.IsValidId(group.Id) {
			item.ID = &group.Id
			item.HasSyncables = &group.HasSyncables
		}
		responseGroups = append(responseGroups, item)
	}

	if err := json.NewEncoder(w).Encode(struct {
		Count  int              `json:"count"`
		Groups []*groupResponse `json:"groups"`
	}{Count: total, Groups: responseGroups}); err != nil {
		c.Logger.Warn("Error while writing response", mlog.Err(err))
	}
}
