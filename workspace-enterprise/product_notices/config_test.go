package product_notices

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	noticejobs "github.com/mattermost/mattermost/server/v8/channels/jobs/product_notices"
	"github.com/mattermost/mattermost/server/v8/config"
	"github.com/stretchr/testify/require"
)

func TestDisableOverridesExistingSettingsAndReloads(t *testing.T) {
	// A previous deployment may enable notices through either the environment
	// or a persisted config. Neither may restore an external fetch.
	t.Setenv(adminNoticesEnvironment, "true")
	t.Setenv(userNoticesEnvironment, "true")
	t.Setenv(noticesURLEnvironment, model.AnnouncementSettingsDefaultNoticesJsonURL)
	t.Setenv("mm_announcementsettings_adminnoticesenabled", "true")
	t.Setenv("mm_announcementsettings_usernoticesenabled", "true")
	t.Setenv("mm_announcementsettings_noticesurl", model.AnnouncementSettingsDefaultNoticesJsonURL)
	require.NoError(t, Disable())

	initial := &model.Config{}
	initial.SetDefaults()
	*initial.AnnouncementSettings.AdminNoticesEnabled = true
	*initial.AnnouncementSettings.UserNoticesEnabled = true
	*initial.AnnouncementSettings.NoticesURL = model.AnnouncementSettingsDefaultNoticesJsonURL

	backing, err := config.NewMemoryStoreWithOptions(&config.MemoryStoreOptions{InitialConfig: initial})
	require.NoError(t, err)
	store, err := config.NewStoreFromBacking(backing, nil, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	assertDisabled := func() {
		t.Helper()
		cfg := store.Get()
		require.False(t, *cfg.AnnouncementSettings.AdminNoticesEnabled)
		require.False(t, *cfg.AnnouncementSettings.UserNoticesEnabled)
		require.Empty(t, *cfg.AnnouncementSettings.NoticesURL)
		require.False(t, noticejobs.MakeScheduler(nil).Enabled(cfg), "no background notice job may be scheduled")
	}
	assertDisabled()

	_, _, err = store.Set(initial)
	require.NoError(t, err)
	assertDisabled()
	require.NoError(t, store.Load())
	assertDisabled()
}
