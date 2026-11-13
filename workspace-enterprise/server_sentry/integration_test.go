package server_sentry

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// The API test helper applies its test license after NewServer returns.
	// Force the licensed branch here to exercise the production initialization
	// order; license gating itself is covered by TestConfigure.
	app.RegisterComplianceInterface(func(*app.App) einterfaces.ComplianceInterface {
		_ = Configure(true)
		return nil
	})
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestHTTPPanicReachesConfiguredCollector(t *testing.T) {
	type capturedRequest struct {
		path string
		body string
	}
	requests := make(chan capturedRequest, 2)
	panicked := make(chan struct{}, 1)
	collector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case requests <- capturedRequest{path: r.URL.Path, body: string(body)}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	certificate := filepath.Join(t.TempDir(), "collector.pem")
	require.NoError(t, os.WriteFile(certificate,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: collector.Certificate().Raw}), 0o600))
	t.Setenv("SSL_CERT_FILE", certificate)
	t.Setenv("MM_SERVICEENVIRONMENT", model.ServiceEnvironmentTest)
	t.Setenv(dsnEnvironmentVariable, "https://public-key@"+strings.TrimPrefix(collector.URL, "https://")+"/42")
	previousDSN := app.SentryDSN
	t.Cleanup(func() {
		app.SentryDSN = previousDSN
		sentry.CurrentHub().BindClient(nil)
	})

	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		*cfg.LogSettings.EnableDiagnostics = true
		*cfg.LogSettings.EnableSentry = true
	})
	require.Contains(t, app.SentryDSN, collector.Listener.Addr().String())
	th.Server.RootRouter.HandleFunc("/workspace-sentry-test-panic", func(http.ResponseWriter, *http.Request) {
		panicked <- struct{}{}
		panic("workspace sentry integration test")
	}).Methods(http.MethodPost)
	client := &http.Client{Timeout: 5 * time.Second}
	response, _ := client.Post(th.Client.URL+"/workspace-sentry-test-panic", "application/octet-stream", nil)
	if response != nil {
		_ = response.Body.Close()
	}
	select {
	case <-panicked:
	case <-time.After(time.Second):
		t.Fatal("the HTTP request did not reach the panic handler")
	}
	require.True(t, sentry.Flush(5*time.Second))
	select {
	case request := <-requests:
		require.Contains(t, request.path, "/api/42/")
		require.Contains(t, request.body, "workspace sentry integration test")
	case <-time.After(time.Second):
		t.Fatal("HTTP panic did not reach the configured HTTPS collector")
	}
}
