package saml

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestNewSAMLUsersInheritDefaultClientLocale(t *testing.T) {
	for _, tc := range []struct {
		name, mapping, value, want string
		guest                      bool
	}{
		{name: "member_without_mapping", want: "ar"},
		{name: "guest_without_mapping", guest: true, want: "ar"},
		{name: "provider_locale_takes_precedence", mapping: "language", value: "en", want: "en"},
		{name: "empty_provider_locale_uses_default", mapping: "language", want: "ar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupSAML(t)
			f.th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.LocalizationSettings.DefaultClientLocale = new("ar")
				cfg.SamlSettings.LocaleAttribute = new(tc.mapping)
				if tc.guest {
					cfg.GuestAccountsSettings.Enable = new(true)
					cfg.SamlSettings.GuestAttribute = new("member_type=guest")
				}
			})
			require.Equal(t, "ar", *f.th.App.Config().LocalizationSettings.DefaultClientLocale, "the fixture must retain its configured default before testing SAML")
			values := map[string]string{"uid": model.NewId(), "mail": "locale@example.test", "username": "saml_locale", "givenName": "Locale"}
			if tc.mapping != "" {
				values[tc.mapping] = tc.value
			}
			if tc.guest {
				values["member_type"] = "guest"
			}
			user, _, created, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", values, nil, true), nil)
			require.Nil(t, appErr)
			require.True(t, created)
			require.Equal(t, tc.want, user.Locale)
			require.Equal(t, tc.guest, user.IsGuest())
			stored, appErr := f.th.App.GetUser(f.th.Context, user.Id)
			require.Nil(t, appErr)
			require.Equal(t, tc.want, stored.Locale)
		})
	}
}

func TestExistingSAMLUserKeepsPreferredLocaleWithoutProviderMapping(t *testing.T) {
	f := setupSAML(t)
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.LocalizationSettings.DefaultClientLocale = new("ar") })
	require.Equal(t, "ar", *f.th.App.Config().LocalizationSettings.DefaultClientLocale)
	values := map[string]string{"uid": model.NewId(), "mail": "locale@example.test", "username": "saml_locale", "givenName": "Locale"}
	user, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", values, nil, true), nil)
	require.Nil(t, appErr)
	user.Locale = "fr"
	_, appErr = f.th.App.UpdateUser(f.th.Context, user, false)
	require.Nil(t, appErr)
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.LocalizationSettings.DefaultClientLocale = new("es") })
	require.Equal(t, "es", *f.th.App.Config().LocalizationSettings.DefaultClientLocale)
	again, _, created, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", values, nil, true), nil)
	require.Nil(t, appErr)
	require.False(t, created)
	require.Equal(t, user.Id, again.Id)
	require.Equal(t, "fr", again.Locale, "an existing user's choice must survive a server-default change")
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.LocaleAttribute = new("language") })
	values["language"] = "en"
	withMapping, _, created, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", values, nil, true), nil)
	require.Nil(t, appErr)
	require.False(t, created)
	require.Equal(t, "en", withMapping.Locale, "explicit provider mapping remains authoritative")
}
