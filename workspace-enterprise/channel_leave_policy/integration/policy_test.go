//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/mattermost/enterprise" // exercise the production registration
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// These helpers create and drop databases. Require a temporary local Unix
	// socket explicitly; never fall back to development or deployment settings.
	dsn, err := url.Parse(os.Getenv("TEST_DATABASE_POSTGRESQL_DSN"))
	if err != nil || dsn == nil || dsn.Scheme != "postgres" || dsn.Hostname() != "" || dsn.Port() == "" ||
		!strings.HasPrefix(dsn.Query().Get("host"), "/tmp/workspace-channel-leave-") {
		fmt.Fprintln(os.Stderr, "integration tests require an isolated PostgreSQL socket under /tmp/workspace-channel-leave-*")
		os.Exit(1)
	}
	helper := testlib.NewMainHelperWithOptions(&testlib.HelperOptions{
		EnableStore: true, EnableResources: true, RunParallel: true, Parallelism: 1,
	})
	api4.SetMainHelper(helper)
	defer helper.Close()
	helper.Main(m)
}

func TestChannelLeavePolicy(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		*cfg.CacheSettings.CacheType = model.CacheTypeLRU
		*cfg.ServiceSettings.EnableCommands = true
	}).InitBasic(t)
	ctx := context.Background()
	_, appErr := th.App.UpdateTeamMemberSchemeRoles(th.Context, th.BasicTeam.Id, th.BasicUser.Id, false, true, false)
	require.Nil(t, appErr)

	for _, licensed := range []bool{false, true} {
		if licensed {
			th.App.Srv().SetLicense(model.NewTestLicense()) // test fixture only
		} else {
			th.App.Srv().SetLicense(nil)
		}
		for _, channelType := range []model.ChannelType{model.ChannelTypeOpen, model.ChannelTypePrivate} {
			t.Run(fmt.Sprintf("licensed=%v/type=%s", licensed, channelType), func(t *testing.T) {
				channel := th.CreateChannelWithClient(t, th.SystemAdminClient, channelType)
				_, _, err := th.SystemAdminClient.AddChannelMember(ctx, channel.Id, th.BasicUser.Id)
				require.NoError(t, err)
				// A channel administrator must still be denied voluntary leave.
				_, err = th.SystemAdminClient.UpdateChannelMemberSchemeRoles(ctx, channel.Id, th.BasicUser.Id, &model.SchemeRoles{SchemeUser: true, SchemeAdmin: true})
				require.NoError(t, err)
				response, err := th.Client.RemoveUserFromChannel(ctx, channel.Id, th.BasicUser.Id)
				require.Error(t, err)
				require.Equal(t, http.StatusForbidden, response.StatusCode)
				for _, command := range []string{"/leave", "/remove @" + th.BasicUser.Username, "/kick " + strings.ToUpper(th.BasicUser.Username)} {
					_, response, err := th.Client.ExecuteCommand(ctx, channel.Id, command)
					require.Error(t, err, command)
					require.Equal(t, http.StatusForbidden, response.StatusCode, command)
				}
				if licensed {
					for _, command := range []string{"/مغادرة", "/إزالة @" + th.BasicUser.Username, "/طرد @" + th.BasicUser.Username} {
						_, response, err := th.Client.ExecuteCommand(ctx, channel.Id, command)
						require.Error(t, err, command)
						require.Equal(t, http.StatusForbidden, response.StatusCode, command)
					}
				}
				_, _, err = th.Client.GetChannelMember(ctx, channel.Id, th.BasicUser.Id, "")
				require.NoError(t, err, "denials must leave membership intact")

				// Administrating another team must not grant leave here.
				otherTeam := th.CreateTeam(t)
				th.LinkUserToTeam(t, th.BasicUser, otherTeam)
				_, appErr := th.App.UpdateTeamMemberSchemeRoles(th.Context, otherTeam.Id, th.BasicUser.Id, false, true, true)
				require.Nil(t, appErr)
				response, err = th.Client.RemoveUserFromChannel(ctx, channel.Id, th.BasicUser.Id)
				require.Error(t, err)
				require.Equal(t, http.StatusForbidden, response.StatusCode)

				// Removal of other members still uses native permissions.
				_, _, err = th.SystemAdminClient.AddChannelMember(ctx, channel.Id, th.BasicUser2.Id)
				require.NoError(t, err)
				_, err = th.SystemAdminClient.RemoveUserFromChannel(ctx, channel.Id, th.BasicUser2.Id)
				require.NoError(t, err)
				_, err = th.SystemAdminClient.RemoveUserFromChannel(ctx, channel.Id, th.SystemAdminUser.Id)
				require.NoError(t, err)

				_, appErr = th.App.UpdateTeamMemberSchemeRoles(th.Context, th.BasicTeam.Id, th.BasicUser.Id, false, true, true)
				require.Nil(t, appErr)
				// All three command providers must delegate successfully for
				// the administrator of this channel's team.
				for _, command := range []string{"/leave", "/remove @" + th.BasicUser.Username, "/kick @" + th.BasicUser.Username} {
					_, _, err = th.Client.ExecuteCommand(ctx, channel.Id, command)
					require.NoError(t, err, command)
					_, _, err = th.SystemAdminClient.AddChannelMember(ctx, channel.Id, th.BasicUser.Id)
					require.NoError(t, err)
				}
				_, err = th.Client.RemoveUserFromChannel(ctx, channel.Id, th.BasicUser.Id)
				require.NoError(t, err)
				_, appErr = th.App.UpdateTeamMemberSchemeRoles(th.Context, th.BasicTeam.Id, th.BasicUser.Id, false, true, false)
				require.Nil(t, appErr)
			})
		}
	}
}
