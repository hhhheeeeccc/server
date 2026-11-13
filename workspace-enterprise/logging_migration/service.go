// Package logging_migration repairs the legacy /logs directory setting without
// expanding the allowed logging root or bypassing log-reader authorization.
package logging_migration

import (
	"fmt"
	"path/filepath"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/config"
)

// Prepare returns a migrated copy for the known legacy /logs setting only.
// Custom paths and explicit installations whose allowed root is /logs remain
// unchanged. No files, configuration stores or databases are written here.
func Prepare(cfg *model.Config, root string) (*model.Config, bool, error) {
	if cfg == nil || !model.SafeDereference(cfg.LogSettings.EnableFile) || cfg.LogSettings.FileLocation == nil {
		return cfg, false, nil
	}
	location := *cfg.LogSettings.FileLocation
	if filepath.Clean(location) != "/logs" {
		return cfg, false, nil
	}
	if err := config.ValidateLogFilePath(config.GetLogFileLocation(location), root); err == nil {
		return cfg, false, nil
	}
	destination, err := filepath.Abs(root)
	if err != nil {
		return nil, false, err
	}
	updated := cfg.Clone()
	updated.LogSettings.FileLocation = &destination
	if err = config.ValidateLogPaths(updated, root); err != nil {
		return nil, false, fmt.Errorf("cannot migrate legacy log directory safely: %w", err)
	}
	return updated, true, nil
}

// Register applies the narrow config migration during Enterprise startup. It
// uses the normal save path, which preserves environment overrides, read-only
// configuration and logging-path validation. It does not grant permissions.
func Register(a *app.App) {
	updated, changed, err := Prepare(a.Config(), config.GetLogRootPath())
	if err != nil {
		a.Log().Error("Unable to migrate legacy log directory", mlog.Err(err))
		return
	}
	if !changed {
		return
	}
	if _, _, appErr := a.SaveConfig(updated, false); appErr != nil {
		a.Log().Error("Unable to persist migrated log directory", mlog.Err(appErr))
		return
	}
	// An environment override may keep the effective value at /logs even after
	// saving the underlying configuration. Report this instead of claiming success.
	if err := config.ValidateLogFilePath(config.GetLogFileLocation(model.SafeDereference(a.Config().LogSettings.FileLocation)), config.GetLogRootPath()); err != nil {
		a.Log().Error("Log directory remains outside the allowed root; review MM_LOGSETTINGS_FILELOCATION", mlog.Err(err))
		return
	}
	a.Log().Info("Migrated legacy /logs setting to the allowed logging directory", mlog.String("path", model.SafeDereference(a.Config().LogSettings.FileLocation)))
}
