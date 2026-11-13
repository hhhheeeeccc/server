//go:build integration

package auth_verify

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/mattermost/enterprise/compliance_service"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	app.RegisterComplianceInterface(func(a *app.App) einterfaces.ComplianceInterface {
		Register(a)
		return compliance_service.New(a)
	})
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestVerifyAuth(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	token := th.Client.AuthToken

	check := func(t *testing.T, cookie, bearer string, wantStatus int) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, th.Client.URL+endpoint, nil)
		require.NoError(t, err)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: model.SessionCookieToken, Value: cookie})
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.Header.Set("If-None-Match", "*")

		response, err := th.Client.HTTPClient.Do(req)
		require.NoError(t, err)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.Equal(t, wantStatus, response.StatusCode, string(body))
		require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
		require.Empty(t, response.Header.Get("Location"))
		if wantStatus == http.StatusNoContent {
			require.Empty(t, body)
		}
	}

	for _, testCase := range []struct {
		name   string
		cookie string
		bearer string
		status int
	}{
		{name: "anonymous", status: http.StatusUnauthorized},
		{name: "invalid cookie", cookie: model.NewId(), status: http.StatusUnauthorized},
		{name: "invalid bearer", bearer: model.NewId(), status: http.StatusUnauthorized},
		{name: "valid browser session", cookie: token, status: http.StatusNoContent},
		{name: "valid bearer session", bearer: token, status: http.StatusNoContent},
		{name: "invalid cookie takes precedence over valid bearer", cookie: model.NewId(), bearer: token, status: http.StatusUnauthorized},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			check(t, testCase.cookie, testCase.bearer, testCase.status)
		})
	}

	t.Run("session token in URL is rejected", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, th.Client.URL+endpoint+"?access_token="+token, nil)
		require.NoError(t, err)
		response, err := th.Client.HTTPClient.Do(req)
		require.NoError(t, err)
		defer response.Body.Close()
		require.Equal(t, http.StatusUnauthorized, response.StatusCode)
		require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
		require.Empty(t, response.Header.Get("Location"))
	})

	t.Run("enforced MFA rejects a session without MFA", func(t *testing.T) {
		th.App.UpdateConfig(func(cfg *model.Config) {
			cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
			cfg.ServiceSettings.EnforceMultifactorAuthentication = new(true)
		})
		check(t, token, "", http.StatusForbidden)
		th.App.UpdateConfig(func(cfg *model.Config) {
			cfg.ServiceSettings.EnforceMultifactorAuthentication = new(false)
		})
		check(t, token, "", http.StatusNoContent)
	})

	t.Run("license required", func(t *testing.T) {
		th.Server.SetLicense(nil)
		check(t, token, "", http.StatusForbidden)
		require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	})

	t.Run("logged out session", func(t *testing.T) {
		_, err := th.Client.Logout(context.Background())
		require.NoError(t, err)
		check(t, token, "", http.StatusUnauthorized)
		check(t, "", token, http.StatusUnauthorized)
	})
}
