package ldap

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"testing"

	ldapclient "github.com/mattermost/ldap"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

// Run against an isolated OpenLDAP instance. The test owns only its temporary
// OU and skips unless an administrator explicitly supplies test credentials.
func TestOpenLDAPLoginSyncAndDeactivation(t *testing.T) {
	ldapURL := os.Getenv("WORKSPACE_TEST_OPENLDAP_URL")
	baseDN := os.Getenv("WORKSPACE_TEST_OPENLDAP_BASE_DN")
	bindDN := os.Getenv("WORKSPACE_TEST_OPENLDAP_BIND_DN")
	bindPassword := os.Getenv("WORKSPACE_TEST_OPENLDAP_PASSWORD")
	if ldapURL == "" || baseDN == "" || bindDN == "" || bindPassword == "" {
		t.Skip("set WORKSPACE_TEST_OPENLDAP_URL, BASE_DN, BIND_DN and PASSWORD for an isolated directory")
	}
	parsed, err := url.Parse(ldapURL)
	require.NoError(t, err)
	require.Equal(t, "ldap", parsed.Scheme, "this test expects an isolated plaintext local LDAP endpoint")
	host := parsed.Hostname()
	require.True(t, host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()), "use a local isolated OpenLDAP instance")
	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)
	require.NotEmpty(t, host)

	admin, err := ldapclient.DialURL(ldapURL)
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })
	admin.Start()
	require.NoError(t, admin.Bind(bindDN, bindPassword))

	suffix := model.NewId()
	ouDN := "ou=workspace-" + suffix + "," + baseDN
	username := "openldap" + suffix
	userDN := "uid=" + username + "," + ouDN
	samlUsername := "samldir" + suffix
	samlDN := "uid=" + samlUsername + "," + ouDN
	password := "test-password-" + model.NewId()
	addOU := ldapclient.NewAddRequest(ouDN, nil)
	addOU.Attribute("objectClass", []string{"top", "organizationalUnit"})
	addOU.Attribute("ou", []string{"workspace-" + suffix})
	require.NoError(t, admin.Add(addOU))
	t.Cleanup(func() {
		_ = admin.Del(ldapclient.NewDelRequest(userDN, nil))
		_ = admin.Del(ldapclient.NewDelRequest(samlDN, nil))
		_ = admin.Del(ldapclient.NewDelRequest(ouDN, nil))
	})
	addUser := ldapclient.NewAddRequest(userDN, nil)
	addUser.Attribute("objectClass", []string{"top", "person", "organizationalPerson", "inetOrgPerson"})
	addUser.Attribute("uid", []string{username})
	addUser.Attribute("cn", []string{"OpenLDAP Test"})
	addUser.Attribute("sn", []string{"Test"})
	addUser.Attribute("givenName", []string{"Before"})
	addUser.Attribute("mail", []string{username + "@example.test"})
	addUser.Attribute("userPassword", []string{password})
	require.NoError(t, admin.Add(addUser))

	settings := model.LdapSettings{}
	settings.SetDefaults()
	settings.Enable = new(true)
	settings.EnableSync = new(true)
	settings.LdapServer = &host
	settings.LdapPort = &port
	settings.BaseDN = &ouDN
	settings.BindUsername = &bindDN
	settings.BindPassword = &bindPassword
	settings.IdAttribute = new("entryUUID")
	settings.GroupIdAttribute = new("entryUUID")
	settings.GroupDisplayNameAttribute = new("cn")
	settings.UsernameAttribute = new("uid")
	settings.LoginIdAttribute = new("uid")
	settings.EmailAttribute = new("mail")
	settings.FirstNameAttribute = new("givenName")
	settings.UserFilter = new("(objectClass=inetOrgPerson)")
	settings.QueryTimeout = new(5)
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.LdapSettings = settings
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		*cfg.EmailSettings.SendEmailNotifications = false
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	s := th.App.Ldap().(*Service)
	resolved, appErr := s.GetUser(th.Context, username)
	require.Nil(t, appErr)
	require.NotEmpty(t, *resolved.AuthData)
	_, appErr = s.DoLogin(th.Context, *resolved.AuthData, password)
	require.Nil(t, appErr)
	client := th.CreateClient()
	loggedIn, _, err := client.Login(th.Context.Context(), username, password)
	require.NoError(t, err)
	require.Equal(t, *resolved.AuthData, *loggedIn.AuthData)
	require.Equal(t, "Before", loggedIn.FirstName)
	addSAMLDirectoryUser := ldapclient.NewAddRequest(samlDN, nil)
	addSAMLDirectoryUser.Attribute("objectClass", []string{"top", "person", "organizationalPerson", "inetOrgPerson"})
	addSAMLDirectoryUser.Attribute("uid", []string{samlUsername})
	addSAMLDirectoryUser.Attribute("cn", []string{"SAML Directory Test"})
	addSAMLDirectoryUser.Attribute("sn", []string{"Test"})
	addSAMLDirectoryUser.Attribute("givenName", []string{"SAML Before"})
	addSAMLDirectoryUser.Attribute("mail", []string{samlUsername + "@example.test"})
	require.NoError(t, admin.Add(addSAMLDirectoryUser))
	samlID := "saml-" + model.NewId()
	samlUser, err := th.Store.User().Save(th.Context, &model.User{
		Username: samlUsername, Email: samlUsername + "@example.test",
		AuthService: model.UserAuthServiceSaml, AuthData: &samlID,
		FirstName: "From SAML", Roles: model.SystemUserRoleId,
	})
	require.NoError(t, err)
	samlSession, appErr := th.App.CreateSession(th.Context, &model.Session{UserId: samlUser.Id, Props: model.StringMap{}})
	require.Nil(t, appErr)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.EnableSyncWithLdap = new(true) })

	modify := ldapclient.NewModifyRequest(userDN, nil)
	modify.Replace("givenName", []string{"After"})
	modify.Replace("mail", []string{username + "-changed@example.test"})
	require.NoError(t, admin.Modify(modify))
	modifySAML := ldapclient.NewModifyRequest(samlDN, nil)
	modifySAML.Replace("givenName", []string{"SAML After"})
	require.NoError(t, admin.Modify(modifySAML))
	require.Nil(t, s.synchronize(th.Context, false))
	stored, err := th.Store.User().Get(th.Context, loggedIn.Id)
	require.NoError(t, err)
	require.Equal(t, "After", stored.FirstName)
	require.Equal(t, username+"-changed@example.test", stored.Email)
	require.Zero(t, stored.DeleteAt)
	samlStored, err := th.Store.User().Get(th.Context, samlUser.Id)
	require.NoError(t, err)
	require.Equal(t, "SAML After", samlStored.FirstName)
	require.Equal(t, samlID, *samlStored.AuthData)
	require.Zero(t, samlStored.DeleteAt)
	_, _, err = client.GetMe(th.Context.Context(), "")
	require.NoError(t, err)
	require.NoError(t, admin.Del(ldapclient.NewDelRequest(samlDN, nil)))
	require.Nil(t, s.synchronize(th.Context, false))
	samlStored, err = th.Store.User().Get(th.Context, samlUser.Id)
	require.NoError(t, err)
	require.Positive(t, samlStored.DeleteAt)
	_, appErr = th.App.GetSession(samlSession.Token)
	require.NotNil(t, appErr, "deactivation must revoke the SAML account session")

	require.NoError(t, admin.Del(ldapclient.NewDelRequest(userDN, nil)))
	require.Nil(t, s.synchronize(th.Context, false))
	stored, err = th.Store.User().Get(th.Context, loggedIn.Id)
	require.NoError(t, err)
	require.Positive(t, stored.DeleteAt)
	_, _, err = client.GetMe(th.Context.Context(), "")
	require.Error(t, err, "deactivation must revoke the existing session")
}
