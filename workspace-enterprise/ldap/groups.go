package ldap

import (
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	ldapclient "github.com/mattermost/ldap"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

func (s *Service) groupSettings() (model.LdapSettings, *model.AppError) {
	license := s.app.License()
	if license == nil || license.Features == nil || !model.SafeDereference(license.Features.LDAPGroups) {
		return model.LdapSettings{}, model.NewAppError("LDAP.Groups", "api.ldap_groups.license_error", nil, "", http.StatusForbidden)
	}
	settings := s.app.Config().LdapSettings
	settings.SetDefaults()
	return settings, nil
}

func (s *Service) directoryGroups(rctx request.CTX, settings model.LdapSettings, id string) ([]*model.Group, *model.AppError) {
	if !attributeName.MatchString(*settings.GroupIdAttribute) || !attributeName.MatchString(*settings.GroupDisplayNameAttribute) {
		return nil, model.NewAppError("LDAP.Groups", "api.context.invalid_body_param.app_error", map[string]any{"Name": "GroupIdAttribute / GroupDisplayNameAttribute"}, "", http.StatusBadRequest)
	}
	filter := groupFilter(settings)
	if id != "" {
		idFilter, err := groupIDFilter(*settings.GroupIdAttribute, id)
		if err != nil {
			return nil, model.NewAppError("LDAP.GetGroup", "api.context.invalid_url_param.app_error", map[string]any{"Name": "remote_id"}, "", http.StatusBadRequest)
		}
		filter = "(&" + filter + idFilter + ")"
	}
	d, err := connect(rctx, settings, s.app.GetConfigFile)
	if err != nil {
		return nil, model.NewAppError("LDAP.Groups", "ent.ldap.do_login.unable_to_connect.app_error", nil, "", http.StatusBadGateway).Wrap(err)
	}
	defer d.close()
	entries, err := d.search(settings, *settings.BaseDN, filter, []string{*settings.GroupIdAttribute, *settings.GroupDisplayNameAttribute}, ldapclient.ScopeWholeSubtree)
	if err != nil {
		return nil, model.NewAppError("LDAP.Groups", "ent.ldap_groups.groups_search_error", nil, "", http.StatusBadGateway).Wrap(err)
	}
	groups := make([]*model.Group, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		remoteID := entryID(entry, *settings.GroupIdAttribute)
		name := attributeValue(entry, *settings.GroupDisplayNameAttribute)
		if remoteID == "" || len(remoteID) > model.GroupRemoteIDMaxLength || !utf8.ValidString(remoteID) || name == "" || seen[remoteID] {
			return nil, model.NewAppError("LDAP.Groups", "ent.ldap_groups.groups_search_error", nil, "LDAP groups require unique IDs and display names; check the configured attribute mapping", http.StatusBadGateway)
		}
		seen[remoteID] = true
		groups = append(groups, &model.Group{Source: model.GroupSourceLdap, RemoteId: &remoteID, DisplayName: name})
	}
	return groups, nil
}

func (s *Service) GetGroup(rctx request.CTX, id string) (*model.Group, *model.AppError) {
	settings, appErr := s.groupSettings()
	if appErr != nil {
		return nil, appErr
	}
	if !*settings.Enable && !*settings.EnableSync {
		return nil, model.NewAppError("LDAP.GetGroup", "ent.ldap.disabled.app_error", nil, "", http.StatusForbidden)
	}
	if id == "" {
		return nil, model.NewAppError("LDAP.GetGroup", "api.context.invalid_url_param.app_error", map[string]any{"Name": "remote_id"}, "", http.StatusBadRequest)
	}
	groups, appErr := s.directoryGroups(rctx, settings, id)
	if appErr != nil {
		return nil, appErr
	}
	if len(groups) == 0 {
		return nil, model.NewAppError("LDAP.GetGroup", "api.ldap_group.not_found", nil, "", http.StatusNotFound)
	}
	if len(groups) != 1 {
		return nil, model.NewAppError("LDAP.GetGroup", "ent.ldap_groups.group_search_error", nil, "LDAP group identifier is ambiguous", http.StatusBadGateway)
	}
	return groups[0], nil
}

func (s *Service) GetAllGroupsPage(rctx request.CTX, page, perPage int, opts model.LdapGroupSearchOpts) ([]*model.Group, int, *model.AppError) {
	settings, appErr := s.groupSettings()
	if appErr != nil {
		return nil, 0, appErr
	}
	if page < 0 || perPage < 1 || perPage > 1000 {
		return nil, 0, model.NewAppError("LDAP.GetAllGroupsPage", "api.context.invalid_url_param.app_error", map[string]any{"Name": "page / per_page"}, "", http.StatusBadRequest)
	}
	if !*settings.Enable && !*settings.EnableSync {
		return []*model.Group{}, 0, nil
	}
	groups, appErr := s.directoryGroups(rctx, settings, "")
	if appErr != nil {
		return nil, 0, appErr
	}
	linked, err := s.app.Srv().Store().Group().GetAllBySource(model.GroupSourceLdap)
	if err != nil {
		return nil, 0, model.NewAppError("LDAP.GetAllGroupsPage", "ent.ldap_groups.groups_search_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	byRemoteID := make(map[string]*model.Group, len(linked))
	for _, group := range linked {
		byRemoteID[group.GetRemoteId()] = group
	}
	matched := make([]*model.Group, 0, len(groups))
	query := strings.ToLower(opts.Q)
	for _, group := range groups {
		if !strings.Contains(strings.ToLower(group.DisplayName), query) {
			continue
		}
		stored, isLinked := byRemoteID[group.GetRemoteId()]
		if opts.IsLinked != nil && *opts.IsLinked != isLinked {
			continue
		}
		if isLinked {
			group.Id = stored.Id
		}
		matched = append(matched, group)
	}
	if err := s.populateGroupSyncables(rctx, matched); err != nil {
		return nil, 0, model.NewAppError("LDAP.GetAllGroupsPage", "ent.ldap_groups.groups_search_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	if opts.IsConfigured != nil {
		filtered := matched[:0]
		for _, group := range matched {
			if *opts.IsConfigured == group.HasSyncables {
				filtered = append(filtered, group)
			}
		}
		matched = filtered
	}
	sort.Slice(matched, func(i, j int) bool {
		a, b := strings.ToLower(matched[i].DisplayName), strings.ToLower(matched[j].DisplayName)
		if a == b {
			return matched[i].GetRemoteId() < matched[j].GetRemoteId()
		}
		return a < b
	})
	count := len(matched)
	if page > count/perPage { // Check before multiplying to avoid integer overflow.
		return []*model.Group{}, count, nil
	}
	start := page * perPage
	return matched[start:min(start+perPage, count)], count, nil
}
