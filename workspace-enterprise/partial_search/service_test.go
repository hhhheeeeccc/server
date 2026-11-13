package partial_search

import (
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
)

func TestNormalizeArabicForSearch(t *testing.T) {
	tests := map[string]string{
		"أحمد":       "احمد",
		"إبراهيم":    "ابراهيم",
		"آمال":       "امال",
		"ٱسماء":      "اسما",
		"مُحَمَّد":   "محمد",
		"عليٌّ":      "علي",
		"مــروان":    "مروان",
		"John Smith": "John Smith",
	}

	for input, expected := range tests {
		t.Run(input, func(t *testing.T) {
			if actual := normalizeArabicForSearch(input); actual != expected {
				t.Fatalf("expected %q, got %q", expected, actual)
			}
		})
	}
}

func TestBuildUserQueryNormalizesTermsAndKeepsRestrictions(t *testing.T) {
	query, args := buildUserQuery(
		"team-id",
		"channel-id",
		[]string{"channel-id", "visible-channel"},
		"أَيْمَن",
		&model.UserSearchOptions{AllowFullNames: true, Limit: 25},
		true,
	)

	for _, fragment := range []string{
		"FROM users u",
		"FROM teammembers tm",
		"FROM channelmembers cm",
		"FROM channelmembers rcm",
		"translate(translate(COALESCE(u.firstname",
		"u.deleteat = 0",
	} {
		if !strings.Contains(query, fragment) {
			t.Errorf("query is missing %q: %s", fragment, query)
		}
	}
	if strings.Contains(query, "أَيْمَن") || strings.Contains(query, "ايمن") {
		t.Fatalf("user input must be bound instead of embedded in SQL: %s", query)
	}

	foundNormalizedTerm := false
	for _, arg := range args {
		if arg == "%ايمن%" {
			foundNormalizedTerm = true
			break
		}
	}
	if !foundNormalizedTerm {
		t.Fatalf("expected normalized search term in bound arguments: %#v", args)
	}
}

func TestBuildUserQueryOmitsPrivateFields(t *testing.T) {
	query, _ := buildUserQuery("", "", nil, "marwan", &model.UserSearchOptions{Limit: 10}, true)
	if strings.Contains(query, "u.firstname") || strings.Contains(query, "u.lastname") || strings.Contains(query, "u.email") {
		t.Fatalf("query searched fields that the caller cannot view: %s", query)
	}
}

func TestBuildUserQueryPreservesRoleSemantics(t *testing.T) {
	query, args := buildUserQuery(
		"team-id",
		"channel-id",
		nil,
		"",
		&model.UserSearchOptions{
			Role:         model.SystemUserRoleId,
			Roles:        []string{model.SystemAdminRoleId},
			TeamRoles:    []string{model.TeamAdminRoleId},
			ChannelRoles: []string{model.ChannelAdminRoleId},
			Limit:        20,
		},
		true,
	)

	if !strings.Contains(query, "u.roles") || !strings.Contains(query, "FROM teammembers tm") || !strings.Contains(query, "FROM channelmembers cm") {
		t.Fatalf("role scopes are missing from query: %s", query)
	}
	if !strings.Contains(query, "OR EXISTS (SELECT 1 FROM teammembers tm") || !strings.Contains(query, "OR EXISTS (SELECT 1 FROM channelmembers cm") {
		t.Fatalf("system, team, and channel role filters must be alternatives: %s", query)
	}

	foundSystemRole := false
	for _, arg := range args {
		if arg == "% "+escapeLike(model.SystemAdminRoleId)+" %" {
			foundSystemRole = true
			break
		}
	}
	if !foundSystemRole {
		t.Fatalf("system role was not bound safely: %#v", args)
	}
}

func TestBuildScopedUserQueryNormalizesEveryMemberSearchAndKeepsRoles(t *testing.T) {
	tests := map[string]model.UserSearch{
		"without team":    {Term: "إِبْرَاهِيم", WithoutTeam: true, Limit: 20},
		"inside channel":  {Term: "إِبْرَاهِيم", TeamId: "team-id", InChannelId: "channel-id", Limit: 20},
		"outside channel": {Term: "إِبْرَاهِيم", TeamId: "team-id", NotInChannelId: "channel-id", Limit: 20},
		"outside team":    {Term: "إِبْرَاهِيم", NotInTeamId: "team-id", Limit: 20},
		"inside group":    {Term: "إِبْرَاهِيم", InGroupId: "group-id", Limit: 20},
		"outside group":   {Term: "إِبْرَاهِيم", NotInGroupId: "group-id", Limit: 20},
	}

	for name, props := range tests {
		t.Run(name, func(t *testing.T) {
			query, args := buildScopedUserQuery(&props, &model.UserSearchOptions{
				AllowFullNames: true,
				Limit:          props.Limit,
				Roles:          []string{model.SystemAdminRoleId},
				TeamRoles:      []string{model.TeamAdminRoleId},
				ChannelRoles:   []string{model.ChannelAdminRoleId},
				ViewRestrictions: &model.ViewUsersRestrictions{
					Teams:    []string{"visible-team"},
					Channels: []string{"visible-channel"},
				},
			})

			for _, fragment := range []string{"translate(translate(COALESCE(u.firstname", "FROM teammembers rtm", "FROM channelmembers rcm", "u.roles"} {
				if !strings.Contains(query, fragment) {
					t.Errorf("query is missing %q: %s", fragment, query)
				}
			}
			foundNormalizedTerm := false
			for _, arg := range args {
				if arg == "%ابراهيم%" {
					foundNormalizedTerm = true
					break
				}
			}
			if !foundNormalizedTerm {
				t.Fatalf("normalized Arabic term is missing from arguments: %#v", args)
			}
		})
	}
}

func TestBuildViewRestrictionConditionsBlocksAnEmptyPermissionSet(t *testing.T) {
	builder := &sqlQueryBuilder{}
	conditions := buildViewRestrictionConditions(builder, &model.ViewUsersRestrictions{Teams: []string{}, Channels: []string{}})
	if len(conditions) != 1 || conditions[0] != "FALSE" {
		t.Fatalf("expected an empty permission set to block all results, got %#v", conditions)
	}
	conditions = buildViewRestrictionConditions(builder, &model.ViewUsersRestrictions{})
	if len(conditions) != 1 || conditions[0] != "FALSE" {
		t.Fatalf("missing team and channel restrictions must block all results, got %#v", conditions)
	}
}

func TestBuildViewRestrictionConditionsAllowsSharedTeamOrChannel(t *testing.T) {
	builder := &sqlQueryBuilder{}
	conditions := buildViewRestrictionConditions(builder, &model.ViewUsersRestrictions{
		Teams:    []string{"visible-team"},
		Channels: []string{"visible-channel"},
	})
	if len(conditions) != 1 || !strings.Contains(conditions[0], ") OR EXISTS (") {
		t.Fatalf("team and channel visibility must be alternatives, got %#v", conditions)
	}
	if len(builder.args) != 2 || builder.args[0] != "visible-team" || builder.args[1] != "visible-channel" {
		t.Fatalf("visibility scopes must be bound as query arguments, got %#v", builder.args)
	}
}

func TestInChannelAutocompleteDoesNotRequireTeamMembership(t *testing.T) {
	query, _ := buildUserQuery("", "direct-channel", nil, "أحمد", &model.UserSearchOptions{Limit: 20}, true)
	if strings.Contains(query, "FROM teammembers tm") || strings.Contains(query, "FROM channelmembers rcm") {
		t.Fatalf("in-channel results must rely on the already-authorized target channel only: %s", query)
	}
	if !strings.Contains(query, "FROM channelmembers cm") {
		t.Fatalf("target channel membership is missing: %s", query)
	}
}

func TestConfiguredSearchEngineEnabled(t *testing.T) {
	config := &model.Config{}
	config.SetDefaults()
	if configuredSearchEngineEnabled(config) {
		t.Fatal("default configuration must use Workspace partial search")
	}

	config.ElasticsearchSettings.EnableSearching = model.NewPointer(true)
	if !configuredSearchEngineEnabled(config) {
		t.Fatal("explicit external search configuration must be preserved")
	}
}
