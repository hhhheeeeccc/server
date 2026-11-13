package product_notices

import (
	"fmt"
	"os"
	"strings"
)

const (
	adminNoticesEnvironment = "MM_ANNOUNCEMENTSETTINGS_ADMINNOTICESENABLED"
	userNoticesEnvironment  = "MM_ANNOUNCEMENTSETTINGS_USERNOTICESENABLED"
	noticesURLEnvironment   = "MM_ANNOUNCEMENTSETTINGS_NOTICESURL"
)

// Disable prevents product-notice fetches before the configuration store and
// background workers start. Environment overrides also cover existing configs
// and later reloads without changing the core or requiring an Enterprise license.
func Disable() error {
	for _, setting := range []struct{ key, value string }{
		{adminNoticesEnvironment, "false"},
		{userNoticesEnvironment, "false"},
		{noticesURLEnvironment, ""},
	} {
		// The configuration loader normalizes environment names to uppercase.
		// Remove alternate spellings so they cannot override this policy later.
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			if key != setting.key && strings.EqualFold(key, setting.key) {
				if err := os.Unsetenv(key); err != nil {
					return fmt.Errorf("disable product notices: %w", err)
				}
			}
		}
		if err := os.Setenv(setting.key, setting.value); err != nil {
			return fmt.Errorf("disable product notices: %w", err)
		}
	}
	return nil
}
