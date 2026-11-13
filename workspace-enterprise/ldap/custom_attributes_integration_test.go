package ldap

import (
	"encoding/json"
	"net/http"
	"testing"

	ldapclient "github.com/mattermost/ldap"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func createLDAPTextField(t *testing.T, th *api4.TestHelper) *model.PropertyField {
	t.Helper()
	group, appErr := th.App.GetPropertyGroup(th.Context, model.AccessControlPropertyGroupName)
	require.Nil(t, appErr)
	admin := th.Context.WithContext(model.WithCallerID(th.Context.Context(), th.SystemAdminUser.Id))
	field, appErr := th.App.CreatePropertyField(admin, &model.PropertyField{
		GroupID: group.ID, Name: "ldap_department_" + model.NewId(),
		Type: model.PropertyFieldTypeText, ObjectType: model.PropertyFieldObjectTypeUser,
		TargetType: string(model.PropertyFieldTargetLevelSystem),
		Attrs:      model.StringInterface{model.PropertyFieldAttrLDAP: "department"},
	}, false, "")
	require.Nil(t, appErr)
	return field
}

func ldapTextValue(t *testing.T, th *api4.TestHelper, field *model.PropertyField, userID string) string {
	t.Helper()
	values, appErr := th.App.SearchPropertyValues(th.Context, field.GroupID, model.PropertyValueSearchOpts{
		TargetType: model.PropertyValueTargetTypeUser, TargetIDs: []string{userID},
		FieldID: field.ID, PerPage: 10,
	})
	require.Nil(t, appErr)
	require.Len(t, values, 1)
	var value string
	require.NoError(t, json.Unmarshal(values[0].Value, &value))
	return value
}

func TestLDAPCustomTextAttributesLoginAndSync(t *testing.T) {
	th, directory := setupDirectoryAPI(t)
	field := createLDAPTextField(t, th)
	attrs := userFixture()
	attrs["department"] = []string{"Engineering"}
	directory.setUser(t, attrs)
	user, appErr := th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
	require.Nil(t, appErr)
	require.Equal(t, "Engineering", ldapTextValue(t, th, field, user.Id))
	admin := th.Context.WithContext(model.WithCallerID(th.Context.Context(), th.SystemAdminUser.Id))
	_, appErr = th.App.UpsertPropertyValue(admin, &model.PropertyValue{
		GroupID: field.GroupID, FieldID: field.ID, TargetID: user.Id,
		TargetType: model.PropertyValueTargetTypeUser, Value: json.RawMessage(`"Manual"`),
	})
	require.NotNil(t, appErr, "a human caller must not overwrite an LDAP-owned field")
	require.Equal(t, "Engineering", ldapTextValue(t, th, field, user.Id))

	th.App.UpdateConfig(func(cfg *model.Config) { cfg.LdapSettings.EnableSync = new(true) })
	attrs["department"] = []string{"Operations"}
	directory.setUser(t, attrs)
	require.Nil(t, th.App.Ldap().(*Service).synchronize(th.Context, false))
	require.Equal(t, "Operations", ldapTextValue(t, th, field, user.Id))

	delete(attrs, "department")
	directory.setUser(t, attrs)
	require.Nil(t, th.App.Ldap().(*Service).synchronize(th.Context, false))
	require.Empty(t, ldapTextValue(t, th, field, user.Id))
}

func TestLDAPSelectAndMultiselectAttributesLoginAndSync(t *testing.T) {
	th, directory := setupDirectoryAPI(t)
	group, appErr := th.App.GetPropertyGroup(th.Context, model.AccessControlPropertyGroupName)
	require.Nil(t, appErr)
	admin := th.Context.WithContext(model.WithCallerID(th.Context.Context(), th.SystemAdminUser.Id))
	engineering, operations := model.NewId(), model.NewId()
	selectField, appErr := th.App.CreatePropertyField(admin, &model.PropertyField{
		GroupID: group.ID, Name: "ldap_select_" + model.NewId(),
		Type: model.PropertyFieldTypeSelect, ObjectType: model.PropertyFieldObjectTypeUser,
		TargetType: string(model.PropertyFieldTargetLevelSystem),
		Attrs: model.StringInterface{
			ldapOptionSourceAttribute: "department",
			model.PropertyAttrsOwners: []model.PropertyOwner{{ID: model.PropertyFieldAttrLDAP, Type: model.PropertyOwnerTypeService}},
			model.PropertyFieldAttributeOptions: []any{
				map[string]any{"id": engineering, "name": "Engineering"},
				map[string]any{"id": operations, "name": "Operations"},
			},
		},
	}, false, "")
	require.Nil(t, appErr)
	goID, sqlID := model.NewId(), model.NewId()
	multiField, appErr := th.App.CreatePropertyField(admin, &model.PropertyField{
		GroupID: group.ID, Name: "ldap_multi_" + model.NewId(),
		Type: model.PropertyFieldTypeMultiselect, ObjectType: model.PropertyFieldObjectTypeUser,
		TargetType: string(model.PropertyFieldTargetLevelSystem),
		Attrs: model.StringInterface{
			ldapOptionSourceAttribute: "skills",
			model.PropertyAttrsOwners: []model.PropertyOwner{{ID: model.PropertyFieldAttrLDAP, Type: model.PropertyOwnerTypeService}},
			model.PropertyFieldAttributeOptions: []any{
				map[string]any{"id": goID, "name": "Go"},
				map[string]any{"id": sqlID, "name": "SQL"},
			},
		},
	}, false, "")
	require.Nil(t, appErr)
	require.Equal(t, "department", selectField.Attrs[ldapOptionSourceAttribute])
	require.Equal(t, "skills", multiField.Attrs[ldapOptionSourceAttribute])
	mappedFields, appErr := th.App.Ldap().(*Service).ldapMappedFields(th.Context)
	require.Nil(t, appErr)
	require.Len(t, mappedFields, 2)
	valueOf := func(field *model.PropertyField, userID string) json.RawMessage {
		values, err := th.App.SearchPropertyValues(th.Context, group.ID, model.PropertyValueSearchOpts{
			TargetType: model.PropertyValueTargetTypeUser, TargetIDs: []string{userID},
			FieldID: field.ID, PerPage: 10,
		})
		require.Nil(t, err)
		require.Len(t, values, 1)
		return values[0].Value
	}
	attrs := userFixture()
	attrs["department"] = []string{"Engineering"}
	attrs["skills"] = []string{"Go", "SQL"}
	directory.setUser(t, attrs)
	user, appErr := th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
	require.Nil(t, appErr)
	require.JSONEq(t, `"`+engineering+`"`, string(valueOf(selectField, user.Id)))
	require.JSONEq(t, `["`+goID+`","`+sqlID+`"]`, string(valueOf(multiField, user.Id)))
	_, appErr = th.App.UpsertPropertyValue(admin, &model.PropertyValue{
		GroupID: group.ID, FieldID: selectField.ID, TargetID: user.Id,
		TargetType: model.PropertyValueTargetTypeUser, Value: json.RawMessage(`"` + operations + `"`),
	})
	require.NotNil(t, appErr, "a human caller must not overwrite an LDAP-owned option field")

	th.App.UpdateConfig(func(cfg *model.Config) { cfg.LdapSettings.EnableSync = new(true) })
	attrs["department"] = []string{"operations"}
	attrs["skills"] = []string{"sql", "GO", "sql"}
	directory.setUser(t, attrs)
	require.Nil(t, th.App.Ldap().(*Service).synchronize(th.Context, false))
	require.JSONEq(t, `"`+operations+`"`, string(valueOf(selectField, user.Id)))
	require.JSONEq(t, `["`+sqlID+`","`+goID+`"]`, string(valueOf(multiField, user.Id)))

	attrs["department"] = []string{"Unknown"}
	attrs["uid"] = []string{"renamed"}
	directory.setUser(t, attrs)
	require.NotNil(t, th.App.Ldap().(*Service).synchronize(th.Context, false))
	require.JSONEq(t, `"`+operations+`"`, string(valueOf(selectField, user.Id)))
	_, appErr = th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
	require.NotNil(t, appErr, "invalid mapped options must stop login before profile writes")
	stored, err := th.Store.User().Get(th.Context, user.Id)
	require.NoError(t, err)
	require.Equal(t, user.Username, stored.Username)
	require.JSONEq(t, `"`+operations+`"`, string(valueOf(selectField, user.Id)))

	attrs["uid"] = []string{"one"}
	delete(attrs, "department")
	delete(attrs, "skills")
	directory.setUser(t, attrs)
	require.Nil(t, th.App.Ldap().(*Service).synchronize(th.Context, false))
	require.JSONEq(t, `""`, string(valueOf(selectField, user.Id)))
	require.JSONEq(t, `[]`, string(valueOf(multiField, user.Id)))
}

func TestLDAPSyncRejectsInvalidOptionBeforeUpdatingEarlierUsers(t *testing.T) {
	th, directory := setupDirectoryAPI(t)
	group, appErr := th.App.GetPropertyGroup(th.Context, model.AccessControlPropertyGroupName)
	require.Nil(t, appErr)
	admin := th.Context.WithContext(model.WithCallerID(th.Context.Context(), th.SystemAdminUser.Id))
	field, appErr := th.App.CreatePropertyField(admin, &model.PropertyField{
		GroupID: group.ID, Name: "ldap_preflight_" + model.NewId(),
		Type: model.PropertyFieldTypeSelect, ObjectType: model.PropertyFieldObjectTypeUser,
		TargetType: string(model.PropertyFieldTargetLevelSystem),
		Attrs: model.StringInterface{
			ldapOptionSourceAttribute: "department",
			model.PropertyAttrsOwners: []model.PropertyOwner{{ID: model.PropertyFieldAttrLDAP, Type: model.PropertyOwnerTypeService}},
			model.PropertyFieldAttributeOptions: []any{
				map[string]any{"id": model.NewId(), "name": "Engineering"},
				map[string]any{"id": model.NewId(), "name": "Operations"},
			},
		},
	}, false, "")
	require.Nil(t, appErr)
	first := userFixture()
	first["department"] = []string{"Engineering"}
	directory.setUser(t, first)
	user, appErr := th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
	require.Nil(t, appErr)

	secondID := "user-2"
	_, err := th.App.Srv().Store().User().Save(th.Context, &model.User{
		Username: "two", Email: "two@example.test", AuthService: model.UserAuthServiceLdap,
		AuthData: &secondID, Roles: model.SystemUserRoleId,
	})
	require.NoError(t, err)

	first["department"] = []string{"Operations"}
	directory.setUser(t, first)
	directory.entriesMu.Lock()
	directory.entries = append(directory.entries, ldapclient.NewEntry("uid=two,dc=example,dc=test", map[string][]string{
		"objectClass": {"person"}, "uid": {"two"}, "entryUUID": {"user-2"},
		"mail": {"two@example.test"}, "department": {"Unknown"},
	}))
	directory.entriesMu.Unlock()
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.LdapSettings.EnableSync = new(true) })

	appErr = th.App.Ldap().(*Service).synchronize(th.Context, false)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusBadRequest, appErr.StatusCode)
	values, appErr := th.App.SearchPropertyValues(th.Context, group.ID, model.PropertyValueSearchOpts{
		TargetType: model.PropertyValueTargetTypeUser, TargetIDs: []string{user.Id},
		FieldID: field.ID, PerPage: 10,
	})
	require.Nil(t, appErr)
	require.Len(t, values, 1)
	mappedFields, appErr := th.App.Ldap().(*Service).ldapMappedFields(th.Context)
	require.Nil(t, appErr)
	require.Len(t, mappedFields, 1)
	expected, appErr := mappedLDAPFieldValue(mappedFields[0], []string{"Engineering"})
	require.Nil(t, appErr)
	require.JSONEq(t, string(expected), string(values[0].Value))
}

func TestLDAPLinkedSelectAttributeLogin(t *testing.T) {
	th, directory := setupDirectoryAPI(t)
	group, appErr := th.App.GetPropertyGroup(th.Context, model.AccessControlPropertyGroupName)
	require.Nil(t, appErr)
	admin := th.Context.WithContext(model.WithCallerID(th.Context.Context(), th.SystemAdminUser.Id))
	optionID := model.NewId()
	template, appErr := th.App.CreatePropertyField(admin, &model.PropertyField{
		GroupID: group.ID, Name: "ldap_template_" + model.NewId(),
		Type: model.PropertyFieldTypeSelect, ObjectType: model.PropertyFieldObjectTypeTemplate,
		TargetType: string(model.PropertyFieldTargetLevelSystem),
		Attrs: model.StringInterface{
			ldapOptionSourceAttribute:           "department",
			model.PropertyAttrsOwners:           []model.PropertyOwner{{ID: model.PropertyFieldAttrLDAP, Type: model.PropertyOwnerTypeService}},
			model.PropertyFieldAttributeOptions: []any{map[string]any{"id": optionID, "name": "Engineering"}},
		},
	}, false, "")
	require.Nil(t, appErr)
	linked, appErr := th.App.CreatePropertyField(admin, &model.PropertyField{
		GroupID: group.ID, Name: "ldap_linked_" + model.NewId(),
		Type: model.PropertyFieldTypeSelect, ObjectType: model.PropertyFieldObjectTypeUser,
		TargetType: string(model.PropertyFieldTargetLevelSystem), LinkedFieldID: &template.ID,
		Attrs: model.StringInterface{
			ldapOptionSourceAttribute: "department",
			model.PropertyAttrsOwners: []model.PropertyOwner{{ID: model.PropertyFieldAttrLDAP, Type: model.PropertyOwnerTypeService}},
		},
	}, false, "")
	require.Nil(t, appErr)
	require.Equal(t, "department", linked.Attrs[ldapOptionSourceAttribute])
	attrs := userFixture()
	attrs["department"] = []string{"Engineering"}
	directory.setUser(t, attrs)
	user, appErr := th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
	require.Nil(t, appErr)
	values, appErr := th.App.SearchPropertyValues(th.Context, group.ID, model.PropertyValueSearchOpts{
		TargetType: model.PropertyValueTargetTypeUser, TargetIDs: []string{user.Id},
		FieldID: linked.ID, PerPage: 10,
	})
	require.Nil(t, appErr)
	require.Len(t, values, 1)
	require.JSONEq(t, `"`+optionID+`"`, string(values[0].Value))
}

func TestLDAPOptionAttributeRequiresExclusiveServiceOwner(t *testing.T) {
	th, directory := setupDirectoryAPI(t)
	group, appErr := th.App.GetPropertyGroup(th.Context, model.AccessControlPropertyGroupName)
	require.Nil(t, appErr)
	admin := th.Context.WithContext(model.WithCallerID(th.Context.Context(), th.SystemAdminUser.Id))
	field, appErr := th.App.CreatePropertyField(admin, &model.PropertyField{
		GroupID: group.ID, Name: "ldap_unowned_" + model.NewId(),
		Type: model.PropertyFieldTypeSelect, ObjectType: model.PropertyFieldObjectTypeUser,
		TargetType: string(model.PropertyFieldTargetLevelSystem),
		Attrs: model.StringInterface{
			ldapOptionSourceAttribute:           "department",
			model.PropertyFieldAttributeOptions: []any{map[string]any{"name": "Engineering"}},
		},
	}, false, "")
	require.Nil(t, appErr)
	attrs := userFixture()
	attrs["department"] = []string{"Engineering"}
	directory.setUser(t, attrs)
	_, appErr = th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
	require.NotNil(t, appErr, "an unowned field would allow a human to overwrite LDAP values")
	require.Equal(t, http.StatusConflict, appErr.StatusCode)

	// The admin UI repairs legacy option fields by PATCHing only their missing
	// service owner. Verify that the app accepts the same update and LDAP login
	// can then write the protected value.
	field.Attrs[model.PropertyAttrsOwners] = []model.PropertyOwner{{ID: model.PropertyFieldAttrLDAP, Type: model.PropertyOwnerTypeService}}
	_, _, appErr = th.App.UpdatePropertyField(admin, group.ID, field, false, "")
	require.Nil(t, appErr)
	user, appErr := th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
	require.Nil(t, appErr)
	values, appErr := th.App.SearchPropertyValues(th.Context, group.ID, model.PropertyValueSearchOpts{
		TargetType: model.PropertyValueTargetTypeUser, TargetIDs: []string{user.Id},
		FieldID: field.ID, PerPage: 10,
	})
	require.Nil(t, appErr)
	require.Len(t, values, 1)
}
