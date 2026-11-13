package outgoing_oauth_connections

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	app.RegisterOutgoingOAuthConnectionInterface(New)
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestOutgoingOAuthConnectionAPIAndToken(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableOutgoingOAuthConnections = new(true)
		cfg.ServiceSettings.AllowedUntrustedInternalConnections = new("127.0.0.1")
	}).InitBasic(t)
	ctx := context.Background()
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterprise, "outgoing_oauth_connections")
	require.True(t, th.Server.SetLicense(license))

	var expectedSecret atomic.Value
	expectedSecret.Store("client-secret")
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, r.ParseForm())
		require.Equal(t, "client_credentials", r.Form.Get("grant_type"))
		require.Equal(t, "client-id", r.Form.Get("client_id"))
		require.Equal(t, expectedSecret.Load(), r.Form.Get("client_secret"))
		require.NoError(t, json.NewEncoder(w).Encode(map[string]string{"access_token": "issued-token", "token_type": "Bearer"}))
	}))
	defer tokenServer.Close()

	input := &model.OutgoingOAuthConnection{
		Name:          "Workspace token connection",
		ClientId:      "client-id",
		ClientSecret:  "client-secret",
		OAuthTokenURL: tokenServer.URL,
		GrantType:     model.OutgoingOAuthConnectionGrantTypeClientCredentials,
		Audiences:     model.StringArray{"https://example.com/hooks/*"},
	}
	_, response, err := th.Client.CreateOutgoingOAuthConnection(ctx, input)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	created, response, err := th.SystemAdminClient.CreateOutgoingOAuthConnection(ctx, input)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	require.True(t, model.IsValidId(created.Id))
	require.Empty(t, created.ClientSecret)

	stored, err := th.Store.OutgoingOAuthConnection().GetConnection(th.Context, created.Id)
	require.NoError(t, err)
	require.Equal(t, input.ClientSecret, stored.ClientSecret)
	service := th.App.OutgoingOAuthConnections().(*Service)
	matched, appErr := service.GetConnectionForAudience(th.Context, "https://example.com/hooks/one")
	require.Nil(t, appErr)
	require.Equal(t, created.Id, matched.Id)
	missing, appErr := service.GetConnectionForAudience(th.Context, "https://example.com.evil/hooks/one")
	require.Nil(t, appErr)
	require.Nil(t, missing)
	missing, appErr = service.GetConnectionForAudience(th.Context, "https://example.com/hooks/%2e%2e/admin")
	require.Nil(t, appErr)
	require.Nil(t, missing)
	token, appErr := service.RetrieveTokenForConnection(th.Context, matched)
	require.Nil(t, appErr)
	require.Equal(t, "Bearer issued-token", token.AsHeaderValue())
	expectedSecret.Store("rotated-secret")
	updated, _, err := th.SystemAdminClient.UpdateOutgoingOAuthConnection(ctx, &model.OutgoingOAuthConnection{
		Id:           created.Id,
		ClientSecret: "rotated-secret",
	})
	require.NoError(t, err)
	require.Empty(t, updated.ClientSecret)
	matched, appErr = service.GetConnectionForAudience(th.Context, "https://example.com/hooks/one")
	require.Nil(t, appErr)
	require.Equal(t, "rotated-secret", matched.ClientSecret)
	token, appErr = service.RetrieveTokenForConnection(th.Context, matched)
	require.Nil(t, appErr)
	require.Equal(t, "Bearer issued-token", token.AsHeaderValue())

	listed, _, err := th.SystemAdminClient.GetOutgoingOAuthConnections(ctx, model.OutgoingOAuthConnectionGetConnectionsFilter{})
	require.NoError(t, err)
	require.NotEmpty(t, listed)
	for _, connection := range listed {
		require.Empty(t, connection.ClientSecret)
	}

	_, err = th.SystemAdminClient.DeleteOutgoingOAuthConnection(ctx, created.Id)
	require.NoError(t, err)
	_, err = th.Store.OutgoingOAuthConnection().GetConnection(th.Context, created.Id)
	require.Error(t, err)

	th.Server.SetLicense(nil)
	_, response, err = th.SystemAdminClient.CreateOutgoingOAuthConnection(ctx, input)
	require.Error(t, err)
	require.NotEqual(t, http.StatusCreated, response.StatusCode)
}

func TestOutgoingOAuthPasswordGrantPersistsCredentials(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableOutgoingOAuthConnections = new(true)
		cfg.ServiceSettings.AllowedUntrustedInternalConnections = new("127.0.0.1")
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterprise, "outgoing_oauth_connections")))

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		require.Equal(t, "password", r.Form.Get("grant_type"))
		require.Equal(t, "service-user", r.Form.Get("username"))
		require.Equal(t, "service-password", r.Form.Get("password"))
		require.NoError(t, json.NewEncoder(w).Encode(map[string]string{"access_token": "password-token", "token_type": "Bearer"}))
	}))
	defer tokenServer.Close()

	input := &model.OutgoingOAuthConnection{
		Name:                "Workspace password connection",
		ClientId:            "client-id",
		ClientSecret:        "client-secret",
		OAuthTokenURL:       tokenServer.URL,
		GrantType:           model.OutgoingOAuthConnectionGrantTypePassword,
		CredentialsUsername: new("service-user"),
		CredentialsPassword: new("service-password"),
		Audiences:           model.StringArray{"https://example.com/password"},
	}
	created, _, err := th.SystemAdminClient.CreateOutgoingOAuthConnection(context.Background(), input)
	require.NoError(t, err)
	require.Nil(t, created.CredentialsPassword)
	stored, err := th.Store.OutgoingOAuthConnection().GetConnection(th.Context, created.Id)
	require.NoError(t, err)
	require.Equal(t, "service-user", model.SafeDereference(stored.CredentialsUsername))
	require.Equal(t, "service-password", model.SafeDereference(stored.CredentialsPassword))

	token, appErr := th.App.OutgoingOAuthConnections().(*Service).RetrieveTokenForConnection(th.Context, stored)
	require.Nil(t, appErr)
	require.Equal(t, "Bearer password-token", token.AsHeaderValue())
}

func TestOutgoingOAuthValidationDistinguishesRejectionFromProviderFailure(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableOutgoingOAuthConnections = new(true)
		cfg.ServiceSettings.AllowedUntrustedInternalConnections = new("127.0.0.1")
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterprise, "outgoing_oauth_connections")))

	var providerStatus atomic.Int64
	providerStatus.Store(http.StatusUnauthorized)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(providerStatus.Load()))
		if providerStatus.Load() == http.StatusOK {
			_, _ = w.Write([]byte("not JSON"))
		}
	}))
	defer provider.Close()
	connection := &model.OutgoingOAuthConnection{
		ClientId: "client-id", ClientSecret: "client-secret", OAuthTokenURL: provider.URL,
		GrantType: model.OutgoingOAuthConnectionGrantTypeClientCredentials,
	}
	validate := func(expected int) {
		t.Helper()
		response, err := th.SystemAdminClient.DoAPIPostJSON(context.Background(), "/oauth/outgoing_connections/validate", connection)
		require.Error(t, err)
		require.Equal(t, expected, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}

	validate(http.StatusBadRequest)
	providerStatus.Store(http.StatusServiceUnavailable)
	validate(http.StatusBadGateway)
	providerStatus.Store(http.StatusOK)
	validate(http.StatusBadGateway)
	provider.Close()
	validate(http.StatusBadGateway)
}

func TestOutgoingOAuthTransportErrorDoesNotExposeTokenURLQuery(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableOutgoingOAuthConnections = new(true)
		cfg.ServiceSettings.AllowedUntrustedInternalConnections = new("127.0.0.1")
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterprise, "outgoing_oauth_connections")))
	const secret = "oauth-query-secret-marker"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://other.example/token", http.StatusTemporaryRedirect)
	}))
	defer provider.Close()
	connection := &model.OutgoingOAuthConnection{
		ClientId: "client-id", ClientSecret: secret,
		OAuthTokenURL: provider.URL + "/token?credential=" + secret,
		GrantType:     model.OutgoingOAuthConnectionGrantTypeClientCredentials,
	}
	_, appErr := th.App.OutgoingOAuthConnections().(*Service).RetrieveTokenForConnection(th.Context, connection)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusBadGateway, appErr.StatusCode)
	require.NotContains(t, appErr.Error(), secret)
	encoded, err := json.Marshal(appErr)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), secret)
}
