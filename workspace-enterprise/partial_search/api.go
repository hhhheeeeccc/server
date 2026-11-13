package partial_search

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

// registerSearchRoute installs the isolated Workspace handler before api4
// registers the core route. This is needed for the scoped searches which do
// not pass through SearchEngineInterface (in/out of a team, channel or group).
func (s *Service) registerSearchRoute() {
	handler := s.searchUsers
	s.app.Srv().Router.Handle(model.APIURLSuffix+"/users/search", &web.Handler{
		Srv:             s.app.Srv(),
		HandleFunc:      handler,
		HandlerName:     web.GetHandlerName(handler),
		RequireSession:  true,
		RequireMfa:      true,
		DisableWhenBusy: true,
	}).Methods(http.MethodPost)

	// Keep mmctl --local behavior aligned with the HTTP route. Local mode
	// creates its administrative session in web.Handler.
	s.app.Srv().LocalRouter.Handle(model.APIURLSuffix+"/users/search", &web.Handler{
		Srv:             s.app.Srv(),
		HandleFunc:      handler,
		HandlerName:     web.GetHandlerName(handler),
		RequireSession:  false,
		RequireMfa:      false,
		IsLocal:         true,
		DisableWhenBusy: true,
	}).Methods(http.MethodPost)
}

func (s *Service) searchUsers(c *web.Context, w http.ResponseWriter, r *http.Request) {
	var props model.UserSearch
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	err := decoder.Decode(&props)
	if err == nil {
		var trailing any
		if trailingErr := decoder.Decode(&trailing); trailingErr != io.EOF {
			err = trailingErr
			if err == nil {
				err = errors.New("unexpected data after user search")
			}
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("SearchUsers", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("props", err)
		return
	}

	if props.Limit == 0 {
		props.Limit = model.UserSearchDefaultLimit
	}
	if props.Term == "" {
		c.SetInvalidParam("term")
		return
	}
	if props.TeamId == "" && props.NotInChannelId != "" {
		c.SetInvalidParam("team_id")
		return
	}
	if props.InGroupId != "" {
		if appErr := workspaceCanReadGroupMembers(c, props.InGroupId); appErr != nil {
			appErr.Where = "WorkspacePartialSearch.searchUsers"
			c.Err = appErr
			return
		}
	}
	if props.NotInGroupId != "" {
		if appErr := workspaceCanReadGroupMembers(c, props.NotInGroupId); appErr != nil {
			appErr.Where = "WorkspacePartialSearch.searchUsers"
			c.Err = appErr
			return
		}
	}
	if props.InChannelId != "" {
		if ok, _ := c.App.SessionHasPermissionToChannel(c.AppContext, *c.AppContext.Session(), props.InChannelId, model.PermissionReadChannel); !ok {
			c.SetPermissionError(model.PermissionReadChannel)
			return
		}
	}
	if props.NotInChannelId != "" {
		if ok, _ := c.App.SessionHasPermissionToChannel(c.AppContext, *c.AppContext.Session(), props.NotInChannelId, model.PermissionReadChannel); !ok {
			c.SetPermissionError(model.PermissionReadChannel)
			return
		}
	}
	if props.TeamId != "" && !c.App.SessionHasPermissionToTeam(*c.AppContext.Session(), props.TeamId, model.PermissionViewTeam) {
		c.SetPermissionError(model.PermissionViewTeam)
		return
	}
	if props.NotInTeamId != "" && !c.App.SessionHasPermissionToTeam(*c.AppContext.Session(), props.NotInTeamId, model.PermissionViewTeam) {
		c.SetPermissionError(model.PermissionViewTeam)
		return
	}
	if props.Limit <= 0 || props.Limit > model.UserSearchMaxLimit {
		c.SetInvalidParam("limit")
		return
	}

	options := &model.UserSearchOptions{
		IsAdmin:          c.IsSystemAdmin(),
		AllowInactive:    props.AllowInactive,
		GroupConstrained: props.GroupConstrained,
		Limit:            props.Limit,
		Role:             props.Role,
		Roles:            props.Roles,
		ChannelRoles:     props.ChannelRoles,
		TeamRoles:        props.TeamRoles,
	}
	if c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionManageSystem) {
		options.AllowEmails = true
		options.AllowFullNames = true
	} else {
		options.AllowEmails = model.SafeDereference(c.App.Config().PrivacySettings.ShowEmailAddress)
		options.AllowFullNames = model.SafeDereference(c.App.Config().PrivacySettings.ShowFullName)
	}

	var appErr *model.AppError
	options, appErr = c.App.RestrictUsersSearchByPermissions(c.AppContext, c.AppContext.Session().UserId, options)
	if appErr != nil {
		c.Err = appErr
		return
	}

	useWorkspaceScopedSearch := s.IsActive() && hasScopedUserSearch(&props)
	if useWorkspaceScopedSearch && props.NotInChannelId != "" {
		// ABAC membership candidates must continue through Mattermost's access
		// control service. It owns policy evaluation for this particular scope.
		controlled, accessErr := c.App.ChannelAccessControlled(c.AppContext, props.NotInChannelId)
		if accessErr != nil {
			c.Err = accessErr
			return
		}
		useWorkspaceScopedSearch = !controlled
	}

	var profiles []*model.User
	if useWorkspaceScopedSearch {
		profiles, appErr = s.searchScopedUsers(c, &props, options)
	} else {
		// The ordinary team/global search reaches SearchEngineInterface. This
		// fallback also retains Community behavior while no license is loaded.
		profiles, appErr = c.App.SearchUsers(c.AppContext, &props, options)
	}
	if appErr != nil {
		c.Err = appErr
		return
	}

	response, err := json.Marshal(profiles)
	if err != nil {
		c.Err = model.NewAppError("WorkspacePartialSearch.searchUsers", "api.marshal_error", nil, "", http.StatusInternalServerError).Wrap(err)
		return
	}
	if _, err := w.Write(response); err != nil {
		c.Logger.Warn("Error while writing Workspace user-search response", mlog.Err(err))
	}
}

func hasScopedUserSearch(props *model.UserSearch) bool {
	return props.WithoutTeam || props.InChannelId != "" || props.NotInChannelId != "" ||
		props.NotInTeamId != "" || props.InGroupId != "" || props.NotInGroupId != ""
}

func (s *Service) searchScopedUsers(c *web.Context, props *model.UserSearch, options *model.UserSearchOptions) ([]*model.User, *model.AppError) {
	query, args := buildScopedUserQuery(props, options)
	ids, err := s.queryIDsContext(c.AppContext.Context(), query, args...)
	if err != nil {
		return nil, searchError("PartialSearch.searchScopedUsers", err)
	}

	users, err := c.App.Srv().Store().User().GetProfileByIds(c.AppContext, ids, nil, false)
	if err != nil {
		return nil, model.NewAppError("PartialSearch.searchScopedUsers", "app.user.search.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	for _, user := range users {
		c.App.SanitizeProfile(user, options.IsAdmin)
	}
	return users, nil
}

func buildScopedUserQuery(props *model.UserSearch, options *model.UserSearchOptions) (string, []any) {
	builder := &sqlQueryBuilder{}
	conditions := make([]string, 0, 14)
	if !options.AllowInactive {
		conditions = append(conditions, "u.deleteat = 0")
	}

	teamRoleID := ""
	channelRoleID := ""
	switch {
	case props.WithoutTeam:
		conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM teammembers stm WHERE stm.userid = u.id AND stm.deleteat = 0)")
	case props.InChannelId != "":
		channelRoleID = props.InChannelId
		teamRoleID = props.TeamId
		conditions = append(conditions, "EXISTS (SELECT 1 FROM channelmembers scm WHERE scm.userid = u.id AND scm.channelid = "+builder.bind(props.InChannelId)+")")
	case props.NotInChannelId != "":
		channelRoleID = props.NotInChannelId
		teamRoleID = props.TeamId
		conditions = append(conditions,
			"NOT EXISTS (SELECT 1 FROM channelmembers scm WHERE scm.userid = u.id AND scm.channelid = "+builder.bind(props.NotInChannelId)+")",
			"EXISTS (SELECT 1 FROM teammembers stm WHERE stm.userid = u.id AND stm.teamid = "+builder.bind(props.TeamId)+" AND stm.deleteat = 0)",
		)
		if options.GroupConstrained {
			conditions = append(conditions, groupConstrainedChannelCondition(builder, props.NotInChannelId))
		}
	case props.NotInTeamId != "":
		teamRoleID = props.NotInTeamId
		conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM teammembers stm WHERE stm.userid = u.id AND stm.teamid = "+builder.bind(props.NotInTeamId)+" AND stm.deleteat = 0)")
		if options.GroupConstrained {
			conditions = append(conditions, groupConstrainedTeamCondition(builder, props.NotInTeamId))
		}
	case props.InGroupId != "":
		conditions = append(conditions, "EXISTS (SELECT 1 FROM groupmembers sgm WHERE sgm.userid = u.id AND sgm.groupid = "+builder.bind(props.InGroupId)+" AND sgm.deleteat = 0)")
	case props.NotInGroupId != "":
		conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM groupmembers sgm WHERE sgm.userid = u.id AND sgm.groupid = "+builder.bind(props.NotInGroupId)+" AND sgm.deleteat = 0)")
	}

	conditions = append(conditions, buildViewRestrictionConditions(builder, options.ViewRestrictions)...)
	conditions = append(conditions, buildUserTermConditions(builder, props.Term, options))
	conditions = append(conditions, buildUserRoleConditions(builder, teamRoleID, channelRoleID, nil, options))
	conditions = nonEmpty(conditions)
	if len(conditions) == 0 {
		conditions = append(conditions, "TRUE")
	}

	return "SELECT u.id FROM users u WHERE " + strings.Join(conditions, " AND ") +
		" ORDER BY LOWER(u.username) ASC, u.id ASC LIMIT " + builder.bind(options.Limit), builder.args
}

func buildViewRestrictionConditions(builder *sqlQueryBuilder, restrictions *model.ViewUsersRestrictions) []string {
	if restrictions == nil {
		return nil
	}
	if len(restrictions.Teams) == 0 && len(restrictions.Channels) == 0 {
		return []string{"FALSE"}
	}

	conditions := make([]string, 0, 2)
	if len(restrictions.Teams) > 0 {
		conditions = append(conditions, "EXISTS (SELECT 1 FROM teammembers rtm WHERE rtm.userid = u.id AND rtm.deleteat = 0 AND "+builder.in("rtm.teamid", restrictions.Teams, false)+")")
	}
	if len(restrictions.Channels) > 0 {
		conditions = append(conditions, "EXISTS (SELECT 1 FROM channelmembers rcm WHERE rcm.userid = u.id AND "+builder.in("rcm.channelid", restrictions.Channels, false)+")")
	}
	return []string{"(" + strings.Join(conditions, " OR ") + ")"}
}

func groupConstrainedChannelCondition(builder *sqlQueryBuilder, channelID string) string {
	return `u.id IN (
		SELECT gm.userid
		FROM groupchannels gc
		JOIN usergroups ug ON ug.id = gc.groupid
		JOIN groupmembers gm ON gm.groupid = ug.id
		WHERE gc.channelid = ` + builder.bind(channelID) + ` AND gc.deleteat = 0 AND ug.deleteat = 0 AND gm.deleteat = 0
	)`
}

func groupConstrainedTeamCondition(builder *sqlQueryBuilder, teamID string) string {
	return `u.id IN (
		SELECT gm.userid
		FROM groupteams gt
		JOIN usergroups ug ON ug.id = gt.groupid
		JOIN groupmembers gm ON gm.groupid = ug.id
		WHERE gt.teamid = ` + builder.bind(teamID) + ` AND gt.deleteat = 0 AND ug.deleteat = 0 AND gm.deleteat = 0
	)`
}

func workspaceCanReadGroupMembers(c *web.Context, groupID string) *model.AppError {
	group, appErr := c.App.GetGroup(groupID, nil, nil)
	if appErr != nil {
		return appErr
	}
	if appErr = workspaceGroupSourceAvailable(c, group.Source); appErr != nil {
		return appErr
	}
	if group.IsSyncable() && !group.AllowReference && !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionSysconsoleReadUserManagementGroups) {
		return model.MakePermissionError(c.AppContext.Session(), []*model.Permission{model.PermissionSysconsoleReadUserManagementGroups})
	}
	return nil
}

func workspaceGroupSourceAvailable(c *web.Context, source model.GroupSource) *model.AppError {
	license := c.App.Srv().License()
	if license == nil {
		return model.NewAppError("", "api.license_error", nil, "", http.StatusForbidden)
	}
	if (source == model.GroupSourceLdap || strings.HasPrefix(string(source), string(model.GroupSourcePluginPrefix))) && !model.SafeDereference(license.Features.LDAPGroups) {
		return model.NewAppError("", "api.ldap_groups.license_error", nil, "", http.StatusForbidden)
	}
	if source == model.GroupSourceCustom && !model.MinimumProfessionalLicense(license) {
		return model.NewAppError("", "api.custom_groups.license_error", nil, "", http.StatusBadRequest)
	}
	if source == model.GroupSourceCustom && !model.SafeDereference(c.App.Config().ServiceSettings.EnableCustomGroups) {
		return model.NewAppError("", "api.custom_groups.feature_disabled", nil, "", http.StatusBadRequest)
	}
	return nil
}
