package ldap

import (
	"net/http"
	"testing"

	ldapclient "github.com/mattermost/ldap"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestSAMLLDAPSyncUpdatesProfileAndRevokesDisabledAccount(t *testing.T) {
	th, directory := setupDirectoryAPI(t)
	customField := createLDAPTextField(t, th)
	propertyGroup, appErr := th.App.GetPropertyGroup(th.Context, model.AccessControlPropertyGroupName)
	require.Nil(t, appErr)
	admin := th.Context.WithContext(model.WithCallerID(th.Context.Context(), th.SystemAdminUser.Id))
	optionID := model.NewId()
	selectField, appErr := th.App.CreatePropertyField(admin, &model.PropertyField{
		GroupID: propertyGroup.ID, Name: "saml_ldap_select_" + model.NewId(),
		Type: model.PropertyFieldTypeSelect, ObjectType: model.PropertyFieldObjectTypeUser,
		TargetType: string(model.PropertyFieldTargetLevelSystem),
		Attrs: model.StringInterface{
			ldapOptionSourceAttribute:           "department",
			model.PropertyAttrsOwners:           []model.PropertyOwner{{ID: model.PropertyFieldAttrLDAP, Type: model.PropertyOwnerTypeService}},
			model.PropertyFieldAttributeOptions: []any{map[string]any{"id": optionID, "name": "Engineering"}},
		},
	}, false, "")
	require.Nil(t, appErr)
	attrs := userFixture()
	attrs["givenName"] = []string{"From LDAP"}
	attrs["department"] = []string{"Engineering"}
	directory.setUser(t, attrs)
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.LdapSettings.EnableSync = new(true)
		cfg.LdapSettings.FirstNameAttribute = new("givenName")
		cfg.SamlSettings.EnableSyncWithLdap = new(true)
	})
	id := "saml-identity"
	user, err := th.App.Srv().Store().User().Save(th.Context, &model.User{
		Username: "samlone", Email: "one@example.test", AuthService: model.UserAuthServiceSaml,
		AuthData: &id, FirstName: "From SAML", Roles: model.SystemUserRoleId,
	})
	require.NoError(t, err)
	directory.entriesMu.Lock()
	directory.entries[0] = ldapclient.NewEntry("cn=alpha,dc=example,dc=test", map[string][]string{
		"objectClass": {"groupOfNames"}, "cn": {"Alpha"}, "entryUUID": {"group-a"},
		"member": {"uid=one,dc=example,dc=test"},
	})
	directory.entriesMu.Unlock()
	group, appErr := th.App.CreateGroup(&model.Group{DisplayName: "Alpha", Source: model.GroupSourceLdap, RemoteId: new("group-a")})
	require.Nil(t, appErr)
	s := th.App.Ldap().(*Service)
	resolved, appErr := s.ResolveSAMLUser(th.Context, "user-1", "", true)
	require.Nil(t, appErr)
	require.Equal(t, "user-1", *resolved.AuthData)
	require.Nil(t, s.synchronize(th.Context, false))
	stored, appErr := th.App.GetUser(th.Context, user.Id)
	require.Nil(t, appErr)
	require.Equal(t, "From LDAP", stored.FirstName)
	require.Equal(t, "Engineering", ldapTextValue(t, th, customField, user.Id))
	valuesForSelect, appErr := th.App.SearchPropertyValues(th.Context, propertyGroup.ID, model.PropertyValueSearchOpts{
		TargetType: model.PropertyValueTargetTypeUser, TargetIDs: []string{user.Id},
		FieldID: selectField.ID, PerPage: 10,
	})
	require.Nil(t, appErr)
	require.Len(t, valuesForSelect, 1)
	require.JSONEq(t, `"`+optionID+`"`, string(valuesForSelect[0].Value))
	require.Equal(t, id, *stored.AuthData, "SAML binding must remain unchanged")
	require.Zero(t, stored.DeleteAt)
	values, appErr := s.GetUserAttributes(th.Context, id, []string{"givenName"})
	require.Nil(t, appErr)
	require.Equal(t, "From LDAP", values["givenName"])
	members, appErr := th.App.GetGroupMemberUsers(group.Id)
	require.Nil(t, appErr)
	require.Len(t, members, 1)
	require.Equal(t, user.Id, members[0].Id)

	attrs["userAccountControl"] = []string{"514"}
	directory.setUser(t, attrs)
	require.Nil(t, s.synchronize(th.Context, false))
	stored, appErr = th.App.GetUser(th.Context, user.Id)
	require.Nil(t, appErr)
	require.Positive(t, stored.DeleteAt)
	members, appErr = th.App.GetGroupMemberUsers(group.Id)
	require.Nil(t, appErr)
	require.Empty(t, members)
	delete(attrs, "userAccountControl")
	directory.setUser(t, attrs)
	require.Nil(t, s.synchronize(th.Context, false))
	stored, appErr = th.App.GetUser(th.Context, user.Id)
	require.Nil(t, appErr)
	require.Zero(t, stored.DeleteAt)
}

func TestSAMLLDAPSyncRejectsAmbiguousEmailBeforeWrites(t *testing.T) {
	th, directory := setupDirectoryAPI(t)
	directory.setUser(t, userFixture())
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.LdapSettings.EnableSync = new(true)
		cfg.SamlSettings.EnableSyncWithLdap = new(true)
	})
	id := "saml-identity"
	user, err := th.App.Srv().Store().User().Save(th.Context, &model.User{
		Username: "samlone", Email: "one@example.test", AuthService: model.UserAuthServiceSaml,
		AuthData: &id, Roles: model.SystemUserRoleId,
	})
	require.NoError(t, err)
	directory.entriesMu.Lock()
	directory.entries = append(directory.entries, ldapclient.NewEntry("uid=other,dc=example,dc=test", map[string][]string{
		"objectClass": {"person"}, "uid": {"other"}, "entryUUID": {"user-other"}, "mail": {"one@example.test"},
	}))
	directory.entriesMu.Unlock()
	appErr := th.App.Ldap().(*Service).synchronize(th.Context, false)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusConflict, appErr.StatusCode)
	stored, appErr := th.App.GetUser(th.Context, user.Id)
	require.Nil(t, appErr)
	require.Zero(t, stored.DeleteAt)
}

func TestSAMLLDAPSyncRejectsInvalidGroupBeforeBindingMigration(t *testing.T) {
	th, directory := setupDirectoryAPI(t)
	directory.setUser(t, userFixture())
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.LdapSettings.EnableSync = new(true)
		cfg.SamlSettings.EnableSyncWithLdap = new(true)
		cfg.SamlSettings.EnableSyncWithLdapIncludeAuth = new(true)
		cfg.SamlSettings.IdAttribute = new("uid")
	})
	oldID := "legacy-saml-id"
	user, err := th.App.Srv().Store().User().Save(th.Context, &model.User{
		Username: "samlone", Email: "one@example.test", AuthService: model.UserAuthServiceSaml,
		AuthData: &oldID, Roles: model.SystemUserRoleId,
	})
	require.NoError(t, err)
	_, appErr := th.App.CreateGroup(&model.Group{DisplayName: "Alpha", Source: model.GroupSourceLdap, RemoteId: new("group-a")})
	require.Nil(t, appErr)
	directory.entriesMu.Lock()
	directory.entries[0] = ldapclient.NewEntry("cn=alpha,dc=example,dc=test", map[string][]string{
		"objectClass": {"groupOfNames"}, "cn": {"Alpha"}, "entryUUID": {"group-a"},
		"member": {"invalid-dn"},
	})
	directory.entriesMu.Unlock()
	appErr = th.App.Ldap().(*Service).synchronize(th.Context, false)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusBadRequest, appErr.StatusCode)
	stored, err := th.Store.User().Get(th.Context, user.Id)
	require.NoError(t, err)
	require.Equal(t, oldID, *stored.AuthData)
}

func TestSAMLLDAPSyncCanLeaveGuestAccountsAlone(t *testing.T) {
	th, directory := setupDirectoryAPI(t)
	directory.setUser(t, userFixture())
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.LdapSettings.EnableSync = new(true)
		cfg.SamlSettings.EnableSyncWithLdap = new(true)
		cfg.SamlSettings.IgnoreGuestsLdapSync = new(true)
	})
	id := "guest-saml-identity"
	guest, err := th.App.Srv().Store().User().Save(th.Context, &model.User{
		Username: "samlguest", Email: "guest@example.test", AuthService: model.UserAuthServiceSaml,
		AuthData: &id, Roles: model.SystemGuestRoleId,
	})
	require.NoError(t, err)
	require.Nil(t, th.App.Ldap().(*Service).synchronize(th.Context, false))
	stored, appErr := th.App.GetUser(th.Context, guest.Id)
	require.Nil(t, appErr)
	require.Zero(t, stored.DeleteAt)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.SAML = new(false)
	require.True(t, th.Server.SetLicense(license))
	appErr = th.App.Ldap().(*Service).synchronize(th.Context, false)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusForbidden, appErr.StatusCode)
}

func TestSAMLLDAPSyncMigratesBindingAndSurvivesEmailChange(t *testing.T) {
	th, directory := setupDirectoryAPI(t)
	directory.setUser(t, userFixture())
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.LdapSettings.EnableSync = new(true)
		cfg.SamlSettings.EnableSyncWithLdap = new(true)
		cfg.SamlSettings.EnableSyncWithLdapIncludeAuth = new(true)
		cfg.SamlSettings.IdAttribute = new("uid")
	})
	oldID := "old-saml-email-binding"
	user, err := th.App.Srv().Store().User().Save(th.Context, &model.User{
		Username: "samlone", Email: "one@example.test", AuthService: model.UserAuthServiceSaml,
		AuthData: &oldID, Roles: model.SystemUserRoleId,
	})
	require.NoError(t, err)
	s := th.App.Ldap().(*Service)
	require.Nil(t, s.synchronize(th.Context, false))
	stored, appErr := th.App.GetUser(th.Context, user.Id)
	require.Nil(t, appErr)
	require.Equal(t, "user-1", *stored.AuthData)
	require.Zero(t, stored.DeleteAt)
	attrs := userFixture()
	attrs["mail"] = []string{"changed@example.test"}
	directory.setUser(t, attrs)
	require.Nil(t, s.synchronize(th.Context, false))
	stored, appErr = th.App.GetUser(th.Context, user.Id)
	require.Nil(t, appErr)
	require.Equal(t, "changed@example.test", stored.Email)
	require.Equal(t, "user-1", *stored.AuthData)
	require.Zero(t, stored.DeleteAt)
}

func TestSAMLLDAPSyncRejectsBindingOwnedByAnotherAccount(t *testing.T) {
	th, directory := setupDirectoryAPI(t)
	directory.setUser(t, userFixture())
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.LdapSettings.EnableSync = new(true)
		cfg.SamlSettings.EnableSyncWithLdap = new(true)
		cfg.SamlSettings.EnableSyncWithLdapIncludeAuth = new(true)
		cfg.SamlSettings.IdAttribute = new("uid")
	})
	targetID := "user-1"
	_, err := th.App.Srv().Store().User().Save(th.Context, &model.User{
		Username: "otherauth", Email: "other@example.test", AuthService: "google",
		AuthData: &targetID, Roles: model.SystemUserRoleId,
	})
	require.NoError(t, err)
	oldID := "old-binding"
	user, err := th.App.Srv().Store().User().Save(th.Context, &model.User{
		Username: "samlone", Email: "one@example.test", AuthService: model.UserAuthServiceSaml,
		AuthData: &oldID, Roles: model.SystemUserRoleId,
	})
	require.NoError(t, err)
	appErr := th.App.Ldap().(*Service).synchronize(th.Context, false)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusConflict, appErr.StatusCode)
	stored, appErr := th.App.GetUser(th.Context, user.Id)
	require.Nil(t, appErr)
	require.Equal(t, oldID, *stored.AuthData)
	require.Zero(t, stored.DeleteAt)
}
