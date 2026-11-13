package site_links

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestMigrateAfterConfigurationReload(t *testing.T) {
	cfg := &model.Config{}
	cfg.SetDefaults()
	require.True(t, Migrate(cfg))
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	var reloaded model.Config
	require.NoError(t, json.Unmarshal(data, &reloaded))
	reloaded.SetDefaults()
	// Core restores the upstream terms URL for an empty value on every load.
	require.Equal(t, model.SupportSettingsDefaultTermsOfServiceLink, *reloaded.SupportSettings.TermsOfServiceLink)
	require.True(t, Migrate(&reloaded))
	require.Empty(t, *reloaded.SupportSettings.TermsOfServiceLink)
	require.Empty(t, *reloaded.SupportSettings.HelpLink)
	require.Empty(t, *reloaded.SupportSettings.PrivacyPolicyLink)
}

func TestMigrateUpstreamDefaults(t *testing.T) {
	cfg := &model.Config{}
	cfg.SetDefaults()
	marketplace := *cfg.PluginSettings.MarketplaceURL
	cws := *cfg.CloudSettings.CWSURL

	require.True(t, Migrate(cfg))
	require.Empty(t, *cfg.SupportSettings.HelpLink)
	require.Empty(t, *cfg.SupportSettings.PrivacyPolicyLink)
	require.Empty(t, *cfg.SupportSettings.AboutLink)
	require.Empty(t, *cfg.SupportSettings.TermsOfServiceLink)
	require.Equal(t, model.SupportSettingsReportAProblemTypeHidden, *cfg.SupportSettings.ReportAProblemType)
	require.Empty(t, *cfg.SupportSettings.ReportAProblemLink)
	require.Empty(t, *cfg.NativeAppSettings.AppDownloadLink)
	require.Empty(t, *cfg.NativeAppSettings.AndroidAppDownloadLink)
	require.Empty(t, *cfg.NativeAppSettings.IosAppDownloadLink)
	require.Empty(t, *cfg.AnnouncementSettings.NoticesURL)
	require.False(t, *cfg.AnnouncementSettings.AdminNoticesEnabled)
	require.False(t, *cfg.AnnouncementSettings.UserNoticesEnabled)
	require.Equal(t, marketplace, *cfg.PluginSettings.MarketplaceURL)
	require.Equal(t, cws, *cfg.CloudSettings.CWSURL)
	require.False(t, Migrate(cfg))
}

func TestMigratePreservesCustomLinks(t *testing.T) {
	cfg := &model.Config{}
	cfg.SetDefaults()
	*cfg.SupportSettings.TermsOfServiceLink = "https://example.test/terms"
	*cfg.SupportSettings.HelpLink = "https://example.test/help"
	*cfg.SupportSettings.PrivacyPolicyLink = "https://example.test/privacy"
	*cfg.SupportSettings.AboutLink = "https://example.test/about"
	*cfg.SupportSettings.ReportAProblemType = model.SupportSettingsReportAProblemTypeLink
	*cfg.SupportSettings.ReportAProblemLink = "https://example.test/support"
	*cfg.NativeAppSettings.AppDownloadLink = "https://example.test/apps"
	*cfg.NativeAppSettings.AndroidAppDownloadLink = "https://example.test/android"
	*cfg.NativeAppSettings.IosAppDownloadLink = "https://example.test/ios"
	*cfg.AnnouncementSettings.NoticesURL = "https://example.test/notices.json"

	require.False(t, Migrate(cfg))
	require.Equal(t, "https://example.test/terms", *cfg.SupportSettings.TermsOfServiceLink)
	require.Equal(t, "https://example.test/help", *cfg.SupportSettings.HelpLink)
	require.Equal(t, "https://example.test/privacy", *cfg.SupportSettings.PrivacyPolicyLink)
	require.Equal(t, "https://example.test/about", *cfg.SupportSettings.AboutLink)
	require.Equal(t, "https://example.test/support", *cfg.SupportSettings.ReportAProblemLink)
	require.Equal(t, "https://example.test/apps", *cfg.NativeAppSettings.AppDownloadLink)
	require.Equal(t, "https://example.test/android", *cfg.NativeAppSettings.AndroidAppDownloadLink)
	require.Equal(t, "https://example.test/ios", *cfg.NativeAppSettings.IosAppDownloadLink)
	require.Equal(t, "https://example.test/notices.json", *cfg.AnnouncementSettings.NoticesURL)
	require.True(t, *cfg.AnnouncementSettings.AdminNoticesEnabled)
	require.True(t, *cfg.AnnouncementSettings.UserNoticesEnabled)
}
