package access_control

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
	"google.golang.org/protobuf/proto"
)

type literalReference struct {
	objectType string
	fieldName  string
	expr       *exprpb.Expr
}

func maskingField(path string) (string, string, bool) {
	switch {
	case strings.HasPrefix(path, "user.attributes."):
		name := strings.TrimPrefix(path, "user.attributes.")
		return model.PropertyFieldObjectTypeUser, name, name != "" && !strings.Contains(name, ".")
	case strings.HasPrefix(path, "resource.attributes."):
		name := strings.TrimPrefix(path, "resource.attributes.")
		return model.PropertyFieldObjectTypeChannel, name, name != "" && !strings.Contains(name, ".")
	default:
		return "", "", false
	}
}

func appendStringLiteralRefs(expr *exprpb.Expr, objectType, fieldName string, refs *[]literalReference) {
	if expr == nil {
		return
	}
	if constant := expr.GetConstExpr(); constant != nil {
		if _, ok := constant.ConstantKind.(*exprpb.Constant_StringValue); ok {
			*refs = append(*refs, literalReference{objectType: objectType, fieldName: fieldName, expr: expr})
		}
		return
	}
	if list := expr.GetListExpr(); list != nil {
		for _, element := range list.Elements {
			appendStringLiteralRefs(element, objectType, fieldName, refs)
		}
	}
}

func collectLiteralReferences(root *exprpb.Expr) []literalReference {
	refs := []literalReference{}
	walkExpr(root, func(expr *exprpb.Expr) {
		path, valueExpr, ok := callAttributeAndValue(expr.GetCallExpr())
		if !ok {
			return
		}
		objectType, name, ok := maskingField(path)
		if !ok {
			return
		}
		appendStringLiteralRefs(valueExpr, objectType, name, &refs)
	})
	return refs
}

func stringConstant(expr *exprpb.Expr) (string, bool) {
	constant := expr.GetConstExpr()
	if constant == nil {
		return "", false
	}
	value, ok := constant.ConstantKind.(*exprpb.Constant_StringValue)
	if !ok {
		return "", false
	}
	return value.StringValue, true
}

func setStringConstant(expr *exprpb.Expr, value string) {
	expr.ExprKind = &exprpb.Expr_ConstExpr{ConstExpr: &exprpb.Constant{ConstantKind: &exprpb.Constant_StringValue{StringValue: value}}}
}

func resolveHidden(resolver model.MaskingFieldResolver, ref literalReference) (bool, error) {
	value, _ := stringConstant(ref.expr)
	info, err := resolver.Resolve(ref.objectType, ref.fieldName)
	if err != nil {
		return true, err
	}
	if info == nil {
		return true, fmt.Errorf("masking resolver returned no information for %s", ref.fieldName)
	}
	return info.IsValueHidden(value), nil
}

func (s *Service) HasMaskedValuesForCaller(rctx request.CTX, expression string, resolver model.MaskingFieldResolver) (bool, *model.AppError) {
	if appErr := s.ensureEngine(rctx); appErr != nil {
		return true, appErr
	}
	checked, err := s.engine.checkedExpression(expression)
	if err != nil {
		return true, badRequest("AccessControl.HasMaskedValuesForCaller", "enterprise.access_control.invalid_expression.app_error", err.Error())
	}
	for _, ref := range collectLiteralReferences(checked.Expr) {
		hidden, resolveErr := resolveHidden(resolver, ref)
		if resolveErr != nil {
			return true, internalError("AccessControl.HasMaskedValuesForCaller", "enterprise.access_control.masking_resolver.app_error", resolveErr)
		}
		if hidden {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) MaskExpressionForCaller(rctx request.CTX, expression string, resolver model.MaskingFieldResolver) (string, bool, *model.AppError) {
	if appErr := s.ensureEngine(rctx); appErr != nil {
		return "", false, appErr
	}
	checked, err := s.engine.checkedExpression(expression)
	if err != nil {
		return "", false, badRequest("AccessControl.MaskExpressionForCaller", "enterprise.access_control.invalid_expression.app_error", err.Error())
	}
	cloned := proto.Clone(checked).(*exprpb.CheckedExpr)
	changed := false
	for _, ref := range collectLiteralReferences(cloned.Expr) {
		hidden, resolveErr := resolveHidden(resolver, ref)
		if resolveErr != nil {
			return "", false, internalError("AccessControl.MaskExpressionForCaller", "enterprise.access_control.masking_resolver.app_error", resolveErr)
		}
		if hidden {
			setStringConstant(ref.expr, model.MaskingTokenValue)
			changed = true
		}
	}
	if !changed {
		return expression, false, nil
	}
	masked, err := cel.AstToString(cel.CheckedExprToAst(cloned))
	if err != nil {
		return "", false, internalError("AccessControl.MaskExpressionForCaller", "enterprise.access_control.unparse.app_error", err)
	}
	return masked, true, nil
}

func (s *Service) ValidateExpressionValuesForCaller(rctx request.CTX, expression string, resolver model.MaskingFieldResolver) *model.AppError {
	if appErr := s.ensureEngine(rctx); appErr != nil {
		return appErr
	}
	checked, err := s.engine.checkedExpression(expression)
	if err != nil {
		return badRequest("AccessControl.ValidateExpressionValuesForCaller", "enterprise.access_control.invalid_expression.app_error", err.Error())
	}
	for _, ref := range collectLiteralReferences(checked.Expr) {
		value, _ := stringConstant(ref.expr)
		if value == model.MaskingTokenValue {
			continue
		}
		hidden, resolveErr := resolveHidden(resolver, ref)
		if resolveErr != nil {
			return internalError("AccessControl.ValidateExpressionValuesForCaller", "enterprise.access_control.masking_resolver.app_error", resolveErr)
		}
		if hidden {
			return forbidden("AccessControl.ValidateExpressionValuesForCaller", "enterprise.access_control.masked_value_forbidden.app_error", "expression contains a value hidden from the caller")
		}
	}
	return nil
}

func (s *Service) MergeExpressionWithMaskedValuesCanonical(rctx request.CTX, submittedExpression, storedExpression string, resolver model.MaskingFieldResolver) (string, *model.AppError) {
	if appErr := s.ensureEngine(rctx); appErr != nil {
		return "", appErr
	}
	submitted, err := s.engine.checkedExpression(submittedExpression)
	if err != nil {
		return "", badRequest("AccessControl.MergeExpressionWithMaskedValuesCanonical", "enterprise.access_control.invalid_expression.app_error", err.Error())
	}
	stored, err := s.engine.checkedExpression(storedExpression)
	if err != nil {
		return "", badRequest("AccessControl.MergeExpressionWithMaskedValuesCanonical", "enterprise.access_control.invalid_stored_expression.app_error", err.Error())
	}

	storedRefs := collectLiteralReferences(stored.Expr)
	hasHidden := false
	for _, ref := range storedRefs {
		hidden, resolveErr := resolveHidden(resolver, ref)
		if resolveErr != nil {
			return "", internalError("AccessControl.MergeExpressionWithMaskedValuesCanonical", "enterprise.access_control.masking_resolver.app_error", resolveErr)
		}
		hasHidden = hasHidden || hidden
	}
	if !hasHidden {
		return submittedExpression, nil
	}
	if !exprShapeEqual(submitted.Expr, stored.Expr) {
		return "", forbidden("AccessControl.MergeExpressionWithMaskedValuesCanonical", "enterprise.access_control.masked_shape_changed.app_error", "expression structure changed while hidden values were present")
	}

	merged := proto.Clone(submitted).(*exprpb.CheckedExpr)
	mergedRefs := collectLiteralReferences(merged.Expr)
	if len(mergedRefs) != len(storedRefs) {
		return "", forbidden("AccessControl.MergeExpressionWithMaskedValuesCanonical", "enterprise.access_control.masked_shape_changed.app_error", "literal structure changed while hidden values were present")
	}
	for i, storedRef := range storedRefs {
		hidden, resolveErr := resolveHidden(resolver, storedRef)
		if resolveErr != nil {
			return "", internalError("AccessControl.MergeExpressionWithMaskedValuesCanonical", "enterprise.access_control.masking_resolver.app_error", resolveErr)
		}
		if !hidden {
			continue
		}
		submittedValue, _ := stringConstant(mergedRefs[i].expr)
		if submittedValue != model.MaskingTokenValue {
			return "", forbidden("AccessControl.MergeExpressionWithMaskedValuesCanonical", "enterprise.access_control.masked_value_changed.app_error", "a hidden value was changed or removed")
		}
		storedValue, _ := stringConstant(storedRef.expr)
		setStringConstant(mergedRefs[i].expr, storedValue)
	}
	result, err := cel.AstToString(cel.CheckedExprToAst(merged))
	if err != nil {
		return "", internalError("AccessControl.MergeExpressionWithMaskedValuesCanonical", "enterprise.access_control.unparse.app_error", err)
	}
	return result, nil
}
