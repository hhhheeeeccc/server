package partial_search_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	_ "github.com/mattermost/enterprise"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestSearchRequestRejectsTrailingAndOversizedJSON(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	ctx := context.Background()
	valid := `{"term":"` + th.BasicUser.Username + `"}`
	users, _, err := th.Client.SearchUsers(ctx, &model.UserSearch{Term: th.BasicUser.Username})
	require.NoError(t, err)
	require.NotEmpty(t, users)
	for _, tc := range []struct {
		body   string
		status int
	}{
		{valid + ` true`, http.StatusBadRequest},
		{valid + strings.Repeat(" ", 1<<20), http.StatusRequestEntityTooLarge},
	} {
		response, requestErr := th.Client.DoAPIPost(ctx, "/users/search", tc.body)
		require.Error(t, requestErr)
		require.Equal(t, tc.status, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
}

func TestScopedSearchIncludesUserVisibleOnlyThroughDirectChannel(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	ctx := context.Background()
	th.App.Srv().SetLicense(model.NewTestLicense())

	role, appErr := th.App.GetRoleByName(th.Context, model.SystemUserRoleId)
	require.Nil(t, appErr)
	originalPermissions := append([]string(nil), role.Permissions...)
	restrictedPermissions := make([]string, 0, len(originalPermissions))
	for _, permission := range originalPermissions {
		if permission != model.PermissionViewMembers.Id {
			restrictedPermissions = append(restrictedPermissions, permission)
		}
	}
	_, appErr = th.App.PatchRole(role, &model.RolePatch{Permissions: &restrictedPermissions})
	require.Nil(t, appErr)
	t.Cleanup(func() {
		_, restoreErr := th.App.PatchRole(role, &model.RolePatch{Permissions: &originalPermissions})
		require.Nil(t, restoreErr)
	})

	id := model.NewId()
	otherTeam, _, err := th.SystemAdminClient.CreateTeam(ctx, &model.Team{
		Name: "search" + id[:18], DisplayName: "Search visibility team", Email: "team-" + id + "@example.com", Type: model.TeamOpen,
	})
	require.NoError(t, err)
	otherUser, appErr := th.App.CreateUser(th.Context, &model.User{
		Username: "search" + id[:18], Email: id + "@example.com", Password: model.NewId(),
	})
	require.Nil(t, appErr)
	_, _, err = th.SystemAdminClient.AddTeamMember(ctx, otherTeam.Id, otherUser.Id)
	require.NoError(t, err)

	search := &model.UserSearch{Term: otherUser.Username, NotInTeamId: th.BasicTeam.Id}
	users, _, err := th.Client.SearchUsers(ctx, search)
	require.NoError(t, err)
	require.Empty(t, users, "users outside visible teams and channels must remain hidden")

	_, _, err = th.SystemAdminClient.CreateDirectChannel(ctx, th.BasicUser.Id, otherUser.Id)
	require.NoError(t, err)
	users, _, err = th.Client.SearchUsers(ctx, search)
	require.NoError(t, err)
	require.Len(t, users, 1, "a direct channel makes the user visible despite separate teams")
	require.Equal(t, otherUser.Id, users[0].Id)
}

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}
