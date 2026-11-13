package post_ack_batch_test

import (
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

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestBatchAcknowledgementRejectsMissingPostBeforeWrites(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)

	path := "/users/" + th.BasicUser.Id + "/posts/ack_batch"
	requestBody, err := json.Marshal(map[string][]string{"post_ids": {th.BasicPost.Id}})
	require.NoError(t, err)
	response, requestErr := th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPost, path, string(requestBody), nil)
	require.Error(t, requestErr)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	api4.CheckErrorID(t, requestErr, "api.license.upgrade_needed.app_error")
	require.NoError(t, response.Body.Close())

	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuProfessional)))
	otherUserPath := "/users/" + th.BasicUser2.Id + "/posts/ack_batch"
	requestBody, err = json.Marshal(map[string][]string{"post_ids": {th.BasicPost.Id}})
	require.NoError(t, err)
	response, requestErr = th.Client.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPost, otherUserPath, string(requestBody), nil)
	require.Error(t, requestErr)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())
	_, err = th.Store.PostAcknowledgement().Get(th.BasicPost.Id, th.BasicUser2.Id)
	require.Error(t, err, "denied cross-account request must not create an acknowledgement")

	requestBody, err = json.Marshal(map[string][]string{"post_ids": {th.BasicPost.Id, model.NewId()}})
	require.NoError(t, err)
	response, requestErr = th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPost, path, string(requestBody), nil)
	require.Error(t, requestErr)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	api4.CheckErrorID(t, requestErr, "app.post.get.app_error")
	require.NoError(t, response.Body.Close())
	_, err = th.Store.PostAcknowledgement().Get(th.BasicPost.Id, th.BasicUser.Id)
	require.Error(t, err, "the valid post must remain unacknowledged when another post is missing")

	requestBody, err = json.Marshal(map[string][]string{"post_ids": {th.BasicPost.Id, th.BasicPost.Id}})
	require.NoError(t, err)
	response, requestErr = th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPost, path, string(requestBody), nil)
	require.NoError(t, requestErr)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	var saved []*model.PostAcknowledgement
	require.NoError(t, json.NewDecoder(response.Body).Decode(&saved))
	require.NoError(t, response.Body.Close())
	require.Len(t, saved, 1, "duplicate IDs must produce one acknowledgement")
	_, err = th.Store.PostAcknowledgement().Get(th.BasicPost.Id, th.BasicUser.Id)
	require.NoError(t, err)
}

func TestBatchAcknowledgementRejectsUnreadablePostBeforeWrites(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuProfessional)))

	_, appErr := th.App.AddUserToChannel(th.Context, th.BasicUser2, th.BasicPrivateChannel2, false)
	require.Nil(t, appErr)
	privatePost, _, appErr := th.App.CreatePostAsUser(th.Context, &model.Post{
		UserId:    th.BasicUser2.Id,
		ChannelId: th.BasicPrivateChannel2.Id,
		Message:   "private acknowledgement test",
	}, "", false)
	require.Nil(t, appErr)

	path := "/users/" + th.BasicUser.Id + "/posts/ack_batch"
	requestBody, err := json.Marshal(map[string][]string{"post_ids": {th.BasicPost.Id, privatePost.Id}})
	require.NoError(t, err)
	response, requestErr := th.Client.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPost, path, string(requestBody), nil)
	require.Error(t, requestErr)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())
	_, err = th.Store.PostAcknowledgement().Get(th.BasicPost.Id, th.BasicUser.Id)
	require.Error(t, err, "readable post must remain unacknowledged when another post is private")
}

func TestBatchAcknowledgementRejectsMalformedOrOversizedBodyBeforeWrites(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuProfessional)))
	path := "/users/" + th.BasicUser.Id + "/posts/ack_batch"
	cases := []struct {
		name, body string
		status     int
	}{
		{"trailing JSON", `{"post_ids":["` + th.BasicPost.Id + `"]} true`, http.StatusBadRequest},
		{"unknown field", `{"post_ids":["` + th.BasicPost.Id + `"],"ignored":true}`, http.StatusBadRequest},
		{"oversized body", `{"post_ids":["` + th.BasicPost.Id + `"],"padding":"` + strings.Repeat("x", 33<<10) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, requestErr := th.Client.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPost, path, tc.body, nil)
			require.Error(t, requestErr)
			require.Equal(t, tc.status, response.StatusCode)
			require.NoError(t, response.Body.Close())
			_, err := th.Store.PostAcknowledgement().Get(th.BasicPost.Id, th.BasicUser.Id)
			require.Error(t, err, "invalid request must not save an acknowledgement")
		})
	}
}
