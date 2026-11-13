package localization_test

import (
	"context"
	"testing"

	_ "github.com/mattermost/enterprise"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestUserLocalePatchPersistsAcrossReads(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.LocalizationSettings.DefaultClientLocale = model.NewPointer("ar")
		cfg.LocalizationSettings.DefaultServerLocale = model.NewPointer("ar")
		cfg.LocalizationSettings.AvailableLocales = model.NewPointer("ar,en")
	}).InitBasic(t)
	ctx := context.Background()
	userID := th.BasicUser.Id
	initial, _, err := th.Client.GetMe(ctx, "")
	require.NoError(t, err)

	for _, locale := range []string{"en", "ar"} {
		updated, _, patchErr := th.Client.PatchUser(ctx, "me", &model.UserPatch{Locale: &locale})
		require.NoError(t, patchErr)
		require.Equal(t, locale, updated.Locale)
		stored, _, getErr := th.SystemAdminClient.GetUser(ctx, userID, "")
		require.NoError(t, getErr)
		require.Equal(t, locale, stored.Locale)
		require.Equal(t, initial.Username, stored.Username)
		require.Equal(t, initial.Email, stored.Email)
	}
	adminLocale := "en"
	_, _, err = th.SystemAdminClient.PatchUser(ctx, userID, &model.UserPatch{Locale: &adminLocale})
	require.NoError(t, err)
	stored, _, err := th.Client.GetMe(ctx, "")
	require.NoError(t, err)
	require.Equal(t, adminLocale, stored.Locale)
}

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}
