package openid_logout

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mattermost/enterprise/localization"
	"github.com/mattermost/enterprise/oauth/openid"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if err := localization.RegisterArabicLocale(); err != nil {
		panic(err)
	}
	// Match main.go: mount Enterprise routes during server initialization.
	app.RegisterComplianceInterface(func(a *app.App) einterfaces.ComplianceInterface {
		provider := openid.New()
		provider.Bind(a)
		provider.Register()
		Register(a)
		return nil
	})
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestLogoutAuthServiceUsesCurrentAccountMethod(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	th.LoginBasic(t)
	s := &Service{app: th.App}
	r := httptest.NewRequest(http.MethodPost, "/api/v4/users/logout", nil)
	r.Header.Set(model.HeaderAuth, "Bearer "+th.Client.AuthToken)

	isOpenID, known := s.logoutAuthService(r)
	require.True(t, known)
	require.False(t, isOpenID)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.OpenIdSettings.Enable = new(true)
		cfg.OpenIdSettings.DiscoveryEndpoint = new("https://identity.example/.well-known/openid-configuration")
	})
	_, enabled := s.settings()
	require.True(t, enabled)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	response := httptest.NewRecorder()
	s.injectLogoutClient(next).ServeHTTP(response, r)
	require.Equal(t, "false", response.Header().Get("X-Workspace-OpenID-Logout"))

	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE users SET authservice='openid' WHERE id=$1`, th.BasicUser.Id)
	require.NoError(t, err)
	isOpenID, known = s.logoutAuthService(r)
	require.True(t, known)
	require.True(t, isOpenID)
	response = httptest.NewRecorder()
	s.injectLogoutClient(next).ServeHTTP(response, r)
	require.Equal(t, "true", response.Header().Get("X-Workspace-OpenID-Logout"))
	th.Server.SetLicense(nil)
	response = httptest.NewRecorder()
	s.injectLogoutClient(next).ServeHTTP(response, r)
	require.Equal(t, "false", response.Header().Get("X-Workspace-OpenID-Logout"), "a previously loaded client must honor license removal")
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.OpenIdSettings.Enable = new(false) })
	response = httptest.NewRecorder()
	s.injectLogoutClient(next).ServeHTTP(response, r)
	require.Equal(t, "false", response.Header().Get("X-Workspace-OpenID-Logout"), "a previously loaded client must honor disabling OpenID")

	r.Header.Set(model.HeaderAuth, "Bearer invalid-token")
	_, known = s.logoutAuthService(r)
	require.False(t, known)
}
