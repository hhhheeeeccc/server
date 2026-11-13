package site_links

import (
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

// Register replaces only uncustomized upstream links. Existing administrator
// choices remain intact, and this licensed feature does not change core defaults.
func Register(a *app.App) {
	if a.License() == nil {
		return
	}
	preview := a.Config().Clone()
	if !Migrate(preview) {
		return
	}
	a.UpdateConfig(func(cfg *model.Config) { Migrate(cfg) })
}

// Migrate removes upstream links that have no Workspace destination yet.
func Migrate(cfg *model.Config) bool {
	changed := false
	changeDefault := func(target *string, oldValue, newValue string) {
		if target != nil && *target == oldValue {
			*target = newValue
			changed = true
		}
	}

	changeDefault(cfg.SupportSettings.TermsOfServiceLink, model.SupportSettingsDefaultTermsOfServiceLink, "")
	changeDefault(cfg.SupportSettings.HelpLink, model.SupportSettingsDefaultHelpLink, "")
	changeDefault(cfg.SupportSettings.PrivacyPolicyLink, model.SupportSettingsDefaultPrivacyPolicyLink, "")
	changeDefault(cfg.SupportSettings.AboutLink, model.SupportSettingsDefaultAboutLink, "")
	if cfg.SupportSettings.ReportAProblemType != nil &&
		*cfg.SupportSettings.ReportAProblemType == model.SupportSettingsReportAProblemTypeDefault &&
		cfg.SupportSettings.ReportAProblemLink != nil &&
		*cfg.SupportSettings.ReportAProblemLink == model.SupportSettingsDefaultReportAProblemLink {
		*cfg.SupportSettings.ReportAProblemType = model.SupportSettingsReportAProblemTypeHidden
		*cfg.SupportSettings.ReportAProblemLink = ""
		changed = true
	}
	changeDefault(cfg.NativeAppSettings.AppDownloadLink, model.NativeappSettingsDefaultAppDownloadLink, "")
	changeDefault(cfg.NativeAppSettings.AndroidAppDownloadLink, model.NativeappSettingsDefaultAndroidAppDownloadLink, "")
	changeDefault(cfg.NativeAppSettings.IosAppDownloadLink, model.NativeappSettingsDefaultIosAppDownloadLink, "")
	if cfg.AnnouncementSettings.NoticesURL != nil &&
		*cfg.AnnouncementSettings.NoticesURL == model.AnnouncementSettingsDefaultNoticesJsonURL {
		*cfg.AnnouncementSettings.NoticesURL = ""
		if cfg.AnnouncementSettings.AdminNoticesEnabled != nil {
			*cfg.AnnouncementSettings.AdminNoticesEnabled = false
		}
		if cfg.AnnouncementSettings.UserNoticesEnabled != nil {
			*cfg.AnnouncementSettings.UserNoticesEnabled = false
		}
		changed = true
	}
	return changed
}
