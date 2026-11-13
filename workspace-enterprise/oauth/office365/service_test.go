package office365

import (
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

const graphProfile = `{"id":"immutable-object-id","mail":"User@Example.test","userPrincipalName":"worklogin@example.test","givenName":"First","surname":"Last"}`

func TestMicrosoftProfileUsesObjectIDAndPreferredUsername(t *testing.T) {
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	p := New(func() *model.License { return license })
	rctx := request.TestContext(t)
	u, err := p.GetUserFromJSON(rctx, strings.NewReader(graphProfile), nil, &model.SSOSettings{UsePreferredUsername: new(true)})
	require.NoError(t, err)
	require.Equal(t, "immutable-object-id", *u.AuthData)
	require.Equal(t, "user@example.test", u.Email)
	require.Equal(t, "worklogin", u.Username)
	copy := u.DeepCopy()
	copy.Email = "changed@example.test"
	require.True(t, p.IsSameUser(rctx, u, copy))
	copy.AuthService = model.ServiceGoogle
	require.False(t, p.IsSameUser(rctx, u, copy))
	u, err = p.GetUserFromJSON(rctx, strings.NewReader(strings.Replace(graphProfile, `"User@Example.test"`, `null`, 1)), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "worklogin@example.test", u.Email)
	for _, data := range []string{`{}`, `null`, graphProfile + `{}`, `{"id":"a","mail":"not-email"}`} {
		_, err = p.GetUserFromJSON(rctx, strings.NewReader(data), nil, nil)
		require.Error(t, err)
	}
	license = nil
	_, err = p.GetUserFromJSON(rctx, strings.NewReader(graphProfile), nil, nil)
	require.Error(t, err)
}

func TestMicrosoftDirectoryEndpointsDoNotMutateConfiguration(t *testing.T) {
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	p := New(func() *model.License { return license })
	cfg := &model.Config{}
	cfg.SetDefaults()
	cfg.Office365Settings.Enable, cfg.Office365Settings.Id, cfg.Office365Settings.Secret = new(true), new("client"), new("secret")
	cfg.Office365Settings.DirectoryId = new("tenant.example.test")
	settings, err := p.GetSSOSettings(request.TestContext(t), cfg, model.ServiceOffice365)
	require.NoError(t, err)
	require.Contains(t, *settings.AuthEndpoint, "/tenant.example.test/")
	require.Contains(t, *settings.TokenEndpoint, "/tenant.example.test/")
	require.Contains(t, *cfg.Office365Settings.AuthEndpoint, "/common/")
	originalAuth, originalToken := cfg.Office365Settings.AuthEndpoint, cfg.Office365Settings.TokenEndpoint
	uppercaseAuth := strings.Replace(*originalAuth, "login.microsoftonline.com", "LOGIN.MICROSOFTONLINE.COM.", 1)
	uppercaseToken := strings.Replace(*originalToken, "login.microsoftonline.com", "LOGIN.MICROSOFTONLINE.COM.", 1)
	cfg.Office365Settings.AuthEndpoint, cfg.Office365Settings.TokenEndpoint = &uppercaseAuth, &uppercaseToken
	settings, err = p.GetSSOSettings(request.TestContext(t), cfg, model.ServiceOffice365)
	require.NoError(t, err)
	require.Contains(t, *settings.AuthEndpoint, "/tenant.example.test/")
	require.Contains(t, *settings.TokenEndpoint, "/tenant.example.test/")
	cfg.Office365Settings.AuthEndpoint, cfg.Office365Settings.TokenEndpoint = originalAuth, originalToken
	mismatchedAuth := strings.Replace(*originalAuth, "/common/", "/other-tenant/", 1)
	cfg.Office365Settings.AuthEndpoint = &mismatchedAuth
	_, err = p.GetSSOSettings(request.TestContext(t), cfg, model.ServiceOffice365)
	require.Error(t, err)
	cfg.Office365Settings.AuthEndpoint = originalAuth
	for _, directory := range []string{"../common", "tenant?x=y", "test@evil", "bad%2ftenant"} {
		cfg.Office365Settings.DirectoryId = &directory
		_, err = p.GetSSOSettings(request.TestContext(t), cfg, model.ServiceOffice365)
		require.Error(t, err)
	}
	license.Features.Office365OAuth = new(false)
	_, err = p.GetUserFromIdToken(request.TestContext(t), "unverified.token")
	require.Error(t, err)
}
