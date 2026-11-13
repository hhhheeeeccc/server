//go:build integration

package disappearing_messages

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestPolicyChangesRespectRolePermission(t *testing.T) {
	ctx := context.Background()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	channel, _, err := th.Client.CreateDirectChannel(ctx, th.BasicUser.Id, th.BasicUser2.Id)
	require.NoError(t, err)
	path := "/workspace/channels/" + channel.Id + "/disappearing"
	put := func(status int) {
		t.Helper()
		resp, requestErr := th.Client.DoAPIRequestWithHeaders(ctx, http.MethodPut, path, `{"duration_seconds":86400}`, nil)
		require.NotNil(t, resp)
		defer resp.Body.Close()
		require.Equal(t, status, resp.StatusCode)
		if status == http.StatusOK {
			require.NoError(t, requestErr)
		} else {
			require.Error(t, requestErr)
		}
	}
	readPolicy := func() int64 {
		t.Helper()
		resp, requestErr := th.Client.DoAPIRequestWithHeaders(ctx, http.MethodGet, path, "", nil)
		require.NoError(t, requestErr)
		defer resp.Body.Close()
		var p Policy
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
		return p.DurationSeconds
	}
	put(http.StatusForbidden)
	require.Zero(t, readPolicy())
	role, _, err := th.SystemAdminClient.GetRoleByName(ctx, model.SystemUserRoleId)
	require.NoError(t, err)
	original := append([]string(nil), role.Permissions...)
	allowed := append(append([]string(nil), original...), "manage_disappearing_messages")
	updated, _, err := th.SystemAdminClient.PatchRole(ctx, role.Id, &model.RolePatch{Permissions: &allowed})
	require.NoError(t, err)
	require.Contains(t, updated.Permissions, "manage_disappearing_messages")
	put(http.StatusOK)
	require.EqualValues(t, 86400, readPolicy())
	_, _, err = th.SystemAdminClient.PatchRole(ctx, role.Id, &model.RolePatch{Permissions: &original})
	require.NoError(t, err)
	put(http.StatusForbidden)
	require.EqualValues(t, 86400, readPolicy(), "revocation must not rewrite an existing timer")
	result, _, err := th.Client.ExecuteCommandWithTeam(ctx, channel.Id, th.BasicTeam.Id, "/اختفاء إيقاف")
	require.NoError(t, err)
	require.Contains(t, result.Text, "صلاحية")
	require.EqualValues(t, 86400, readPolicy(), "slash commands must enforce the same permission")
	result, _, err = th.Client.ExecuteCommandWithTeam(ctx, channel.Id, th.BasicTeam.Id, "/اختفاء حالة")
	require.NoError(t, err)
	require.Contains(t, result.Text, "24 ساعة")

	adminChannel, _, err := th.SystemAdminClient.CreateDirectChannel(ctx, th.SystemAdminUser.Id, th.BasicUser.Id)
	require.NoError(t, err)
	resp, err := th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut, "/workspace/channels/"+adminChannel.Id+"/disappearing", `{"duration_seconds":86400}`, nil)
	require.NoError(t, err)
	resp.Body.Close()
}
