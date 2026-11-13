package saml

import (
	"testing"

	"github.com/mattermost/enterprise/account_migration"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestAccountMigrationThenSignedSAMLLoginKeepsLocalAccount(t *testing.T) {
	f := setupSAML(t)
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.IdAttribute = new("") })
	require.Nil(t, account_migration.New(f.th.App).MigrateToSaml(f.th.Context, "email", nil, true, false))
	values := map[string]string{"mail": f.th.BasicUser.Email, "username": f.th.BasicUser.Username, "givenName": "Migrated"}
	u, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", values, nil, true), nil)
	require.Nil(t, appErr)
	require.Equal(t, f.th.BasicUser.Id, u.Id)
	require.Equal(t, "saml", u.AuthService)
	require.Equal(t, "Migrated", u.FirstName)
	_, err := f.th.Store.Channel().GetMember(f.th.Context, f.th.BasicChannel.Id, u.Id)
	require.NoError(t, err)
}

func TestExplicitExternalSAMLIDMigrationThenSignedLogin(t *testing.T) {
	f := setupSAML(t)
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.IdAttribute = new("immutableOID") })
	users, err := f.th.Store.User().GetAllUsingAuthService("")
	require.NoError(t, err)
	identities := make([]account_migration.SAMLIdentity, 0, len(users))
	for _, user := range users {
		identities = append(identities, account_migration.SAMLIdentity{
			UserID: user.Id, Username: user.Username, Email: user.Email, SAMLID: "oid-" + user.Id,
		})
	}
	service := account_migration.New(f.th.App).(*account_migration.Service)
	require.Nil(t, service.MigrateToSamlIdentities(f.th.Context, "email", identities, true))
	require.Nil(t, service.MigrateToSamlIdentities(f.th.Context, "email", identities, false))
	values := map[string]string{
		"mail": f.th.BasicUser.Email, "username": f.th.BasicUser.Username,
		"immutableOID": "oid-" + f.th.BasicUser.Id, "givenName": "Migrated External",
	}
	u, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", values, nil, true), nil)
	require.Nil(t, appErr)
	require.Equal(t, f.th.BasicUser.Id, u.Id)
	require.Equal(t, "oid-"+f.th.BasicUser.Id, *u.AuthData)
	require.Equal(t, "Migrated External", u.FirstName)
}
