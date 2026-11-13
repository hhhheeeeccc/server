package plugin_ids

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

// Workspace identifiers also used by the optional build overlay. The server's
// source files remain unchanged.
const (
	Apps          = "com.workspace.apps"
	Calls         = "com.workspace.calls"
	NPS           = "com.workspace.nps"
	ChannelExport = "com.workspace.plugin-channel-export"
	AI            = "workspace-ai"
)

// Keep legacy IDs literal: with the overlay, model constants contain Workspace
// IDs and must not be used to identify legacy configuration.
var legacyToWorkspace = map[string]string{
	"com.mattermost.apps":                  Apps,
	"com.mattermost.calls":                 Calls,
	"com.mattermost.nps":                   NPS,
	"com.mattermost.plugin-channel-export": ChannelExport,
	"mattermost-ai":                        AI,
}

// Register migrates configuration only for explicitly configured Workspace
// plugins. Branding migration must never install or enable a plugin.
func Register(a *app.App) {
	a.UpdateConfig(func(cfg *model.Config) {
		MigratePluginSettings(&cfg.PluginSettings)
	})
	cfg := a.Config().Clone()
	changed, err := disableUninstalledDefaults(&cfg.PluginSettings)
	if err != nil {
		a.Log().Warn("Unable to inspect installed plugins; keeping their configured states", mlog.Err(err))
		return
	}
	if !changed {
		return
	}
	if !a.IsConfigReadOnly() {
		if _, _, appErr := a.SaveConfig(cfg, true); appErr == nil {
			return
		} else {
			a.Log().Warn("Unable to persist disabled defaults for uninstalled plugins", mlog.Err(appErr))
		}
	}
	a.UpdateConfig(func(current *model.Config) {
		for id, state := range cfg.PluginSettings.PluginStates {
			if state != nil && !state.Enable {
				if _, known := automaticPluginDefaults[id]; known {
					current.PluginSettings.PluginStates[id] = &model.PluginState{Enable: false}
				}
			}
		}
	})
}

var automaticPluginDefaults = map[string]struct{}{
	model.PluginIdCalls: {}, model.PluginIdNPS: {}, model.PluginIdPlaybooks: {}, model.PluginIdAI: {},
}

// When automatic bundled plugins are explicitly disabled, a core default state
// must not opt an absent plugin back in. Inspect manifests, not directory names.
// An unreadable inventory leaves the administrator's current choices intact.
func disableUninstalledDefaults(settings *model.PluginSettings) (bool, error) {
	if settings.AutomaticPrepackagedPlugins == nil || *settings.AutomaticPrepackagedPlugins || settings.Directory == nil {
		return false, nil
	}
	entries, err := os.ReadDir(*settings.Directory)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	installed := make(map[string]bool)
	for _, entry := range entries {
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(*settings.Directory, entry.Name(), "plugin.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		var manifest model.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return false, err
		}
		installed[manifest.Id] = true
	}
	if settings.PluginStates == nil {
		settings.PluginStates = make(map[string]*model.PluginState)
	}
	changed := false
	for id := range automaticPluginDefaults {
		if installed[id] {
			continue
		}
		if state := settings.PluginStates[id]; state == nil || state.Enable {
			settings.PluginStates[id] = &model.PluginState{Enable: false}
			changed = true
		}
	}
	return changed, nil
}

// MigratePluginSettings copies legacy options only when the Workspace plugin
// already has an explicit state. Core's default states do not prove installation.
func MigratePluginSettings(settings *model.PluginSettings) {
	if settings.PluginStates == nil {
		settings.PluginStates = make(map[string]*model.PluginState)
	}
	if settings.Plugins == nil {
		settings.Plugins = make(map[string]map[string]any)
	}

	for legacyID, workspaceID := range legacyToWorkspace {
		if state := settings.PluginStates[workspaceID]; state == nil {
			continue
		}

		if _, exists := settings.Plugins[workspaceID]; !exists {
			if pluginConfig, found := settings.Plugins[legacyID]; found {
				configCopy := make(map[string]any, len(pluginConfig))
				for key, value := range pluginConfig {
					configCopy[key] = value
				}
				settings.Plugins[workspaceID] = configCopy
			}
		}
	}
}

// DisableAutomaticPluginDefaults records explicit opt-outs in NEW distribution
// configurations. Deleting these keys would let core re-enable them on load.
func DisableAutomaticPluginDefaults(settings *model.PluginSettings) {
	if settings.PluginStates == nil {
		settings.PluginStates = make(map[string]*model.PluginState)
	}
	for _, id := range []string{model.PluginIdCalls, model.PluginIdNPS, model.PluginIdPlaybooks, model.PluginIdAI} {
		settings.PluginStates[id] = &model.PluginState{Enable: false}
	}
	settings.AutomaticPrepackagedPlugins = new(false)
}
