package ldap

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	ldapclient "github.com/mattermost/ldap"
	"github.com/mattermost/mattermost/server/public/model"
)

var rangedMemberResponse = regexp.MustCompile(`(?i)^(member|uniqueMember|memberUid);range=([0-9]+)-([0-9]+|\*)$`)

// AD returns large membership attributes in chunks. A missing or overlapping
// chunk must fail the snapshot, since treating it as complete would remove users.
func resolveRangedMembers(d *directory, cfg model.LdapSettings, entry *ldapclient.Entry) *model.AppError {
	type rangeState struct {
		name   string
		next   int
		values []string
	}
	ranges := map[string]*rangeState{}
	plain := map[string]bool{}
	for _, attr := range entry.Attributes {
		name := strings.ToLower(attr.Name)
		if name == "member" || name == "uniquemember" || name == "memberuid" {
			plain[name] = true
			continue
		}
		if !strings.Contains(name, ";range=") {
			continue
		}
		parts := rangedMemberResponse.FindStringSubmatch(attr.Name)
		if len(parts) == 0 {
			return ldapError("ent.ldap.ranged_members.app_error", http.StatusBadGateway)
		}
		kind := strings.ToLower(parts[1])
		start, err := strconv.Atoi(parts[2])
		if err != nil || start != 0 || ranges[kind] != nil || plain[kind] {
			return ldapError("ent.ldap.ranged_members.app_error", http.StatusBadGateway)
		}
		state := &rangeState{name: parts[1], values: append([]string(nil), attr.Values...)}
		if parts[3] != "*" {
			end, err := strconv.Atoi(parts[3])
			if err != nil || end < start || end == int(^uint(0)>>1) || end-start+1 != len(attr.Values) {
				return ldapError("ent.ldap.ranged_members.app_error", http.StatusBadGateway)
			}
			state.next = end + 1
		}
		ranges[kind] = state
	}
	for kind, state := range ranges {
		if plain[kind] {
			return ldapError("ent.ldap.ranged_members.app_error", http.StatusBadGateway)
		}
		for state.next != 0 {
			requested := state.next
			attribute := state.name + ";range=" + strconv.Itoa(requested) + "-*"
			entries, err := d.search(cfg, entry.DN, "(objectClass=*)", []string{attribute}, ldapclient.ScopeBaseObject)
			if err != nil || len(entries) != 1 || !strings.EqualFold(entries[0].DN, entry.DN) {
				return ldapError("ent.ldap.ranged_members.app_error", http.StatusBadGateway)
			}
			var matched *ldapclient.EntryAttribute
			var endText string
			for _, attr := range entries[0].Attributes {
				parts := rangedMemberResponse.FindStringSubmatch(attr.Name)
				if len(parts) == 0 || !strings.EqualFold(parts[1], kind) {
					continue
				}
				start, parseErr := strconv.Atoi(parts[2])
				if parseErr != nil || start != requested || matched != nil {
					return ldapError("ent.ldap.ranged_members.app_error", http.StatusBadGateway)
				}
				matched, endText = attr, parts[3]
			}
			if matched == nil || len(matched.Values) == 0 {
				return ldapError("ent.ldap.ranged_members.app_error", http.StatusBadGateway)
			}
			if endText == "*" {
				state.next = 0
			} else {
				end, parseErr := strconv.Atoi(endText)
				if parseErr != nil || end < requested || end-requested+1 != len(matched.Values) || end == int(^uint(0)>>1) {
					return ldapError("ent.ldap.ranged_members.app_error", http.StatusBadGateway)
				}
				state.next = end + 1
			}
			state.values = append(state.values, matched.Values...)
		}
		entry.Attributes = append(entry.Attributes, ldapclient.NewEntryAttribute(state.name, state.values))
	}
	return nil
}
