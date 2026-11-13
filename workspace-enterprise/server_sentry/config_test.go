package server_sentry

import (
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/stretchr/testify/require"
)

func TestConfigure(t *testing.T) {
	previous := app.SentryDSN
	t.Cleanup(func() { app.SentryDSN = previous })
	t.Setenv("SENTRY_DSN", "https://fallback@example.test/99")

	for _, test := range []struct {
		name     string
		dsn      string
		licensed bool
		want     string
		wantErr  bool
	}{
		{name: "disabled by default"},
		{name: "unlicensed", dsn: "https://public-key@example.test/42"},
		{name: "valid DSN", dsn: "https://public-key@example.test/42", licensed: true, want: "https://public-key@example.test/42"},
		{name: "insecure DSN", dsn: "http://public-key@example.test/42", licensed: true, wantErr: true},
		{name: "password in DSN", dsn: "https://public-key:secret@example.test/42", licensed: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(dsnEnvironmentVariable, test.dsn)
			err := Configure(test.licensed)
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, test.want, app.SentryDSN)
			require.Empty(t, os.Getenv("SENTRY_DSN"))
		})
	}
}

func TestConfiguredDSNSendsToLocalHTTPSCollector(t *testing.T) {
	var licensed atomic.Bool
	licensed.Store(true)
	SetLicenseCheck(licensed.Load)
	t.Cleanup(func() { SetLicenseCheck(nil) })
	requests := make(chan string, 1)
	collector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	certificate := filepath.Join(t.TempDir(), "collector.pem")
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: collector.Certificate().Raw})
	require.NoError(t, os.WriteFile(certificate, encoded, 0o600))
	t.Setenv("SSL_CERT_FILE", certificate)
	t.Setenv(dsnEnvironmentVariable, "https://public-key@"+strings.TrimPrefix(collector.URL, "https://")+"/42")
	previous := app.SentryDSN
	t.Cleanup(func() {
		app.SentryDSN = previous
		sentry.CurrentHub().BindClient(nil)
	})
	require.NoError(t, Configure(true))
	require.NoError(t, sentry.Init(sentry.ClientOptions{Dsn: app.SentryDSN}))
	sentry.CaptureException(errors.New("local sentry delivery test"))
	require.True(t, sentry.Flush(5*time.Second))
	select {
	case path := <-requests:
		require.Contains(t, path, "/api/42/")
	case <-time.After(time.Second):
		t.Fatal("the configured HTTPS collector did not receive the event")
	}
	licensed.Store(false)
	sentry.CaptureException(errors.New("event after license removal"))
	require.True(t, sentry.Flush(time.Second))
	select {
	case <-requests:
		t.Fatal("event was sent after the license was removed")
	default:
	}
	licensed.Store(true)
	sentry.CaptureException(errors.New("event after license restoration"))
	require.True(t, sentry.Flush(5*time.Second))
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("event was not sent after the license was restored")
	}
	t.Setenv("SENTRY_DSN", app.SentryDSN)
	require.NoError(t, Configure(false))
	require.Empty(t, app.SentryDSN)
	require.NoError(t, sentry.Init(sentry.ClientOptions{Dsn: app.SentryDSN}))
	sentry.CaptureException(errors.New("unlicensed event must not be sent"))
	require.True(t, sentry.Flush(time.Second))
	select {
	case <-requests:
		t.Fatal("unlicensed configuration sent an event")
	default:
	}
}
