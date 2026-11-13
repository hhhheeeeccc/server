package user_lookup_test

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

func TestUserLookupEmptyListLicenseAndValidation(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	ctx := context.Background()

	_, response, err := th.Client.GetUsersByIds(ctx, []string{})
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode, "without Enterprise the original API rejects an empty list")

	th.App.Srv().SetLicense(model.NewTestLicense())
	users, response, err := th.Client.GetUsersByIds(ctx, []string{})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Empty(t, users)

	invalidJSON, err := th.Client.DoAPIPost(ctx, "/users/ids", "null")
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, invalidJSON.StatusCode)
	require.NoError(t, invalidJSON.Body.Close())
	trailingJSON, err := th.Client.DoAPIPost(ctx, "/users/ids", "[] true")
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, trailingJSON.StatusCode)
	require.NoError(t, trailingJSON.Body.Close())
	tooLarge, err := th.Client.DoAPIPost(ctx, "/users/ids", "[]"+strings.Repeat(" ", 1<<20))
	require.Error(t, err)
	require.Equal(t, http.StatusRequestEntityTooLarge, tooLarge.StatusCode)
	require.NoError(t, tooLarge.Body.Close())

	invalidSince, err := th.Client.DoAPIPost(ctx, "/users/ids?since=not-a-number", "[]")
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, invalidSince.StatusCode)
	require.NoError(t, invalidSince.Body.Close())

	users, _, err = th.Client.GetUsersByIds(ctx, []string{th.BasicUser.Id, th.BasicUser.Id})
	require.NoError(t, err)
	require.Len(t, users, 1)
	require.Equal(t, th.BasicUser.Id, users[0].Id)

	outsider, appErr := th.App.CreateUser(th.Context, &model.User{
		Username: model.NewUsername(), Email: model.NewId() + "@example.com", Password: model.NewId(),
	})
	require.Nil(t, appErr)
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
	defer func() {
		_, restoreErr := th.App.PatchRole(role, &model.RolePatch{Permissions: &originalPermissions})
		require.Nil(t, restoreErr)
	}()
	restrictions, appErr := th.App.GetViewUsersRestrictions(th.Context, th.BasicUser.Id)
	require.Nil(t, appErr)
	require.NotNil(t, restrictions)
	users, _, err = th.Client.GetUsersByIds(ctx, []string{th.BasicUser.Id, outsider.Id})
	require.NoError(t, err)
	require.Len(t, users, 1, "a user outside visible teams and channels must not leak through the Enterprise route")
	require.Equal(t, th.BasicUser.Id, users[0].Id)
}

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}
