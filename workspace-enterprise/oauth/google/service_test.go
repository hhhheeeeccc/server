package google

import (
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

const googleProfile = `{"resourceName":"people/unstable","metadata":{"sources":[{"type":"CONTACT","id":"contact"},{"type":"PROFILE","id":"123456"}]},"names":[{"givenName":"First","familyName":"Last","metadata":{"primary":true}}],"emailAddresses":[{"value":"other@example.test","metadata":{"verified":true}},{"value":"User@Example.test","metadata":{"primary":true,"verified":true}}]}`

func TestGoogleProfileUsesVerifiedPrimaryEmailAndStableID(t *testing.T) {
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	p := New(func() *model.License { return license })
	rctx := request.TestContext(t)
	u, err := p.GetUserFromJSON(rctx, strings.NewReader(googleProfile), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "123456", *u.AuthData)
	require.Equal(t, "user@example.test", u.Email)
	require.Equal(t, "user", u.Username)
	require.Equal(t, "First", u.FirstName)
	require.True(t, u.EmailVerified)
	reserved := strings.Replace(googleProfile, "User@Example.test", "login@example.test", 1)
	a, err := p.GetUserFromJSON(rctx, strings.NewReader(reserved), nil, nil)
	require.NoError(t, err)
	b, err := p.GetUserFromJSON(rctx, strings.NewReader(reserved), nil, nil)
	require.NoError(t, err)
	require.Equal(t, a.Username, b.Username)
	require.True(t, model.IsValidUsername(a.Username))
	copy := u.DeepCopy()
	copy.Email = "changed@example.test"
	require.True(t, p.IsSameUser(rctx, u, copy))
	copy.AuthData = new("different")
	require.False(t, p.IsSameUser(rctx, u, copy))
	for _, data := range []string{
		strings.ReplaceAll(googleProfile, `"verified":true`, `"verified":false`),
		strings.Replace(googleProfile, `"type":"PROFILE"`, `"type":"CONTACT"`, 1),
		`null`, `{}`, googleProfile + `{}`, `{"error":{"message":"denied"}}`,
	} {
		_, err = p.GetUserFromJSON(rctx, strings.NewReader(data), nil, nil)
		require.Error(t, err)
	}
	license = nil
	_, err = p.GetUserFromJSON(rctx, strings.NewReader(googleProfile), nil, nil)
	require.Error(t, err)
}

func TestGoogleSettingsGatesFeatureAndValidatesEndpoints(t *testing.T) {
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	p := New(func() *model.License { return license })
	cfg := &model.Config{}
	cfg.SetDefaults()
	cfg.GoogleSettings.Enable, cfg.GoogleSettings.Id, cfg.GoogleSettings.Secret = new(true), new("client"), new("secret")
	settings, err := p.GetSSOSettings(request.TestContext(t), cfg, model.ServiceGoogle)
	require.NoError(t, err)
	require.Equal(t, model.GoogleSettingsDefaultUserAPIEndpoint, *settings.UserAPIEndpoint)
	cfg.GoogleSettings.TokenEndpoint = new("http://example.test/token")
	_, err = p.GetSSOSettings(request.TestContext(t), cfg, model.ServiceGoogle)
	require.Error(t, err)
	license.Features.GoogleOAuth = new(false)
	_, err = p.GetUserFromIdToken(request.TestContext(t), "untrusted.jwt")
	require.Error(t, err)
}
