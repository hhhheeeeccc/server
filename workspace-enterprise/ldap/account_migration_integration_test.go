package ldap

import (
	"testing"

	"github.com/mattermost/enterprise/account_migration"
	ldapclient "github.com/mattermost/ldap"
	"github.com/stretchr/testify/require"
)

func TestAccountMigrationThenLDAPLoginKeepsLocalAccount(t *testing.T) {
	th, f := setupDirectoryAPI(t)
	users, err := th.Store.User().GetAllUsingAuthService("")
	require.NoError(t, err)
	entries := make([]*ldapclient.Entry, 0, len(users))
	for _, user := range users {
		dn := "uid=" + user.Id + ",dc=example,dc=test"
		if user.Id == th.BasicUser.Id {
			dn = "uid=one,dc=example,dc=test" // Credential accepted by the protocol fixture.
		}
		entries = append(entries, ldapclient.NewEntry(dn, map[string][]string{
			"objectClass": {"person"}, "uid": {"external" + user.Id},
			"entryUUID": {"ldap-" + user.Id}, "mail": {user.Email},
		}))
	}
	f.entriesMu.Lock()
	f.entries = entries
	f.entriesMu.Unlock()
	require.Nil(t, account_migration.New(th.App).MigrateToLdap(th.Context, "email", "email", false, false))
	client := th.CreateClient()
	u, _, err := client.Login(th.Context.Context(), "external"+th.BasicUser.Id, "user-password")
	require.NoError(t, err)
	require.Equal(t, th.BasicUser.Id, u.Id)
	require.Equal(t, th.BasicUser.Email, u.Email)
	require.Equal(t, "ldap", u.AuthService)
	_, err = th.Store.Channel().GetMember(th.Context, th.BasicChannel.Id, u.Id)
	require.NoError(t, err)
}
