package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestDistributionDefaults(t *testing.T) {
	var data bytes.Buffer
	require.NoError(t, writeDefaults(&data))
	var cfg model.Config
	require.NoError(t, json.Unmarshal(data.Bytes(), &cfg))
	require.Equal(t, "Workspace", *cfg.TeamSettings.SiteName)
	require.Empty(t, *cfg.SupportSettings.TermsOfServiceLink)
	require.Empty(t, *cfg.SupportSettings.HelpLink)
	require.Empty(t, *cfg.SupportSettings.PrivacyPolicyLink)
	require.Empty(t, *cfg.SupportSettings.AboutLink)
	require.Equal(t, model.SupportSettingsReportAProblemTypeHidden, *cfg.SupportSettings.ReportAProblemType)
	require.Empty(t, *cfg.NativeAppSettings.AppDownloadLink)
	require.Empty(t, *cfg.AnnouncementSettings.NoticesURL)
	require.False(t, *cfg.PluginSettings.AutomaticPrepackagedPlugins)
	for _, state := range cfg.PluginSettings.PluginStates {
		require.False(t, state.Enable)
	}
}

func TestExistingConfigurationIsPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "config.json")
	require.NoError(t, run(path))
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.ErrorIs(t, run(path), os.ErrExist)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
