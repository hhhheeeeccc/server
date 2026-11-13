package channel_management_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	_ "github.com/mattermost/enterprise"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestBulkRenameAndChannelNameQueries(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	ctx := context.Background()
	id := model.NewId()
	name := "rename" + id[:15]
	newDisplayName := "New channel display name"
	team, _, err := th.SystemAdminClient.CreateTeam(ctx, &model.Team{
		DisplayName: "Second team " + id[:8],
		Name:        "second" + id[:18],
		Email:       "second-" + id + "@example.com",
		Type:        model.TeamOpen,
	})
	require.NoError(t, err)
	createChannel := func(teamID string) *model.Channel {
		channel, _, createErr := th.SystemAdminClient.CreateChannel(ctx, &model.Channel{
			TeamId: teamID, Name: name, DisplayName: "Old channel display name", Type: model.ChannelTypeOpen,
		})
		require.NoError(t, createErr)
		return channel
	}
	first := createChannel(th.BasicTeam.Id)
	second := createChannel(team.Id)
	requestBody, err := json.Marshal(map[string]string{"old_name": name, "new_display_name": newDisplayName})
	require.NoError(t, err)

	countPath := "/channels/count_by_name?name=" + name
	response, err := th.SystemAdminClient.DoAPIGet(ctx, countPath, "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPost, "/channels/bulk_rename", string(requestBody), nil)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())
	unchanged, _, err := th.SystemAdminClient.GetChannel(ctx, first.Id)
	require.NoError(t, err)
	require.Equal(t, "Old channel display name", unchanged.DisplayName)

	th.App.Srv().SetLicense(model.NewTestLicense())
	response, err = th.Client.DoAPIGet(ctx, countPath, "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())

	response, err = th.SystemAdminClient.DoAPIGet(ctx, countPath, "")
	require.NoError(t, err)
	var count struct {
		Count int64 `json:"count"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&count))
	require.NoError(t, response.Body.Close())
	require.Equal(t, int64(2), count.Count)

	response, err = th.Client.DoAPIRequestWithHeaders(ctx, http.MethodPost, "/channels/bulk_rename", string(requestBody), nil)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"old_name":"` + name + `","new_display_name":"rejected","extra":true}`, http.StatusBadRequest},
		{`{"old_name":"` + name + `","new_display_name":"rejected"} true`, http.StatusBadRequest},
		{`{"old_name":"` + name + `","new_display_name":"rejected"}` + strings.Repeat(" ", 4096), http.StatusRequestEntityTooLarge},
	} {
		response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPost, "/channels/bulk_rename", tc.body, nil)
		require.Error(t, err)
		require.Equal(t, tc.status, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
	unchanged, _, err = th.SystemAdminClient.GetChannel(ctx, first.Id)
	require.NoError(t, err)
	require.Equal(t, "Old channel display name", unchanged.DisplayName)

	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPost, "/channels/bulk_rename", string(requestBody), nil)
	require.NoError(t, err)
	require.NoError(t, json.NewDecoder(response.Body).Decode(&count))
	require.NoError(t, response.Body.Close())
	require.Equal(t, int64(2), count.Count)

	for _, channel := range []*model.Channel{first, second} {
		updated, _, getErr := th.SystemAdminClient.GetChannel(ctx, channel.Id)
		require.NoError(t, getErr)
		require.Equal(t, newDisplayName, updated.DisplayName)
	}
	var indexedCount int64
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM publicchannels WHERE name=$1 AND displayname=$2 AND deleteat=0`,
		name, newDisplayName).Scan(&indexedCount))
	require.Equal(t, int64(2), indexedCount)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/channels/autocomplete_names?term="+name, "")
	require.NoError(t, err)
	var names []struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&names))
	require.NoError(t, response.Body.Close())
	require.Len(t, names, 2)
	for _, item := range names {
		require.Equal(t, name, item.Name)
		require.Equal(t, newDisplayName, item.DisplayName)
	}
}

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}
