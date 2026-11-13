package openid_logout

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
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

// Opt-in: the runner owns both loopback services, and Chromium starts with an
// empty profile. Neither application credentials nor a live database are used.
func TestKeycloakOpenIDLogout(t *testing.T) {
	idp := os.Getenv("WORKSPACE_TEST_KEYCLOAK_URL")
	if idp == "" {
		t.Skip("use deployment/saml/run_keycloak_test.py --openid-logout --browser --static-dir")
	}
	parsed, err := url.Parse(idp)
	require.NoError(t, err)
	ip := net.ParseIP(parsed.Hostname())
	require.True(t, parsed.Scheme == "http" && ip != nil && ip.IsLoopback() && parsed.Port() != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Path == "")
	staticDir := os.Getenv("WORKSPACE_TEST_OPENID_STATIC_DIR")
	require.True(t, filepath.IsAbs(staticDir))
	_, err = os.Stat(filepath.Join(staticDir, "root.html"))
	require.NoError(t, err)
	require.NotEmpty(t, os.Getenv("WORKSPACE_TEST_CHROME_EXECUTABLE"))

	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.EmailSettings.SendPushNotifications = new(false)
		cfg.ServiceSettings.EnableOnboardingFlow = new(false)
		cfg.ServiceSettings.EnableDesktopLandingPage = new(false)
		cfg.ServiceSettings.AllowedUntrustedInternalConnections = new("127.0.0.1 localhost")
		cfg.LocalizationSettings.DefaultClientLocale = new("ar")
		cfg.LocalizationSettings.DefaultServerLocale = new("ar")
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	service := &Service{app: th.App}

	backend, err := url.Parse(th.Client.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(backend)
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		director(r)
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-Host", r.Host)
	}
	static := http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir)))
	// The test server has no webapp. Render the isolated build through the same
	// Enterprise HTML middleware used by Register; proxy real APIs and callbacks.
	pages := service.injectLogoutClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			static.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/test/openid-probe" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<!doctype html><title>OpenID session probe</title>")
			return
		}
		http.ServeFile(w, r, filepath.Join(staticDir, "root.html"))
	}))
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/oauth/") ||
			strings.HasPrefix(r.URL.Path, "/signup/openid/") || r.URL.Path == scriptRoute {
			proxy.ServeHTTP(w, r)
			return
		}
		pages.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	origin := strings.Replace(front.URL, "127.0.0.1", "localhost", 1)

	admin := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := admin.PostForm(idp+"/realms/master/protocol/openid-connect/token", url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"},
		"username": {os.Getenv("WORKSPACE_TEST_KEYCLOAK_ADMIN")}, "password": {os.Getenv("WORKSPACE_TEST_KEYCLOAK_PASSWORD")},
	})
	require.NoError(t, err)
	var token struct {
		AccessToken string `json:"access_token"`
	}
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token))
	require.NoError(t, response.Body.Close())
	require.NotEmpty(t, token.AccessToken)
	adminRequest := func(method, route string, body any, status int) []byte {
		t.Helper()
		encoded, marshalErr := json.Marshal(body)
		require.NoError(t, marshalErr)
		req, requestErr := http.NewRequest(method, idp+route, bytes.NewReader(encoded))
		require.NoError(t, requestErr)
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		res, requestErr := admin.Do(req)
		require.NoError(t, requestErr)
		defer res.Body.Close()
		require.Equal(t, status, res.StatusCode, "isolated Keycloak administration %s", method)
		value, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		require.NoError(t, readErr)
		return value
	}
	realm, clientID, clientSecret := "workspace-logout-"+model.NewId(), "workspace-browser", model.NewId()
	userID, username, password := model.NewId(), "logout"+model.NewId(), model.NewId()+"-Test1!"
	adminRequest(http.MethodPost, "/admin/realms", map[string]any{
		"realm": realm, "enabled": true, "sslRequired": "none",
		"clients": []any{map[string]any{
			"clientId": clientID, "protocol": "openid-connect", "enabled": true,
			"publicClient": false, "secret": clientSecret, "standardFlowEnabled": true,
			"redirectUris": []string{origin + "/signup/openid/complete", origin + "/test/openid-probe"},
			"webOrigins":   []string{origin},
			"attributes":   map[string]string{"post.logout.redirect.uris": origin + "/"},
		}},
		"users": []any{map[string]any{
			"id": userID, "username": username, "enabled": true, "emailVerified": true,
			"email": username + "@example.test", "firstName": "OpenID", "lastName": "Logout",
			"credentials": []any{map[string]any{"type": "password", "value": password, "temporary": false}},
		}},
	}, http.StatusCreated)
	t.Cleanup(func() { adminRequest(http.MethodDelete, "/admin/realms/"+realm, nil, http.StatusNoContent) })
	issuer := idp + "/realms/" + realm
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ServiceSettings.SiteURL = new(origin)
		cfg.OpenIdSettings.Enable = new(true)
		cfg.OpenIdSettings.Id = new(clientID)
		cfg.OpenIdSettings.Secret = new(clientSecret)
		cfg.OpenIdSettings.Scope = new("openid profile email")
		cfg.OpenIdSettings.DiscoveryEndpoint = new(issuer + "/.well-known/openid-configuration")
		cfg.OpenIdSettings.AuthEndpoint = new("")
		cfg.OpenIdSettings.TokenEndpoint = new("")
		cfg.OpenIdSettings.UserAPIEndpoint = new("")
		cfg.OpenIdSettings.UsePreferredUsername = new(true)
		cfg.OpenIdSettings.ButtonText = new("دخول OpenID للاختبار")
	})
	team := *th.BasicTeam
	team.AllowOpenInvite = true
	team.DisplayName = "مجتمع اختبار OpenID"
	updated, appErr := th.App.UpdateTeam(&team)
	require.Nil(t, appErr)
	for _, route := range []string{scriptRoute, metadataRoute} {
		res, requestErr := admin.Get(th.Client.URL + route)
		require.NoError(t, requestErr)
		require.Equal(t, http.StatusOK, res.StatusCode, "Enterprise route must be mounted during initialization: %s", route)
		require.NoError(t, res.Body.Close())
	}

	input, err := json.Marshal(map[string]any{
		"appOrigin": origin, "idpOrigin": idp, "issuer": issuer, "clientId": clientID,
		"username": username, "password": password, "authData": userID,
		"teamName": updated.Name, "teamDisplayName": updated.DisplayName,
		"executable": os.Getenv("WORKSPACE_TEST_CHROME_EXECUTABLE"),
		"outputDir":  os.Getenv("WORKSPACE_TEST_OPENID_BROWSER_OUTPUT"),
	})
	require.NoError(t, err)
	script := os.Getenv("WORKSPACE_TEST_OPENID_BROWSER_SCRIPT")
	require.True(t, filepath.IsAbs(script))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "node", script)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	require.NoError(t, err, "OpenID Chromium verification: %s", stderr.String())
	var result struct {
		Status            string `json:"status"`
		AfterLogoutStatus int    `json:"originalSessionAfterLogoutStatus"`
		PromptNoneError   string `json:"promptNoneError"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	require.Equal(t, "passed", result.Status)
	require.Equal(t, http.StatusUnauthorized, result.AfterLogoutStatus)
	require.Equal(t, "login_required", result.PromptNoneError)
	var sessions []json.RawMessage
	require.NoError(t, json.Unmarshal(adminRequest(http.MethodGet, "/admin/realms/"+realm+"/users/"+userID+"/sessions", nil, http.StatusOK), &sessions))
	require.Empty(t, sessions, "provider administration must independently confirm that the browser SSO session was terminated")
	user, appErr := th.App.GetUserByAuth(&userID, model.ServiceOpenid)
	require.Nil(t, appErr)
	require.Equal(t, "ar", user.Locale)
	th.Server.SetLicense(nil)
	for route, status := range map[string]int{scriptRoute: http.StatusNotFound, metadataRoute: http.StatusForbidden} {
		res, requestErr := admin.Get(th.Client.URL + route)
		require.NoError(t, requestErr)
		require.Equal(t, status, res.StatusCode, "license gate: %s", route)
		require.NoError(t, res.Body.Close())
	}
	t.Log("OPENID_LOGOUT_LICENSE_GATES passed: script=404 metadata=403")
	t.Logf("CHROME_OPENID_LOGOUT_RESULT %s", strings.TrimSpace(stdout.String()))
	if output := os.Getenv("WORKSPACE_TEST_OPENID_BROWSER_OUTPUT"); output != "" {
		require.NoError(t, os.WriteFile(filepath.Join(output, "browser-verification.json"), stdout.Bytes(), 0o600))
	}
}
