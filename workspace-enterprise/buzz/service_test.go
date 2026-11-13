package buzz_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	_ "github.com/mattermost/enterprise"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
)

func TestMain(m *testing.M) {
	mainHelper := testlib.NewMainHelper()
	defer mainHelper.Close()

	api4.SetMainHelper(mainHelper)
	mainHelper.Main(m)
}

func TestBuzzCooldown(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	ctx := context.Background()

	_, response, err := th.Client.CreatePost(ctx, &model.Post{
		ChannelId: th.BasicChannel.Id,
		Message:   "normal message",
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)

	_, response, err = th.Client.CreatePost(ctx, &model.Post{
		ChannelId: th.BasicChannel.Id,
		Message:   "BUZZMESSAGE",
	})
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.Equal(t, "api.license.upgrade_needed.app_error", err.(*model.AppError).Id)

	th.App.Srv().SetLicense(model.NewTestLicense())

	_, response, err = th.Client.CreatePost(ctx, &model.Post{
		ChannelId: th.BasicChannel.Id,
		Message:   "BUZZMESSAGE",
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)

	_, response, err = th.Client.CreatePost(ctx, &model.Post{
		ChannelId: th.BasicChannel.Id,
		Message:   "BUZZMESSAGE",
	})
	require.Error(t, err)
	require.Equal(t, http.StatusTooManyRequests, response.StatusCode)
	require.Equal(t, "api.post.create_post.cooldown.app_error", err.(*model.AppError).Id)

	_, response, err = th.Client.CreatePost(ctx, &model.Post{
		ChannelId: th.BasicChannel2.Id,
		Message:   "BUZZMESSAGE",
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)

	secondUserClient := th.CreateClient()
	th.LoginBasic2WithClient(t, secondUserClient)
	_, response, err = secondUserClient.CreatePost(ctx, &model.Post{
		ChannelId: th.BasicChannel.Id,
		Message:   "BUZZMESSAGE",
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)

	_, appErr := th.App.UpdateUserRoles(
		th.Context,
		th.BasicUser.Id,
		model.SystemUserRoleId+" "+model.SystemAdminRoleId,
		false,
	)
	require.Nil(t, appErr)
	for range 2 {
		_, response, err = th.Client.CreatePost(ctx, &model.Post{
			ChannelId: th.BasicChannel.Id,
			Message:   "BUZZMESSAGE",
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, response.StatusCode)
	}
}
