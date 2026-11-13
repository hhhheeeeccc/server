package access_control

import (
	"net/http"
	"slices"
	"strings"

	"github.com/google/cel-go/common/operators"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

func hasControlledValues(field *model.PropertyField) bool {
	return field.Protected || model.IsPropertyFieldSynced(field) ||
		field.Attrs[model.PropertyFieldAttrManaged] == "admin" || len(model.GetPropertyFieldOwners(field)) > 0
}

// constantAttributePath also recognizes CEL's equivalent bracket notation.
func constantAttributePath(expr *exprpb.Expr) (string, bool) {
	if call := expr.GetCallExpr(); call != nil && call.Function == operators.Index && len(call.Args) == 2 {
		prefix, ok := constantAttributePath(call.Args[0])
		key, literal := stringConstant(call.Args[1])
		return prefix + "." + key, ok && literal
	}
	if selected := expr.GetSelectExpr(); selected != nil {
		prefix, ok := constantAttributePath(selected.Operand)
		return prefix + "." + selected.Field, ok
	}
	return selectPath(expr)
}

func (s *Service) validateRuleAttributes(rctx request.CTX, rule model.AccessControlPolicyRule) *model.AppError {
	checked, err := s.engine.checkedExpression(rule.Expression)
	if err != nil {
		return badRequest("AccessControl.SavePolicy", "enterprise.access_control.invalid_expression.app_error", err.Error())
	}
	fields := map[string]bool{}
	usesSession := false
	usesResourceAttributes := false
	walkExpr(checked.Expr, func(expr *exprpb.Expr) {
		path, ok := constantAttributePath(expr)
		if !ok {
			return
		}
		if path == "user.session" || strings.HasPrefix(path, "user.session.") {
			usesSession = true
		}
		if path == "resource.attributes" || strings.HasPrefix(path, "resource.attributes.") {
			usesResourceAttributes = true
		}
		if name, found := strings.CutPrefix(path, "user.attributes."); found {
			fields[strings.SplitN(name, ".", 2)[0]] = true
		}
	})
	// The public server's ResourceAttributesInPolicies contract gates saves
	// only. Existing policies must keep being evaluated after the flag is off.
	if usesResourceAttributes && !s.app.ResourceAttributesInPoliciesEnabled() {
		return model.NewAppError("AccessControl.SavePolicy", "app.pap.save_policy.resource_attributes_disabled", nil, "", http.StatusBadRequest)
	}
	if usesSession {
		if slices.Contains(rule.Actions, model.AccessControlPolicyActionMembership) {
			return model.NewAppError("AccessControl.SavePolicy", "api.context.invalid_body_param.app_error", map[string]any{"Name": "rules"}, "membership rules cannot depend on session attributes", http.StatusBadRequest)
		}
		if !s.app.Config().FeatureFlags.SessionAttributes {
			return model.NewAppError("AccessControl.SavePolicy", "api.user.session_attributes.disabled.app_error", nil, "", http.StatusBadRequest)
		}
	}
	if len(fields) == 0 || *s.app.Config().AccessControlSettings.EnableUserManagedAttributes {
		return nil
	}
	group, appErr := s.app.GetPropertyGroup(rctx, model.AccessControlPropertyGroupName)
	if appErr != nil {
		return appErr
	}
	for name := range fields {
		field, err := s.app.Srv().Store().PropertyField().GetFieldByNameForObjectType(rctx, group.ID, "", model.PropertyFieldObjectTypeUser, name)
		if err != nil {
			return storeError("AccessControl.SavePolicy", "enterprise.access_control.attribute_field.app_error", err)
		}
		if !hasControlledValues(field) {
			return model.NewAppError("AccessControl.SavePolicy", "api.context.invalid_body_param.app_error", map[string]any{"Name": "rules"}, "user-managed attributes are disabled for access rules: "+name, http.StatusBadRequest)
		}
	}
	return nil
}

func (s *Service) controlledSubject(rctx request.CTX, subject model.Subject) (model.Subject, error) {
	if len(subject.Attributes) == 0 || *s.app.Config().AccessControlSettings.EnableUserManagedAttributes {
		return subject, nil
	}
	group, appErr := s.app.GetPropertyGroup(rctx, model.AccessControlPropertyGroupName)
	if appErr != nil {
		return subject, appErr
	}
	attributes := make(map[string]any, len(subject.Attributes))
	for name, value := range subject.Attributes {
		field, err := s.app.Srv().Store().PropertyField().GetFieldByNameForObjectType(rctx, group.ID, "", model.PropertyFieldObjectTypeUser, name)
		if store.IsErrNotFound(err) {
			continue
		}
		if err != nil {
			return subject, err
		}
		if hasControlledValues(field) {
			attributes[name] = value
		}
	}
	// Copy instead of mutating the cached subject returned by the app layer.
	subject.Attributes = attributes
	return subject, nil
}
