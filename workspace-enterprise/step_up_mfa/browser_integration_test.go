//go:build integration

package step_up_mfa

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

// This fixture serves the complete compiled webapp. Every API operation and
// session is handled by a real core server backed by disposable PostgreSQL.
func TestBrowserStepUpMFA(t *testing.T) {
	staticDir := os.Getenv("WORKSPACE_TEST_STEP_UP_STATIC_DIR")
	if staticDir == "" {
		t.Skip("run deployment/step_up_mfa/run_test.py with an isolated webapp build")
	}
	require.True(t, filepath.IsAbs(staticDir))
	_, err := os.Stat(filepath.Join(staticDir, "root.html"))
	require.NoError(t, err)
	t.Setenv("WORKSPACE_STEP_UP_MFA_GRACE_MINUTES", "5")
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
		cfg.ServiceSettings.EnableOnboardingFlow = new(false)
		cfg.ServiceSettings.EnableDesktopLandingPage = new(false)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.EmailSettings.SendPushNotifications = new(false)
		cfg.LocalizationSettings.DefaultClientLocale = new("ar")
		cfg.LocalizationSettings.DefaultServerLocale = new("ar")
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	admin, appErr := th.App.GetUser(th.Context, th.SystemAdminUser.Id)
	require.Nil(t, appErr)
	admin.Locale = "ar"
	_, appErr = th.App.UpdateUser(th.Context, admin, false)
	require.Nil(t, appErr)
	secondClient := model.NewAPIv4Client(th.Client.URL)
	_, _, err = secondClient.Login(th.Context.Context(), th.SystemAdminUser.Email, th.SystemAdminUser.Password)
	require.NoError(t, err)
	secret := enableAdminMFA(t, th)
	backend, err := url.Parse(th.Client.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(backend)
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		director(r)
		r.Header.Set("X-Forwarded-Proto", "https")
	}
	static := http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir)))
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/"):
			proxy.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/static/"):
			static.ServeHTTP(w, r)
		default:
			http.ServeFile(w, r, filepath.Join(staticDir, "root.html"))
		}
	}))
	t.Cleanup(front.Close)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ServiceSettings.SiteURL = new(front.URL) })
	var sessions []map[string]string
	for _, token := range []string{th.SystemAdminClient.AuthToken, secondClient.AuthToken} {
		session, sessionErr := th.App.GetSession(token)
		require.Nil(t, sessionErr)
		sessions = append(sessions, map[string]string{"token": token, "csrf": session.GetCSRF()})
	}
	input, err := json.Marshal(map[string]any{
		"appOrigin": front.URL, "adminId": admin.Id, "secret": secret,
		"sessions": sessions, "targets": []string{th.BasicUser.Id, th.BasicUser2.Id, th.TeamAdminUser.Id},
		"executable": os.Getenv("WORKSPACE_TEST_CHROME_EXECUTABLE"),
		"outputDir":  os.Getenv("WORKSPACE_TEST_STEP_UP_OUTPUT"),
	})
	require.NoError(t, err)
	script := os.Getenv("WORKSPACE_TEST_STEP_UP_BROWSER_SCRIPT")
	require.True(t, filepath.IsAbs(script))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "node", script)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	require.NoError(t, err, "Chromium step-up verification: %s", stderr.String())
	var result struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	require.Equal(t, "passed", result.Status)
	for i, id := range []string{th.BasicUser.Id, th.BasicUser2.Id, th.TeamAdminUser.Id} {
		user, userErr := th.App.GetUser(th.Context, id)
		require.Nil(t, userErr)
		if i < 2 {
			require.NotZero(t, user.DeleteAt)
		} else {
			require.Zero(t, user.DeleteAt)
		}
	}
	first, appErr := th.App.GetSession(th.SystemAdminClient.AuthToken)
	require.Nil(t, appErr)
	require.NotEmpty(t, first.Props[verifiedAtProp])
	second, appErr := th.App.GetSession(secondClient.AuthToken)
	require.Nil(t, appErr)
	require.Empty(t, second.Props[verifiedAtProp], "grace must belong to the verified session only")
	service := &Service{app: th.App, db: th.Store.GetInternalMasterDB()}
	policy, err := service.readPolicy(context.Background())
	require.NoError(t, err)
	require.Equal(t, 7, policy.GraceMinutes, "the browser must persist the policy through the actual main save control")
	t.Logf("CHROME_STEP_UP_RESULT %s", strings.TrimSpace(stdout.String()))
	if output := os.Getenv("WORKSPACE_TEST_STEP_UP_OUTPUT"); output != "" {
		require.NoError(t, os.WriteFile(filepath.Join(output, "browser-verification.json"), stdout.Bytes(), 0o600))
	}
}
