package openid

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

const validDiscoveryDocument = `{"authorization_endpoint":"https://idp.example/auth","token_endpoint":"https://idp.example/token","userinfo_endpoint":"https://idp.example/userinfo"}`

type testEmailProvisioner struct {
	called bool
}

func (p *testEmailProvisioner) PrepareOpenIDUser(_ context.Context, user *model.User) error {
	p.called = true
	user.Email = "no-email-openid-test@users.invalid"
	user.DisableWelcomeEmail = true
	return nil
}

func TestDiscoveryCachesConcurrentRequests(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, validDiscoveryDocument)
	}))
	defer server.Close()

	provider := &Provider{}
	var waitGroup sync.WaitGroup
	for range 20 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, err := provider.getDiscovery(nil, server.URL, discoveryAll)
			if err != nil {
				t.Errorf("getDiscovery returned an error: %v", err)
			}
		}()
	}
	waitGroup.Wait()
	require.EqualValues(t, 1, calls.Load())
}

func TestDiscoveryRejectsUnsafeAndIncompleteDocuments(t *testing.T) {
	for _, document := range []string{
		`{}`,
		`{"authorization_endpoint":"javascript:alert(1)","token_endpoint":"https://idp.example/token","userinfo_endpoint":"https://idp.example/userinfo"}`,
		`{"authorization_endpoint":"https://idp.example/auth"}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, document)
		}))
		provider := &Provider{}
		_, err := provider.getDiscovery(nil, server.URL, discoveryAll)
		require.Error(t, err)
		server.Close()
	}
}

func TestGetSSOSettingsKeepsManualEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"authorization_endpoint":"https://idp.example/auth","token_endpoint":"https://idp.example/token"}`)
	}))
	defer server.Close()

	config := &model.Config{}
	config.SetDefaults()
	config.OpenIdSettings.Enable = model.NewPointer(true)
	config.OpenIdSettings.Id = model.NewPointer("  client-id  ")
	config.OpenIdSettings.Secret = model.NewPointer("  client-secret  ")
	config.OpenIdSettings.DiscoveryEndpoint = model.NewPointer(server.URL)
	config.OpenIdSettings.UserAPIEndpoint = model.NewPointer("https://idp.example/manual-userinfo")
	provider := &Provider{licenseCheck: func() bool { return true }}
	settings, err := provider.GetSSOSettings(nil, config, model.ServiceOpenid)
	require.NoError(t, err)
	require.Equal(t, "https://idp.example/auth", *settings.AuthEndpoint)
	require.Equal(t, "https://idp.example/token", *settings.TokenEndpoint)
	require.Equal(t, "https://idp.example/manual-userinfo", *settings.UserAPIEndpoint)
	require.Equal(t, "client-id", *settings.Id)
	require.Equal(t, "client-secret", *settings.Secret)
	require.Equal(t, "  client-id  ", *config.OpenIdSettings.Id)
	require.Equal(t, "  client-secret  ", *config.OpenIdSettings.Secret)
}

func TestGetSSOSettingsRequiresLicense(t *testing.T) {
	config := &model.Config{}
	config.SetDefaults()
	_, err := (&Provider{}).GetSSOSettings(nil, config, model.ServiceOpenid)
	require.ErrorContains(t, err, "active licensed feature")
}

func TestGetUserFromJSON(t *testing.T) {
	provider := &Provider{}
	settings := &model.SSOSettings{UsePreferredUsername: model.NewPointer(true)}
	rctx := request.TestContext(t)
	user, err := provider.GetUserFromJSON(rctx, strings.NewReader(`{
		"sub":"subject-1",
		"email":"USER@EXAMPLE.COM",
		"preferred_username":"openid-user",
		"name":"Open ID User"
	}`), nil, settings)
	require.NoError(t, err)
	require.Equal(t, "openid-user", user.Username)
	require.Equal(t, "user@example.com", user.Email)
	require.Equal(t, "Open", user.FirstName)
	require.Equal(t, "ID User", user.LastName)
	require.Equal(t, model.ServiceOpenid, user.AuthService)
	require.Equal(t, "subject-1", *user.AuthData)
}

func TestGetUserFromJSONPreparesUserWithoutEmail(t *testing.T) {
	provisioner := &testEmailProvisioner{}
	provider := &Provider{emailProvisioner: provisioner}
	settings := &model.SSOSettings{UsePreferredUsername: model.NewPointer(true)}
	rctx := request.TestContext(t)
	user, err := provider.GetUserFromJSON(rctx, strings.NewReader(`{
		"sub":"subject-without-email",
		"preferred_username":"openid-no-email",
		"name":"OpenID User"
	}`), nil, settings)
	require.NoError(t, err)
	require.True(t, provisioner.called)
	require.Equal(t, "no-email-openid-test@users.invalid", user.Email)
	require.True(t, user.DisableWelcomeEmail)
	require.Equal(t, "subject-without-email", *user.AuthData)
}

func TestGetUserFromJSONTreatsInvalidEmailAsMissing(t *testing.T) {
	provisioner := &testEmailProvisioner{}
	provider := &Provider{emailProvisioner: provisioner}
	settings := &model.SSOSettings{UsePreferredUsername: model.NewPointer(true)}
	rctx := request.TestContext(t)
	user, err := provider.GetUserFromJSON(rctx, strings.NewReader(`{
		"sub":"subject-with-invalid-email",
		"email":"not-an-email",
		"preferred_username":"openid-invalid-email"
	}`), nil, settings)
	require.NoError(t, err)
	require.True(t, provisioner.called)
	require.Equal(t, "no-email-openid-test@users.invalid", user.Email)
	require.True(t, user.DisableWelcomeEmail)
}

func TestIsSameUserUsesOpenIDSubject(t *testing.T) {
	provider := &Provider{}
	newUser := func(service, subject string) *model.User {
		return &model.User{AuthService: service, AuthData: model.NewPointer(subject)}
	}
	require.True(t, provider.IsSameUser(nil, newUser(model.ServiceOpenid, "subject-1"), newUser(model.ServiceOpenid, "subject-1")))
	require.False(t, provider.IsSameUser(nil, newUser(model.ServiceOpenid, "subject-1"), newUser(model.ServiceOpenid, "subject-2")))
	require.False(t, provider.IsSameUser(nil, newUser(model.ServiceGitlab, "subject-1"), newUser(model.ServiceOpenid, "subject-1")))
}
