package outgoing_oauth_connections

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

// Opt-in coverage against the disposable provider owned by deployment/saml's
// --outgoing-oauth runner. It never accepts a remote provider or live database.
func TestKeycloakOutgoingOAuth(t *testing.T) {
	origin := os.Getenv("WORKSPACE_TEST_KEYCLOAK_URL")
	if origin == "" {
		t.Skip("use deployment/saml/run_keycloak_test.py --outgoing-oauth")
	}
	parsed, err := url.Parse(origin)
	require.NoError(t, err)
	ip := net.ParseIP(parsed.Hostname())
	require.True(t, parsed.Scheme == "http" && ip != nil && ip.IsLoopback() && parsed.Port() != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Path == "", "only a literal loopback Keycloak origin is allowed")
	provider := &oauthKeycloak{origin: origin, client: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	provider.authenticate(t)
	realm := "workspace-oauth-" + model.NewId()
	clientID, clientSecret := "workspace-connector", model.NewId()
	userID, username, password := model.NewId(), "oauth"+model.NewId(), model.NewId()+"-Test1!"
	provider.request(t, http.MethodPost, "/admin/realms", map[string]any{
		"realm": realm, "enabled": true, "sslRequired": "none",
		"clients": []any{map[string]any{
			"id": clientID, "clientId": clientID, "protocol": "openid-connect", "enabled": true,
			"publicClient": false, "secret": clientSecret, "serviceAccountsEnabled": true,
			"directAccessGrantsEnabled": true, "standardFlowEnabled": false,
		}},
		"users": []any{map[string]any{
			"id": userID, "username": username, "enabled": true, "emailVerified": true,
			"email": username + "@example.test", "firstName": "OAuth", "lastName": "Test",
			"credentials": []any{map[string]any{"type": "password", "value": password, "temporary": false}},
		}},
	}, http.StatusCreated)
	t.Cleanup(func() { provider.request(t, http.MethodDelete, "/admin/realms/"+realm, nil, http.StatusNoContent) })
	issuer := origin + "/realms/" + realm
	tokenURL := issuer + "/protocol/openid-connect/token"
	var currentSecret atomic.Value
	currentSecret.Store(clientSecret)
	var accepted atomic.Int32
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The resource accepts only a token independently confirmed active by the
		// real provider, not merely any string with a Bearer prefix.
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		claims, ok := provider.introspect(issuer, clientID, currentSecret.Load().(string), header[len("Bearer "):])
		if !ok || !claims.Active || claims.ClientID != clientID || claims.Subject == "" ||
			(r.URL.Path == "/password" && claims.Subject != userID) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		accepted.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&model.CommandResponse{Text: "keycloak-protected-resource-ok", ResponseType: model.CommandResponseTypeEphemeral})
	}))
	t.Cleanup(resource.Close)

	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableCommands = new(true)
		cfg.ServiceSettings.EnableOutgoingOAuthConnections = new(true)
		cfg.ServiceSettings.AllowedUntrustedInternalConnections = new("127.0.0.1")
	}).InitBasic(t)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ServiceSettings.SiteURL = new(th.Client.URL) })
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterprise, "outgoing_oauth_connections")))
	ctx := context.Background()
	service := th.App.OutgoingOAuthConnections().(*Service)
	for _, grant := range []model.OutgoingOAuthConnectionGrantType{
		model.OutgoingOAuthConnectionGrantTypeClientCredentials, model.OutgoingOAuthConnectionGrantTypePassword,
	} {
		t.Run(string(grant), func(t *testing.T) {
			endpoint := resource.URL + "/" + string(grant)
			input := &model.OutgoingOAuthConnection{Name: "Keycloak " + string(grant), ClientId: clientID,
				ClientSecret: currentSecret.Load().(string), OAuthTokenURL: tokenURL, GrantType: grant,
				Audiences: model.StringArray{endpoint}}
			if grant == model.OutgoingOAuthConnectionGrantTypePassword {
				input.CredentialsUsername, input.CredentialsPassword = &username, &password
			}
			_, response, err := th.Client.CreateOutgoingOAuthConnection(ctx, input)
			require.Error(t, err)
			require.Equal(t, http.StatusForbidden, response.StatusCode)
			created, response, err := th.SystemAdminClient.CreateOutgoingOAuthConnection(ctx, input)
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, response.StatusCode)
			require.Empty(t, created.ClientSecret)
			require.Nil(t, created.CredentialsPassword)
			command, _, err := th.SystemAdminClient.CreateCommand(ctx, &model.Command{
				TeamId: th.BasicTeam.Id, URL: endpoint, Method: model.CommandMethodPost, Trigger: "oauth" + string(grant),
			})
			require.NoError(t, err)
			execute := func() {
				t.Helper()
				before := accepted.Load()
				result, response, err := th.Client.ExecuteCommandWithTeam(ctx, th.BasicChannel.Id, th.BasicTeam.Id, "/"+command.Trigger)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.StatusCode)
				require.Equal(t, "keycloak-protected-resource-ok", result.Text)
				require.Equal(t, before+1, accepted.Load())
			}
			stored := func() *model.OutgoingOAuthConnection {
				t.Helper()
				connection, appErr := service.GetConnectionForAudience(th.Context, endpoint)
				require.Nil(t, appErr)
				require.NotNil(t, connection)
				require.Equal(t, created.Id, connection.Id)
				return connection
			}
			validate := func(connection *model.OutgoingOAuthConnection, expected int) {
				t.Helper()
				response, err := th.SystemAdminClient.DoAPIPostJSON(ctx, "/oauth/outgoing_connections/validate", connection)
				if expected == http.StatusOK {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				require.Equal(t, expected, response.StatusCode)
				require.NoError(t, response.Body.Close())
			}
			validate(stored(), http.StatusOK)
			execute()

			if grant == model.OutgoingOAuthConnectionGrantTypeClientCredentials {
				rotated := model.NewId()
				provider.request(t, http.MethodPut, "/admin/realms/"+realm+"/clients/"+clientID,
					map[string]any{"secret": rotated}, http.StatusNoContent)
				currentSecret.Store(rotated)
				validate(stored(), http.StatusBadRequest)
				updated, _, err := th.SystemAdminClient.UpdateOutgoingOAuthConnection(ctx,
					&model.OutgoingOAuthConnection{Id: created.Id, ClientSecret: rotated})
				require.NoError(t, err)
				require.Empty(t, updated.ClientSecret)
				validate(stored(), http.StatusOK)
				execute()
			} else {
				bad := *stored()
				bad.CredentialsPassword = new("incorrect-test-password")
				validate(&bad, http.StatusBadRequest)
				provider.request(t, http.MethodPut, "/admin/realms/"+realm+"/users/"+userID,
					map[string]any{"enabled": false}, http.StatusNoContent)
				validate(stored(), http.StatusBadRequest)
			}
			_, err = th.SystemAdminClient.DeleteOutgoingOAuthConnection(ctx, created.Id)
			require.NoError(t, err)
			_, appErr := service.GetConnection(th.Context, created.Id)
			require.NotNil(t, appErr)
			require.Equal(t, http.StatusNotFound, appErr.StatusCode)
		})
	}
	require.EqualValues(t, 3, accepted.Load(), "the protected resource must accept all three successful commands")
	th.Server.SetLicense(nil)
	_, appErr := service.RetrieveTokenForConnection(th.Context, &model.OutgoingOAuthConnection{
		ClientId: clientID, ClientSecret: currentSecret.Load().(string), OAuthTokenURL: tokenURL,
		GrantType: model.OutgoingOAuthConnectionGrantTypeClientCredentials,
	})
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusForbidden, appErr.StatusCode)
	t.Log("KEYCLOAK_OUTGOING_OAUTH verified both grants, protected slash commands, secret rotation, invalid credentials, disabled provider account and license removal")
}

type oauthKeycloak struct {
	origin string
	token  string
	client *http.Client
}

func (k *oauthKeycloak) authenticate(t *testing.T) {
	t.Helper()
	username, password := os.Getenv("WORKSPACE_TEST_KEYCLOAK_ADMIN"), os.Getenv("WORKSPACE_TEST_KEYCLOAK_PASSWORD")
	require.NotEmpty(t, username)
	require.NotEmpty(t, password)
	response, err := k.client.PostForm(k.origin+"/realms/master/protocol/openid-connect/token", url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {username}, "password": {password},
	})
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var token struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token))
	require.NotEmpty(t, token.AccessToken)
	k.token = token.AccessToken
}

func (k *oauthKeycloak) request(t *testing.T, method, path string, body any, status int) {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(method, k.origin+path, bytes.NewReader(encoded))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+k.token)
	response, err := k.client.Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, status, response.StatusCode, "Keycloak administrative operation %s", method)
}

type oauthKeycloakClaims struct {
	Active   bool   `json:"active"`
	ClientID string `json:"client_id"`
	Subject  string `json:"sub"`
}

func (k *oauthKeycloak) introspect(issuer, clientID, secret, token string) (oauthKeycloakClaims, bool) {
	response, err := k.client.PostForm(issuer+"/protocol/openid-connect/token/introspect", url.Values{
		"client_id": {clientID}, "client_secret": {secret}, "token": {token},
	})
	if err != nil {
		return oauthKeycloakClaims{}, false
	}
	defer response.Body.Close()
	var claims oauthKeycloakClaims
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&claims) != nil {
		return oauthKeycloakClaims{}, false
	}
	return claims, true
}
