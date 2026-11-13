package status_lock_test

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

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestLockedPersonalDataBlocksManualStatusChanges(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	ctx := context.Background()
	license := model.NewTestLicense()
	license.Features.LockTeammateNameDisplay = new(true)
	th.App.Srv().SetLicense(license)
	userID := th.BasicUser.Id
	set := func(status string) (*model.Status, *model.Response, error) {
		return th.Client.UpdateUserStatus(ctx, userID, &model.Status{UserId: userID, Status: status})
	}

	// The core route remains available while the setting is off.
	updated, response, err := set(model.StatusAway)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, model.StatusAway, updated.Status)

	th.App.UpdateConfig(func(cfg *model.Config) { cfg.TeamSettings.LockTeammateNameDisplay = new(true) })
	_, response, err = set(model.StatusOffline)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	stored, appErr := th.App.GetStatus(userID)
	require.Nil(t, appErr)
	require.Equal(t, model.StatusAway, stored.Status)

	updated, response, err = th.SystemAdminClient.UpdateUserStatus(ctx, userID, &model.Status{UserId: userID, Status: model.StatusOnline})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, model.StatusOnline, updated.Status)
	_, response, err = th.SystemAdminClient.UpdateUserStatus(ctx, userID, &model.Status{UserId: th.BasicUser2.Id, Status: model.StatusOffline})
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	rawResponse, err := th.Client.DoAPIRequestWithHeaders(ctx, http.MethodPut, "/users/"+userID+"/status", "not-json", nil)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, rawResponse.StatusCode)
	require.NoError(t, rawResponse.Body.Close())
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"user_id":"` + userID + `","status":"away"} true`, http.StatusBadRequest},
		{`{"user_id":"` + userID + `","status":"away","padding":"` + strings.Repeat("x", 8<<10) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		rawResponse, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut, "/users/"+userID+"/status", tc.body, nil)
		require.Error(t, err)
		require.Equal(t, tc.status, rawResponse.StatusCode)
		require.NoError(t, rawResponse.Body.Close())
	}
	stored, appErr = th.App.GetStatus(userID)
	require.Nil(t, appErr)
	require.Equal(t, model.StatusOnline, stored.Status)

	license.Features.LockTeammateNameDisplay = new(false)
	th.App.Srv().SetLicense(license)
	updated, response, err = set(model.StatusAway)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, model.StatusAway, updated.Status)
	license.Features.LockTeammateNameDisplay = new(true)
	th.App.Srv().SetLicense(license)

	th.App.UpdateConfig(func(cfg *model.Config) { cfg.TeamSettings.LockTeammateNameDisplay = new(false) })
	updated, response, err = set(model.StatusOffline)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, model.StatusOffline, updated.Status)
}

func TestLockedPersonalDataBlocksPresenceSlashCommands(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	ctx := context.Background()
	license := model.NewTestLicense()
	license.Features.LockTeammateNameDisplay = new(true)
	th.App.Srv().SetLicense(license)

	response, _, err := th.Client.ExecuteCommand(ctx, th.BasicChannel.Id, "/dnd")
	require.NoError(t, err)
	require.NotEmpty(t, response.Text)

	th.App.UpdateConfig(func(cfg *model.Config) { cfg.TeamSettings.LockTeammateNameDisplay = new(true) })
	for _, command := range []string{"/online", "/متصل", "/away", "/بالخارج", "/offline", "/غير_متصل", "/dnd", "/عدم_الإزعاج"} {
		response, _, err = th.Client.ExecuteCommand(ctx, th.BasicChannel.Id, command)
		require.NoError(t, err, command)
		require.Equal(t, model.CommandResponseTypeEphemeral, response.ResponseType, command)
		require.NotEmpty(t, response.Text, command)
		stored, appErr := th.App.GetStatus(th.BasicUser.Id)
		require.Nil(t, appErr)
		require.Equal(t, model.StatusDnd, stored.Status, command)
	}

	response, _, err = th.SystemAdminClient.ExecuteCommand(ctx, th.BasicChannel.Id, "/online")
	require.NoError(t, err)
	require.NotContains(t, response.Text, "locked")
	stored, appErr := th.App.GetStatus(th.SystemAdminUser.Id)
	require.Nil(t, appErr)
	require.Equal(t, model.StatusOnline, stored.Status)
}

func TestInvalidManualStatusDoesNotDisableAutoResponder(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicense()
	license.Features.LockTeammateNameDisplay = new(true)
	th.App.Srv().SetLicense(license)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.TeamSettings.LockTeammateNameDisplay = new(true) })
	userID := th.BasicUser.Id
	user, appErr := th.App.GetUser(th.Context, userID)
	require.Nil(t, appErr)
	notifyProps := user.NotifyProps
	notifyProps[model.AutoResponderActiveNotifyProp] = "true"
	_, appErr = th.App.PatchUser(th.Context, userID, &model.UserPatch{NotifyProps: notifyProps}, true)
	require.Nil(t, appErr)
	th.App.SetStatusOutOfOffice(userID)

	response, err := th.SystemAdminClient.DoAPIRequestWithHeaders(context.Background(), http.MethodPut,
		"/users/"+userID+"/status", `{"user_id":"`+userID+`","status":"invalid"}`, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.NoError(t, response.Body.Close())
	stored, appErr := th.App.GetUser(th.Context, userID)
	require.Nil(t, appErr)
	require.Equal(t, "true", stored.NotifyProps[model.AutoResponderActiveNotifyProp])
	status, appErr := th.App.GetStatus(userID)
	require.Nil(t, appErr)
	require.Equal(t, model.StatusOutOfOffice, status.Status)
}
