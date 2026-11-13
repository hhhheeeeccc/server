package preferences_test

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

func TestLegacyVisibleDMPreferenceRequiresLicenseAndSavesBatch(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	ctx := context.Background()
	userID := th.BasicUser.Id
	legacy := model.Preference{
		UserId: userID, Category: model.PreferenceCategorySidebarSettings,
		Name: model.PreferenceLimitVisibleDmsGms, Value: "0",
	}
	other := model.Preference{UserId: userID, Category: "workspace_test", Name: "marker", Value: "saved"}
	response, err := th.Client.UpdatePreferences(ctx, userID, model.Preferences{legacy, other})
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	_, _, err = th.Client.GetPreferenceByCategoryAndName(ctx, userID, legacy.Category, legacy.Name)
	require.Error(t, err)

	th.App.Srv().SetLicense(model.NewTestLicense())
	invalid := other
	invalid.Category = strings.Repeat("x", 33)
	response, err = th.Client.UpdatePreferences(ctx, userID, model.Preferences{legacy, invalid})
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	_, _, err = th.Client.GetPreferenceByCategoryAndName(ctx, userID, legacy.Category, legacy.Name)
	require.Error(t, err, "a rejected batch must not save its valid first preference")

	response, err = th.Client.UpdatePreferences(ctx, userID, model.Preferences{legacy, other})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	for _, want := range []model.Preference{legacy, other} {
		stored, _, getErr := th.Client.GetPreferenceByCategoryAndName(ctx, userID, want.Category, want.Name)
		require.NoError(t, getErr)
		require.Equal(t, want.Value, stored.Value)
	}

	legacy.Value = "10000"
	response, err = th.Client.UpdatePreferences(ctx, userID, model.Preferences{legacy})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	stored, _, err := th.Client.GetPreferenceByCategoryAndName(ctx, userID, legacy.Category, legacy.Name)
	require.NoError(t, err)
	require.Equal(t, "10000", stored.Value)
}

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}
