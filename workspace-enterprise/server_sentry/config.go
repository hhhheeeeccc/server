package server_sentry

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/getsentry/sentry-go"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

const dsnEnvironmentVariable = "WORKSPACE_SERVER_SENTRY_DSN"

type licenseCheck struct{ allowed func() bool }

var currentLicenseCheck atomic.Pointer[licenseCheck]
var installLicenseCheck sync.Once

// SetLicenseCheck drops server events after the Enterprise license is removed
// without requiring a restart. The checker is read for every captured event.
// Passing nil resets the check in tests; production always binds an App.
func SetLicenseCheck(check func() bool) {
	installLicenseCheck.Do(func() {
		sentry.AddGlobalEventProcessor(func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			if current := currentLicenseCheck.Load(); current != nil && !current.allowed() {
				return nil
			}
			return event
		})
	})
	if check == nil {
		currentLicenseCheck.Store(nil)
	} else {
		currentLicenseCheck.Store(&licenseCheck{allowed: check})
	}
}

// DisableDefault prevents the upstream DSN and sentry-go's SENTRY_DSN fallback
// from bypassing the Enterprise license and explicit DSN configuration.
func DisableDefault() {
	app.SentryDSN = ""
	_ = os.Unsetenv("SENTRY_DSN")
}

// Configure installs the Enterprise DSN before Mattermost initializes its
// built-in Go error reporter. An empty DSN keeps that reporter disconnected.
func Configure(licensed bool) error {
	DisableDefault()
	if !licensed {
		return nil
	}
	dsn := strings.TrimSpace(os.Getenv(dsnEnvironmentVariable))
	if err := validateDSN(dsn); err != nil {
		return fmt.Errorf("%s: %w", dsnEnvironmentVariable, err)
	}
	app.SentryDSN = dsn
	return nil
}

func validateDSN(dsn string) error {
	if dsn == "" {
		return nil
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User == nil || parsed.User.Username() == "" || parsed.Path == "" {
		return fmt.Errorf("invalid HTTPS Sentry DSN")
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		return fmt.Errorf("Sentry DSN must not contain a password")
	}
	return nil
}
