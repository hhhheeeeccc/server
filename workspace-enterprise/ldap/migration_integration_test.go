package ldap

import (
	"net/http"
	"testing"

	ldapclient "github.com/mattermost/ldap"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestMigrateIDAttributeCommitsWholeDirectoryMapping(t *testing.T) {
	th, f := setupDirectoryAPI(t)
	f.setUser(t, userFixture())
	s := th.App.Ldap()
	one, appErr := s.DoLogin(th.Context, "user-1", "user-password")
	require.Nil(t, appErr)
	_, appErr = s.DoLogin(th.Context, "user-2", "user-password")
	require.NotNil(t, appErr) // fixture's second entry has no email mapping

	attrs := userFixture()
	delete(attrs, "uid")
	f.setUser(t, attrs)
	err := s.MigrateIDAttribute(th.Context, "uid")
	require.Error(t, err, "missing target attribute must not change any account")
	stored, appErr := th.App.GetUser(th.Context, one.Id)
	require.Nil(t, appErr)
	require.Equal(t, "user-1", *stored.AuthData)

	f.setUser(t, userFixture())
	f.searchError.Store(ldapclient.LDAPResultSizeLimitExceeded)
	require.Error(t, s.MigrateIDAttribute(th.Context, "uid"))
	f.searchError.Store(0)
	require.NoError(t, s.MigrateIDAttribute(th.Context, "uid"))
	stored, appErr = th.App.GetUser(th.Context, one.Id)
	require.Nil(t, appErr)
	require.Equal(t, "one", *stored.AuthData)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.LdapSettings.IdAttribute = new("uid") })
	loggedIn, appErr := s.DoLogin(th.Context, "one", "user-password")
	require.Nil(t, appErr)
	require.Equal(t, one.Id, loggedIn.Id)
}

func TestMigrateIDAttributeRejectsAmbiguityAndUnlicensedCall(t *testing.T) {
	th, f := setupDirectoryAPI(t)
	f.setUser(t, userFixture())
	s := th.App.Ldap()
	_, appErr := s.DoLogin(th.Context, "user-1", "user-password")
	require.Nil(t, appErr)
	require.Error(t, s.MigrateIDAttribute(th.Context, "uid)(objectClass=*"))
	f.entriesMu.Lock()
	f.entries = append(f.entries, ldapclient.NewEntry("uid=duplicate,dc=example,dc=test", map[string][]string{
		"objectClass": {"person"}, "uid": {"one"}, "entryUUID": {"user-other"}}))
	f.entriesMu.Unlock()
	require.Error(t, s.MigrateIDAttribute(th.Context, "uid"))
	th.Server.SetLicense(nil)
	err := s.MigrateIDAttribute(th.Context, "uid")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, err.(*model.AppError).StatusCode)
}

func TestMigrateIDAttributeSupportsExchangedValues(t *testing.T) {
	th, directory := setupDirectoryAPI(t)
	first := userFixture()
	first["newID"] = []string{"user-2"}
	directory.setUser(t, first)
	directory.entriesMu.Lock()
	directory.entries = append(directory.entries, ldapclient.NewEntry("uid=two,dc=example,dc=test", map[string][]string{
		"objectClass": {"person"}, "uid": {"two"}, "entryUUID": {"user-2"},
		"newID": {"user-1"}, "mail": {"two@example.test"},
	}))
	directory.entriesMu.Unlock()
	s := th.App.Ldap()
	one, appErr := s.DoLogin(th.Context, "user-1", "user-password")
	require.Nil(t, appErr)
	secondID := "user-2"
	two, err := th.App.Srv().Store().User().Save(th.Context, &model.User{
		Username: "two", Email: "two@example.test", AuthService: model.UserAuthServiceLdap,
		AuthData: &secondID, Roles: model.SystemUserRoleId,
	})
	require.NoError(t, err)
	require.NoError(t, s.MigrateIDAttribute(th.Context, "newID"))
	one, appErr = th.App.GetUser(th.Context, one.Id)
	require.Nil(t, appErr)
	two, appErr = th.App.GetUser(th.Context, two.Id)
	require.Nil(t, appErr)
	require.Equal(t, "user-2", *one.AuthData)
	require.Equal(t, "user-1", *two.AuthData)
}
