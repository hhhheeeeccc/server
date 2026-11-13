package ldap

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/lib/pq"
	ldapclient "github.com/mattermost/ldap"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

type groupMembershipPlan struct {
	id       string
	previous map[string]bool
	next     map[string]bool
}

// ldapDNKey mirrors DN.Equal: RDN order and attribute values are significant,
// while attribute order within an RDN and case of attribute types are not.
func ldapDNKey(dn *ldapclient.DN) string {
	var key strings.Builder
	writeDNPart(&key, strconv.Itoa(len(dn.RDNs)))
	for _, rdn := range dn.RDNs {
		attributes := make([]string, 0, len(rdn.Attributes))
		for _, attribute := range rdn.Attributes {
			var part strings.Builder
			writeDNPart(&part, foldLDAPAttributeType(attribute.Type))
			writeDNPart(&part, attribute.Value)
			attributes = append(attributes, part.String())
		}
		sort.Strings(attributes)
		writeDNPart(&key, strconv.Itoa(len(attributes)))
		for i, attribute := range attributes {
			if i > 0 && attribute == attributes[i-1] {
				continue
			}
			writeDNPart(&key, attribute)
		}
	}
	return key.String()
}

func writeDNPart(key *strings.Builder, part string) {
	key.WriteString(strconv.Itoa(len(part)))
	key.WriteByte(':')
	key.WriteString(part)
}

func foldLDAPAttributeType(value string) string {
	var folded strings.Builder
	for _, r := range value {
		lowest := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < lowest {
				lowest = next
			}
		}
		folded.WriteRune(lowest)
	}
	return folded.String()
}

// Read and validate the entire snapshot before changing any group memberships.
func (s *Service) planGroupMemberships(rctx request.CTX, cfg model.LdapSettings, local []*model.User, active map[string]*model.User, remoteIDs map[string]string, includeSAML bool) ([]groupMembershipPlan, *model.AppError) {
	linked, err := s.app.Srv().Store().Group().GetAllBySource(model.GroupSourceLdap)
	if err != nil {
		return nil, ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
	}
	if len(linked) == 0 {
		return nil, nil
	}
	if _, appErr := s.groupSettings(); appErr != nil {
		return nil, appErr
	}
	d, entries, appErr := s.userEntries(rctx, cfg, "", "", []string{*cfg.IdAttribute, "uid"})
	if appErr != nil {
		return nil, appErr
	}
	defer d.close()
	groups, err := d.search(cfg, *cfg.BaseDN, groupFilter(cfg), []string{*cfg.GroupIdAttribute, "member", "uniqueMember", "memberUid"}, ldapclient.ScopeWholeSubtree)
	if err != nil {
		return nil, ldapError("ent.ldap.search_failed.app_error", http.StatusBadGateway)
	}
	userDNs := make(map[string]*ldapclient.DN)
	usernames := make(map[string]string)
	for _, e := range entries {
		id := entryID(e, *cfg.IdAttribute)
		if active[id] == nil {
			continue
		}
		dn, err := ldapclient.ParseDN(e.DN)
		if err != nil || userDNs[id] != nil {
			return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
		}
		userDNs[id] = dn
		usernames[id] = attributeValue(e, "uid")
	}
	groupDNs := make([]*ldapclient.DN, len(groups))
	groupIDs := map[string]int{}
	members := make([][]*ldapclient.DN, len(groups))
	posixMembers := make([]map[string]bool, len(groups))
	for i, e := range groups {
		if appErr := resolveRangedMembers(d, cfg, e); appErr != nil {
			return nil, appErr
		}
		id := entryID(e, *cfg.GroupIdAttribute)
		if _, duplicate := groupIDs[id]; duplicate || id == "" {
			return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
		}
		groupIDs[id] = i
		groupDNs[i], err = ldapclient.ParseDN(e.DN)
		if err != nil {
			return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
		}
		posixMembers[i] = map[string]bool{}
		for _, attr := range e.Attributes {
			name := strings.ToLower(attr.Name)
			if name == "memberuid" {
				for _, uid := range attr.Values {
					posixMembers[i][uid] = true
				}
				continue
			}
			if name != "member" && name != "uniquemember" {
				continue
			}
			for _, value := range attr.Values {
				dn, err := ldapclient.ParseDN(value)
				if err != nil {
					return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
				}
				members[i] = append(members[i], dn)
			}
		}
	}
	// Index parsed DNs once instead of comparing every member with every group.
	groupByDN := make(map[string][]int, len(groups))
	for index, dn := range groupDNs {
		key := ldapDNKey(dn)
		groupByDN[key] = append(groupByDN[key], index)
	}
	children := make([][]int, len(groups))
	directByDN := make(map[string][]int)
	directByUID := make(map[string][]int)
	for parent, groupMembers := range members {
		for _, dn := range groupMembers {
			key := ldapDNKey(dn)
			directByDN[key] = append(directByDN[key], parent)
			children[parent] = append(children[parent], groupByDN[key]...)
		}
		for uid := range posixMembers[parent] {
			directByUID[uid] = append(directByUID[uid], parent)
		}
	}
	parents := make([][]int, len(groups))
	for parent, groupChildren := range children {
		for _, child := range groupChildren {
			parents[child] = append(parents[child], parent)
		}
	}
	// Seed direct memberships and walk towards parent groups. This handles
	// cycles without traversing the same group graph for every linked group.
	membersByGroup := make([]map[string]bool, len(groups))
	visited := make([]int, len(groups))
	visitID := 0
	for _, user := range local {
		if rctx.Context().Err() != nil {
			return nil, ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
		}
		remoteID := remoteIDs[user.Id]
		userDN := userDNs[remoteID]
		if userDN == nil {
			continue
		}
		visitID++
		queue := make([]int, 0)
		for _, index := range directByUID[usernames[remoteID]] {
			if visited[index] != visitID {
				visited[index] = visitID
				queue = append(queue, index)
			}
		}
		for _, index := range directByDN[ldapDNKey(userDN)] {
			if visited[index] != visitID {
				visited[index] = visitID
				queue = append(queue, index)
			}
		}
		for len(queue) > 0 {
			index := queue[0]
			queue = queue[1:]
			if membersByGroup[index] == nil {
				membersByGroup[index] = map[string]bool{}
			}
			membersByGroup[index][user.Id] = true
			for _, parent := range parents[index] {
				if visited[parent] != visitID {
					visited[parent] = visitID
					queue = append(queue, parent)
				}
			}
		}
	}
	// Include deactivated users, which the public member-list helper omits.
	// Read all linked groups in one query so a large policy does not issue one
	// database round trip per group.
	linkedIDs := make([]string, 0, len(linked))
	for _, group := range linked {
		linkedIDs = append(linkedIDs, group.Id)
	}
	rows, err := s.app.Srv().Store().GetInternalMasterDB().QueryContext(rctx.Context(),
		`SELECT gm.GroupId, gm.UserId, u.AuthService, u.Roles
		 FROM GroupMembers gm JOIN Users u ON u.Id=gm.UserId
		 WHERE gm.GroupId=ANY($1::text[]) AND gm.DeleteAt=0
		 AND (u.AuthService=$2 OR ($3 AND u.AuthService=$4))`,
		pq.Array(linkedIDs), model.UserAuthServiceLdap, includeSAML, model.UserAuthServiceSaml)
	if err != nil {
		return nil, ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
	}
	previousByGroup := make(map[string]map[string]bool, len(linked))
	ignoreSAMLGuests := model.SafeDereference(s.app.Config().SamlSettings.IgnoreGuestsLdapSync)
	for rows.Next() {
		var groupID, userID, authService, roles string
		if err = rows.Scan(&groupID, &userID, &authService, &roles); err != nil {
			rows.Close()
			return nil, ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
		}
		if authService == model.UserAuthServiceSaml && ignoreSAMLGuests &&
			strings.Contains(" "+roles+" ", " "+model.SystemGuestRoleId+" ") {
			continue
		}
		if previousByGroup[groupID] == nil {
			previousByGroup[groupID] = map[string]bool{}
		}
		previousByGroup[groupID][userID] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
	}
	plans := make([]groupMembershipPlan, 0, len(linked))
	for _, group := range linked {
		previous := previousByGroup[group.Id]
		if previous == nil {
			previous = map[string]bool{}
		}
		plan := groupMembershipPlan{id: group.Id, next: map[string]bool{}, previous: previous}
		index, found := groupIDs[group.GetRemoteId()]
		if found {
			for userID := range membersByGroup[index] {
				plan.next[userID] = true
			}
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func (s *Service) applyGroupMemberships(rctx request.CTX, plans []groupMembershipPlan) *model.AppError {
	for _, plan := range plans {
		if rctx.Context().Err() != nil {
			return ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
		}
		for id := range plan.previous {
			if rctx.Context().Err() != nil {
				return ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
			}
			if !plan.next[id] {
				if _, err := s.app.DeleteGroupMember(plan.id, id); err != nil {
					return err
				}
			}
		}
		for id := range plan.next {
			if rctx.Context().Err() != nil {
				return ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
			}
			if !plan.previous[id] {
				if _, err := s.app.UpsertGroupMember(plan.id, id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Service) syncGroupRoles(rctx request.CTX, plans []groupMembershipPlan) *model.AppError {
	seen := map[string]bool{}
	for _, plan := range plans {
		for _, kind := range []model.GroupSyncableType{model.GroupSyncableTypeTeam, model.GroupSyncableTypeChannel} {
			if rctx.Context().Err() != nil {
				return ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
			}
			links, err := s.app.Srv().Store().Group().GetAllGroupSyncablesByGroupId(plan.id, kind)
			if err != nil {
				return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
			}
			for _, link := range links {
				if rctx.Context().Err() != nil {
					return ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
				}
				key := string(kind) + link.SyncableId
				if link.DeleteAt != 0 || seen[key] {
					continue
				}
				seen[key] = true
				if appErr := s.app.SyncSyncableRoles(rctx, link.SyncableId, kind); appErr != nil {
					return appErr
				}
			}
		}
	}
	return nil
}
