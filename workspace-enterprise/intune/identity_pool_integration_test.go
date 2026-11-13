package intune

import (
	"context"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestIntuneLoginCompletesWithOneApplicationConnection(t *testing.T) {
	for _, mode := range []string{"create", "existing", "saml_existing"} {
		t.Run(mode, func(t *testing.T) {
			f := setupIntune(t)
			claims := validClaims()
			expectedID := ""
			if mode != "create" {
				service := model.ServiceOffice365
				if mode == "saml_existing" {
					service = model.UserAuthServiceSaml
					f.th.App.UpdateConfig(func(cfg *model.Config) {
						cfg.IntuneSettings.AuthService = new(model.UserAuthServiceSaml)
						cfg.SamlSettings.Enable = new(true)
						cfg.SamlSettings.Encrypt = new(false)
						cfg.SamlSettings.IdpURL = new("https://idp.example.test/sso")
						cfg.SamlSettings.IdpDescriptorURL = new("https://idp.example.test/entity")
						cfg.SamlSettings.ServiceProviderIdentifier = new("https://workspace.example.test/entity")
						cfg.SamlSettings.AssertionConsumerServiceURL = new("https://workspace.example.test/login/sso/saml")
						cfg.SamlSettings.IdpCertificateFile = new("fixture-saml.crt")
						cfg.SamlSettings.EmailAttribute = new("mail")
						cfg.SamlSettings.UsernameAttribute = new("username")
					})
					require.Equal(t, model.UserAuthServiceSaml, *f.th.App.Config().IntuneSettings.AuthService)
					require.True(t, f.s.IsConfigured())
				}
				user, appErr := f.th.App.CreateUser(f.th.Context, &model.User{AuthService: service, AuthData: &claims.ObjectID, Email: claims.Email, Username: "pool_intune"})
				require.Nil(t, appErr)
				expectedID = user.Id
			}
			db := f.th.Store.GetInternalMasterDB()
			previous := db.Stats().MaxOpenConnections
			db.SetMaxOpenConns(1)
			defer db.SetMaxOpenConns(previous)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			user, appErr := f.s.Login(f.th.Context.WithContext(ctx), f.token(t, claims))
			require.NoError(t, ctx.Err(), "identity coordination must leave the native application connection available")
			require.Nil(t, appErr)
			require.NotNil(t, user)
			if expectedID != "" {
				require.Equal(t, expectedID, user.Id)
			}
			require.Equal(t, claims.ObjectID, *user.AuthData)
		})
	}
}
