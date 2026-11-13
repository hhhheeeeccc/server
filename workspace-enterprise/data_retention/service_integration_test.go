package data_retention

import (
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	app.RegisterDataRetentionInterface(New)
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestRetentionPolicyAPIRequiresFeatureAndPermission(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.DataRetention = new(false)
	require.True(t, th.Server.SetLicense(license))
	_, response, err := th.SystemAdminClient.GetDataRetentionPolicy(th.Context.Context())
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	license.Features.DataRetention = new(true)
	require.True(t, th.Server.SetLicense(license))
	policy, response, err := th.SystemAdminClient.GetDataRetentionPolicy(th.Context.Context())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NotNil(t, policy)
	policies, response, err := th.SystemAdminClient.GetDataRetentionPolicies(th.Context.Context(), 0, 10)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NotNil(t, policies)
	days := int64(30)
	created, response, err := th.SystemAdminClient.CreateDataRetentionPolicy(th.Context.Context(), &model.RetentionPolicyWithTeamAndChannelIDs{
		RetentionPolicy: model.RetentionPolicy{DisplayName: "Enterprise API storage test", PostDurationDays: &days},
		ChannelIDs:      []string{th.BasicChannel.Id},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	require.Equal(t, int64(1), created.ChannelCount)
	stored, response, err := th.SystemAdminClient.GetDataRetentionPolicyByID(th.Context.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, created.ID, stored.ID)
	require.Equal(t, days, *stored.PostDurationDays)
	missingID := model.NewId()
	_, response, err = th.SystemAdminClient.GetDataRetentionPolicyByID(th.Context.Context(), missingID)
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	_, response, err = th.SystemAdminClient.PatchDataRetentionPolicy(th.Context.Context(),
		&model.RetentionPolicyWithTeamAndChannelIDs{RetentionPolicy: model.RetentionPolicy{ID: missingID, DisplayName: "Missing"}})
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	response, err = th.SystemAdminClient.DeleteDataRetentionPolicy(th.Context.Context(), missingID)
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	updated, response, err := th.SystemAdminClient.PatchDataRetentionPolicy(th.Context.Context(),
		&model.RetentionPolicyWithTeamAndChannelIDs{RetentionPolicy: model.RetentionPolicy{ID: created.ID, DisplayName: "Updated policy"}})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "Updated policy", updated.DisplayName)
	response, err = th.SystemAdminClient.DeleteDataRetentionPolicy(th.Context.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_, response, err = th.SystemAdminClient.GetDataRetentionPolicyByID(th.Context.Context(), created.ID)
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	_, response, err = th.Client.GetDataRetentionPolicies(th.Context.Context(), 0, 10)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	_, response, err = th.Client.CreateDataRetentionPolicy(th.Context.Context(), &model.RetentionPolicyWithTeamAndChannelIDs{
		RetentionPolicy: model.RetentionPolicy{DisplayName: "Unauthorized", PostDurationDays: &days},
		ChannelIDs:      []string{th.BasicChannel.Id},
	})
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
}
