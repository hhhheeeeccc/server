package phone_attribute

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"sync/atomic"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/app/properties"
)

var localMobilePhone = regexp.MustCompile(`^7[1378][0-9]{7}$`)

// Register validates phone attributes at the property service boundary, including
// writes made outside the administration page.
func Register(a *app.App) {
	a.Srv().PropertyService().AddHook(&hook{app: a, service: a.Srv().PropertyService()})
}

type hook struct {
	properties.BasePropertyHook
	app     *app.App
	service *properties.PropertyService
	group   atomic.Pointer[model.PropertyGroup]
}

func duplicatePhoneError() error {
	return model.NewAppError("phone_attribute.validate", "workspace.phone_attribute.duplicate_phone", nil,
		"This phone number is already assigned to another user.", http.StatusConflict)
}

func (h *hook) validate(rctx request.CTX, value *model.PropertyValue) (string, error) {
	group := h.group.Load()
	if group == nil {
		var err error
		group, err = h.service.GetPropertyGroup(model.AccessControlPropertyGroupName)
		if err != nil {
			return "", err
		}
		h.group.Store(group)
	}
	if value.GroupID != group.ID {
		return "", nil
	}

	field, err := h.service.GetPropertyField(rctx, value.GroupID, value.FieldID)
	if err != nil {
		return "", err
	}
	if field.ObjectType != "user" || field.Type != model.PropertyFieldTypeText || field.Attrs["value_type"] != "phone" {
		return "", nil
	}

	if !validPhoneValue(value.Value) {
		return "", model.NewAppError("phone_attribute.validate", "workspace.phone_attribute.invalid_phone", nil,
			"Phone number must contain 9 digits and begin with 71, 73, 77, or 78.", http.StatusBadRequest)
	}
	var phone string
	_ = json.Unmarshal(value.Value, &phone) // validPhoneValue already checked the JSON shape.
	if phone == "" {
		return "", nil
	}

	encodedPhone, _ := json.Marshal(phone)
	search := model.PropertyValueSearchOpts{
		GroupID: value.GroupID, TargetType: "user", Value: encodedPhone, PerPage: 100,
	}
	for {
		existing, err := h.app.Srv().Store().PropertyValue().SearchPropertyValues(rctx, search)
		if err != nil {
			return "", err
		}
		for _, match := range existing {
			if match.TargetID == value.TargetID {
				continue
			}
			if match.FieldID == value.FieldID {
				return "", duplicatePhoneError()
			}
			otherField, err := h.service.GetPropertyField(rctx, match.GroupID, match.FieldID)
			if err != nil {
				return "", err
			}
			if otherField.ObjectType == "user" && otherField.Type == model.PropertyFieldTypeText && otherField.Attrs["value_type"] == "phone" {
				return "", duplicatePhoneError()
			}
		}
		if len(existing) < search.PerPage {
			break
		}
		last := existing[len(existing)-1]
		search.Cursor = model.PropertyValueSearchCursor{PropertyValueID: last.ID, CreateAt: last.CreateAt}
	}
	return phone, nil
}

func validPhoneValue(raw json.RawMessage) bool {
	var phone string
	if err := json.Unmarshal(raw, &phone); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	return phone == "" || localMobilePhone.MatchString(phone)
}

func reserveBatchPhone(seen map[string]string, phone, targetID string) error {
	if phone == "" {
		return nil
	}
	if previousTarget, exists := seen[phone]; exists && previousTarget != targetID {
		return duplicatePhoneError()
	}
	seen[phone] = targetID
	return nil
}

func (h *hook) validateAll(rctx request.CTX, values []*model.PropertyValue) error {
	seen := make(map[string]string, len(values))
	for _, value := range values {
		phone, err := h.validate(rctx, value)
		if err != nil {
			return err
		}
		if err := reserveBatchPhone(seen, phone, value.TargetID); err != nil {
			return err
		}
	}
	return nil
}

func (h *hook) PreCreatePropertyValue(rctx request.CTX, value *model.PropertyValue) (*model.PropertyValue, error) {
	_, err := h.validate(rctx, value)
	return value, err
}

func (h *hook) PreCreatePropertyValues(rctx request.CTX, values []*model.PropertyValue) ([]*model.PropertyValue, error) {
	return values, h.validateAll(rctx, values)
}

func (h *hook) PreUpdatePropertyValue(rctx request.CTX, _ string, value *model.PropertyValue) (*model.PropertyValue, error) {
	_, err := h.validate(rctx, value)
	return value, err
}

func (h *hook) PreUpdatePropertyValues(rctx request.CTX, _ string, values []*model.PropertyValue) ([]*model.PropertyValue, error) {
	return values, h.validateAll(rctx, values)
}

func (h *hook) PreUpsertPropertyValue(rctx request.CTX, value *model.PropertyValue) (*model.PropertyValue, error) {
	_, err := h.validate(rctx, value)
	return value, err
}

func (h *hook) PreUpsertPropertyValues(rctx request.CTX, values []*model.PropertyValue) ([]*model.PropertyValue, error) {
	return values, h.validateAll(rctx, values)
}
