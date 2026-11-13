package plugin_ids

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost/server/public/model"
)

func TestMigratePluginSettings(t *testing.T) {
	t.Run("does not recreate deleted Workspace aliases from core defaults", func(t *testing.T) {
		settings := model.PluginSettings{
			PluginStates: map[string]*model.PluginState{
				"com.mattermost.calls": {Enable: true},
			},
			Plugins: map[string]map[string]any{
				"com.mattermost.calls": {"rtcdserviceurl": "http://calls:8045"},
			},
		}

		MigratePluginSettings(&settings)

		require.NotContains(t, settings.PluginStates, Calls)
		require.NotContains(t, settings.Plugins, Calls)
		require.True(t, settings.PluginStates["com.mattermost.calls"].Enable)
	})

	t.Run("copies options for an explicitly configured Workspace plugin", func(t *testing.T) {
		settings := model.PluginSettings{
			PluginStates: map[string]*model.PluginState{Calls: {Enable: false}},
			Plugins:      map[string]map[string]any{"com.mattermost.calls": {"region": "legacy"}},
		}
		MigratePluginSettings(&settings)
		require.False(t, settings.PluginStates[Calls].Enable)
		require.Equal(t, "legacy", settings.Plugins[Calls]["region"])
		settings.Plugins[Calls]["region"] = "workspace"
		require.Equal(t, "legacy", settings.Plugins["com.mattermost.calls"]["region"])
	})

	t.Run("preserves Workspace-specific values", func(t *testing.T) {
		settings := model.PluginSettings{
			PluginStates: map[string]*model.PluginState{
				"com.mattermost.calls": {Enable: true},
				Calls:                  {Enable: false},
			},
			Plugins: map[string]map[string]any{
				"com.mattermost.calls": {"region": "legacy"},
				Calls:                  {"region": "workspace"},
			},
		}

		MigratePluginSettings(&settings)

		require.False(t, settings.PluginStates[Calls].Enable)
		require.Equal(t, "workspace", settings.Plugins[Calls]["region"])
	})

	t.Run("initializes empty maps", func(t *testing.T) {
		settings := model.PluginSettings{}

		MigratePluginSettings(&settings)

		require.NotNil(t, settings.PluginStates)
		require.NotNil(t, settings.Plugins)
	})
}

func TestStartupDisablesAbsentCoreDefaults(t *testing.T) {
	cfg := &model.Config{}
	cfg.SetDefaults()
	cfg.PluginSettings.AutomaticPrepackagedPlugins = new(false)
	cfg.PluginSettings.Directory = new(t.TempDir())
	// Core regenerates these states as true when their keys were deleted.
	changed, err := disableUninstalledDefaults(&cfg.PluginSettings)
	require.NoError(t, err)
	require.True(t, changed)
	for id := range automaticPluginDefaults {
		require.False(t, cfg.PluginSettings.PluginStates[id].Enable)
	}
	cfg.SetDefaults()
	changed, err = disableUninstalledDefaults(&cfg.PluginSettings)
	require.NoError(t, err)
	require.False(t, changed)
}

func TestStartupPreservesInstalledPluginState(t *testing.T) {
	directory := t.TempDir()
	pluginDirectory := filepath.Join(directory, "different-directory-name")
	require.NoError(t, os.Mkdir(pluginDirectory, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(pluginDirectory, "plugin.json"), []byte(`{"id":"`+model.PluginIdCalls+`"}`), 0600))
	cfg := &model.Config{}
	cfg.SetDefaults()
	cfg.PluginSettings.AutomaticPrepackagedPlugins = new(false)
	cfg.PluginSettings.Directory = &directory
	_, err := disableUninstalledDefaults(&cfg.PluginSettings)
	require.NoError(t, err)
	require.True(t, cfg.PluginSettings.PluginStates[model.PluginIdCalls].Enable)
}

func TestStartupPreservesAutomaticInstallOptIn(t *testing.T) {
	cfg := &model.Config{}
	cfg.SetDefaults()
	cfg.PluginSettings.Directory = new(t.TempDir())
	changed, err := disableUninstalledDefaults(&cfg.PluginSettings)
	require.NoError(t, err)
	require.False(t, changed)
	require.True(t, cfg.PluginSettings.PluginStates[model.PluginIdCalls].Enable)
}

func TestDisabledDistributionPluginsStayDisabledAfterDefaults(t *testing.T) {
	cfg := &model.Config{}
	cfg.SetDefaults()
	DisableAutomaticPluginDefaults(&cfg.PluginSettings)
	cfg.SetDefaults()
	MigratePluginSettings(&cfg.PluginSettings)
	for _, id := range []string{model.PluginIdCalls, model.PluginIdNPS, model.PluginIdPlaybooks, model.PluginIdAI} {
		require.False(t, cfg.PluginSettings.PluginStates[id].Enable)
	}
	require.False(t, *cfg.PluginSettings.AutomaticPrepackagedPlugins)
	for legacy, workspace := range legacyToWorkspace {
		if _, defaultPlugin := automaticPluginDefaults[workspace]; !defaultPlugin {
			require.NotContains(t, cfg.PluginSettings.PluginStates, workspace, legacy)
		}
	}
}

func TestWorkspaceOverlayDefaults(t *testing.T) {
	if model.PluginIdCalls != Calls {
		t.Skip("requires the Workspace build overlay")
	}
	cfg := &model.Config{}
	cfg.SetDefaults()
	for _, id := range []string{Calls, NPS, "workspace", AI} {
		require.Contains(t, cfg.PluginSettings.PluginStates, id)
	}
	for _, id := range []string{"com.mattermost.calls", "com.mattermost.nps", "playbooks", "mattermost-ai"} {
		require.NotContains(t, cfg.PluginSettings.PluginStates, id)
	}
	require.Equal(t, Apps, model.PluginIdApps)
	require.Equal(t, "com.mattermost.plugin-channel-export", model.PluginIdChannelExport)
	// Legacy settings still migrate even when model constants use Workspace IDs.
	cfg.PluginSettings.Plugins["com.mattermost.calls"] = map[string]any{"region": "legacy"}
	MigratePluginSettings(&cfg.PluginSettings)
	require.Equal(t, "legacy", cfg.PluginSettings.Plugins[Calls]["region"])
}
