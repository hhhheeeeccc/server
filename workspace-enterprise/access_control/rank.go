package access_control

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/operators"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
	"google.golang.org/protobuf/proto"
)

var rankMarkerByOperator = map[string]string{
	operators.Equals:        "_rank_eq",
	operators.NotEquals:     "_rank_ne",
	operators.GreaterEquals: "_rank_ge",
	operators.Greater:       "_rank_gt",
	operators.LessEquals:    "_rank_le",
	operators.Less:          "_rank_lt",
}

var rankOperatorByMarker = map[string]string{
	"_rank_eq": operators.Equals,
	"_rank_ne": operators.NotEquals,
	"_rank_ge": operators.GreaterEquals,
	"_rank_gt": operators.Greater,
	"_rank_le": operators.LessEquals,
	"_rank_lt": operators.Less,
}

var invertedRankMarker = map[string]string{
	operators.Equals:        "_rank_eq",
	operators.NotEquals:     "_rank_ne",
	operators.GreaterEquals: "_rank_le",
	operators.Greater:       "_rank_lt",
	operators.LessEquals:    "_rank_ge",
	operators.Less:          "_rank_gt",
}

func rankFieldPath(path string) (objectType, name string, ok bool) {
	switch {
	case strings.HasPrefix(path, "user.attributes."):
		name = strings.TrimPrefix(path, "user.attributes.")
		return model.PropertyFieldObjectTypeUser, name, name != "" && !strings.Contains(name, ".")
	case strings.HasPrefix(path, "resource.attributes."):
		name = strings.TrimPrefix(path, "resource.attributes.")
		return model.PropertyFieldObjectTypeChannel, name, name != "" && !strings.Contains(name, ".")
	default:
		return "", "", false
	}
}

func stringExpr(value string) *exprpb.Expr {
	return &exprpb.Expr{ExprKind: &exprpb.Expr_ConstExpr{ConstExpr: &exprpb.Constant{
		ConstantKind: &exprpb.Constant_StringValue{StringValue: value},
	}}}
}

func clonePolicy(policy *model.AccessControlPolicy) *model.AccessControlPolicy {
	if policy == nil {
		return nil
	}
	cloned := *policy
	cloned.Roles = append([]string(nil), policy.Roles...)
	cloned.Imports = append([]string(nil), policy.Imports...)
	cloned.Rules = append([]model.AccessControlPolicyRule(nil), policy.Rules...)
	for i := range cloned.Rules {
		cloned.Rules[i].Actions = append([]string(nil), policy.Rules[i].Actions...)
	}
	if policy.Props != nil {
		cloned.Props = make(map[string]any, len(policy.Props))
		for key, value := range policy.Props {
			cloned.Props[key] = value
		}
	}
	return &cloned
}

func (s *Service) transformExpression(rctx request.CTX, expression string, transform func(*exprpb.Expr_Call) *model.AppError) (string, *model.AppError) {
	if appErr := s.ensureEngine(rctx); appErr != nil {
		return "", appErr
	}
	ast, issues := s.engine.env.Parse(expression)
	if issues.Err() != nil {
		return "", badRequest("AccessControl.transformExpression", "enterprise.access_control.invalid_expression.app_error", issues.Err().Error())
	}
	parsed, err := cel.AstToParsedExpr(ast)
	if err != nil {
		return "", internalError("AccessControl.transformExpression", "enterprise.access_control.parse.app_error", err)
	}
	parsed = proto.Clone(parsed).(*exprpb.ParsedExpr)
	var transformErr *model.AppError
	walkExpr(parsed.Expr, func(expr *exprpb.Expr) {
		if transformErr != nil || expr.GetCallExpr() == nil {
			return
		}
		transformErr = transform(expr.GetCallExpr())
	})
	if transformErr != nil {
		return "", transformErr
	}
	result, err := cel.AstToString(cel.ParsedExprToAst(parsed))
	if err != nil {
		return "", internalError("AccessControl.transformExpression", "enterprise.access_control.unparse.app_error", err)
	}
	return result, nil
}

func (s *Service) desugarRankExpression(rctx request.CTX, expression string) (string, *model.AppError) {
	group, appErr := s.app.GetPropertyGroup(rctx, model.AccessControlPropertyGroupName)
	if appErr != nil {
		return "", appErr
	}
	return s.transformExpression(rctx, expression, func(call *exprpb.Expr_Call) *model.AppError {
		marker, isComparison := rankMarkerByOperator[call.Function]
		if !isComparison || len(call.Args) != 2 {
			return nil
		}

		attributeExpr, literalExpr := call.Args[0], call.Args[1]
		path, ok := selectPath(attributeExpr)
		if !ok {
			path, ok = selectPath(call.Args[1])
			if !ok {
				return nil
			}
			attributeExpr, literalExpr = call.Args[1], call.Args[0]
			marker = invertedRankMarker[call.Function]
		}
		objectType, fieldName, ok := rankFieldPath(path)
		if !ok {
			return nil
		}
		field, err := s.app.Srv().Store().PropertyField().GetFieldByNameForObjectType(rctx, group.ID, "", objectType, fieldName)
		if err != nil {
			return storeError("AccessControl.desugarRankExpression", "enterprise.access_control.rank_field.app_error", err)
		}
		if field.Type != model.PropertyFieldTypeRank {
			return nil
		}
		var targetFieldID string
		if _, ok := stringConstant(literalExpr); !ok {
			targetPath, ok := selectPath(literalExpr)
			if !ok {
				return nil
			}
			targetType, targetName, ok := rankFieldPath(targetPath)
			if !ok {
				return nil
			}
			targetField, err := s.app.Srv().Store().PropertyField().GetFieldByNameForObjectType(rctx, group.ID, "", targetType, targetName)
			if err != nil {
				return storeError("AccessControl.desugarRankExpression", "enterprise.access_control.rank_target_field.app_error", err)
			}
			if targetField.Type != model.PropertyFieldTypeRank {
				return badRequest("AccessControl.desugarRankExpression", "enterprise.access_control.rank_target_type.app_error", "rank attributes can only be compared with another rank attribute")
			}
			targetFieldID = targetField.ID
		}
		call.Function = marker
		call.Target = nil
		call.Args = []*exprpb.Expr{attributeExpr, literalExpr, stringExpr(field.ID)}
		if targetFieldID != "" {
			call.Args = append(call.Args, stringExpr(targetFieldID))
		}
		return nil
	})
}

func (s *Service) normalizeRankExpression(rctx request.CTX, expression string) (string, *model.AppError) {
	return s.transformExpression(rctx, expression, func(call *exprpb.Expr_Call) *model.AppError {
		operator, ok := rankOperatorByMarker[call.Function]
		if !ok {
			return nil
		}
		if len(call.Args) != 3 && len(call.Args) != 4 {
			return badRequest("AccessControl.normalizeRankExpression", "enterprise.access_control.rank_expression.app_error", "invalid stored rank expression")
		}
		call.Function = operator
		call.Target = nil
		call.Args = call.Args[:2]
		return nil
	})
}

func (s *Service) rankResolver(rctx request.CTX) rankLookup {
	type cachedField struct {
		ranks map[string]int
		ok    bool
	}
	cache := map[string]cachedField{}
	var groupID string
	return func(fieldID, optionName string) (int, bool) {
		entry, found := cache[fieldID]
		if !found {
			if groupID == "" {
				group, appErr := s.app.GetPropertyGroup(rctx, model.AccessControlPropertyGroupName)
				if appErr != nil {
					cache[fieldID] = cachedField{}
					return 0, false
				}
				groupID = group.ID
			}
			field, err := s.app.Srv().Store().PropertyField().Get(rctx, groupID, fieldID)
			if err != nil || field.Type != model.PropertyFieldTypeRank {
				cache[fieldID] = cachedField{}
				return 0, false
			}
			rawOptions, ok := field.Attrs[model.PropertyFieldAttributeOptions]
			if !ok {
				cache[fieldID] = cachedField{}
				return 0, false
			}
			options, err := model.NewPropertyOptionsFromFieldAttrs[*model.CustomProfileAttributesSelectOption](rawOptions)
			if err != nil {
				cache[fieldID] = cachedField{}
				return 0, false
			}
			entry = cachedField{ranks: make(map[string]int), ok: true}
			for _, option := range options {
				if option.Rank != nil {
					entry.ranks[option.Name] = *option.Rank
				}
			}
			cache[fieldID] = entry
		}
		if !entry.ok {
			return 0, false
		}
		rank, ok := entry.ranks[optionName]
		return rank, ok
	}
}

func (s *Service) evaluateExpression(rctx request.CTX, expression string, subject model.Subject, resourceAttributes map[string]any) (bool, error) {
	if s.engine == nil {
		return false, fmt.Errorf("access control engine is not initialized")
	}
	filtered, err := s.controlledSubject(rctx, subject)
	if err != nil {
		return false, err
	}
	return s.engine.evaluateWithRanksContext(rctx.Context(), expression, filtered, resourceAttributes, s.rankResolver(rctx))
}
