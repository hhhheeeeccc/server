package ldap

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

type ldapMappedField struct {
	field     *model.PropertyField
	attribute string
	options   map[string]string
}

// The managed property-field hook reserves "ldap" for text fields and removes
// it from select fields. This Enterprise-owned attribute persists their source.
const ldapOptionSourceAttribute = "workspace_ldap_attribute"

func (s *Service) ldapMappedFields(rctx request.CTX) ([]ldapMappedField, *model.AppError) {
	group, appErr := s.app.GetPropertyGroup(rctx, model.AccessControlPropertyGroupName)
	if appErr != nil {
		if appErr.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, appErr
	}
	fields, appErr := s.app.GetPropertyFieldsForGroup(rctx, group.ID)
	if appErr != nil {
		return nil, appErr
	}
	result := make([]ldapMappedField, 0)
	for _, field := range fields {
		if field.ObjectType != model.PropertyFieldObjectTypeUser ||
			(field.Type != model.PropertyFieldTypeText && field.Type != model.PropertyFieldTypeSelect && field.Type != model.PropertyFieldTypeMultiselect) {
			continue
		}
		attributeKey := model.PropertyFieldAttrLDAP
		if field.Type != model.PropertyFieldTypeText {
			attributeKey = ldapOptionSourceAttribute
		}
		attribute, _ := field.Attrs[attributeKey].(string)
		if attribute == "" {
			continue
		}
		if !attributeName.MatchString(attribute) {
			return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
		}
		mapped := ldapMappedField{field: field, attribute: attribute}
		if field.Type != model.PropertyFieldTypeText {
			owners := model.GetPropertyFieldOwners(field)
			if len(owners) != 1 || owners[0].ID != model.PropertyFieldAttrLDAP || owners[0].Type != model.PropertyOwnerTypeService || len(owners[0].Scopes) != 0 {
				return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusConflict)
			}
			mapped.options, appErr = s.ldapOptionIDs(rctx, field)
			if appErr != nil {
				return nil, appErr
			}
		}
		result = append(result, mapped)
	}
	return result, nil
}

func (s *Service) ldapOptionIDs(rctx request.CTX, field *model.PropertyField) (map[string]string, *model.AppError) {
	reader := rctx.WithContext(model.WithCallerID(rctx.Context(), model.CallerIDLDAPSync))
	options := map[string]string{}
	var cursorAt int64
	var cursorID string
	for {
		page, appErr := s.app.GetPropertyFieldOptions(reader, field.GroupID, field.ID, cursorAt, cursorID, model.PropertyFieldOptionsMaxPerRequest)
		if appErr != nil {
			return nil, appErr
		}
		for _, option := range page.Options {
			key := strings.ToLower(option.Name)
			if key == "" || options[key] != "" {
				return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusConflict)
			}
			options[key] = option.ID
		}
		if !page.HasMore {
			return options, nil
		}
		if page.NextCursorID == "" || (page.NextCursorCreateAt == cursorAt && page.NextCursorID == cursorID) {
			return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusInternalServerError)
		}
		cursorAt, cursorID = page.NextCursorCreateAt, page.NextCursorID
	}
}

func appendLDAPMappedAttributes(attrs []string, fields []ldapMappedField) []string {
	for _, field := range fields {
		found := false
		for _, attr := range attrs {
			if attr == field.attribute {
				found = true
				break
			}
		}
		if !found {
			attrs = append(attrs, field.attribute)
		}
	}
	return attrs
}

func mappedLDAPFieldValue(field ldapMappedField, values []string) (json.RawMessage, *model.AppError) {
	var value any
	switch field.field.Type {
	case model.PropertyFieldTypeText:
		value = ""
		if len(values) > 0 {
			value = values[0]
		}
	case model.PropertyFieldTypeSelect:
		value = ""
		if len(values) > 1 {
			return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusConflict)
		}
		if len(values) == 1 {
			id := field.options[strings.ToLower(values[0])]
			if id == "" {
				return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
			}
			value = id
		}
	case model.PropertyFieldTypeMultiselect:
		ids := make([]string, 0, len(values))
		seen := map[string]bool{}
		for _, name := range values {
			id := field.options[strings.ToLower(name)]
			if id == "" {
				return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
			}
			if !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
		value = ids
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
	}
	return raw, nil
}

func validateLDAPCustomAttributes(entryValues map[string][]string, fields []ldapMappedField) *model.AppError {
	for _, field := range fields {
		if _, appErr := mappedLDAPFieldValue(field, entryValues[field.attribute]); appErr != nil {
			return appErr
		}
	}
	return nil
}

func (s *Service) syncCustomAttributes(rctx request.CTX, userID string, entryValues map[string][]string, fields []ldapMappedField) *model.AppError {
	if len(fields) == 0 {
		return nil
	}
	groupID := fields[0].field.GroupID
	current, appErr := s.app.SearchPropertyValues(rctx, groupID, model.PropertyValueSearchOpts{
		TargetType: model.PropertyValueTargetTypeUser, TargetIDs: []string{userID},
		PerPage: model.AccessControlGroupFieldLimit + 5,
	})
	if appErr != nil {
		return appErr
	}
	byField := make(map[string]*model.PropertyValue, len(current))
	for _, value := range current {
		byField[value.FieldID] = value
	}
	changes := make([]*model.PropertyValue, 0, len(fields))
	for _, field := range fields {
		raw, appErr := mappedLDAPFieldValue(field, entryValues[field.attribute])
		if appErr != nil {
			return appErr
		}
		if old := byField[field.field.ID]; old != nil && bytes.Equal(old.Value, raw) {
			continue
		}
		changes = append(changes, &model.PropertyValue{
			GroupID: groupID, FieldID: field.field.ID, TargetID: userID,
			TargetType: model.PropertyValueTargetTypeUser, Value: raw,
		})
	}
	if len(changes) == 0 {
		return nil
	}
	writer := rctx.WithContext(model.WithCallerID(rctx.Context(), model.CallerIDLDAPSync))
	_, appErr = s.app.UpsertPropertyValues(writer, changes, model.PropertyFieldObjectTypeUser, userID, "")
	return appErr
}
