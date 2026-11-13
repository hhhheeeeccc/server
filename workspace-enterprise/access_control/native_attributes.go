package access_control

import (
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/operators"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
	"google.golang.org/protobuf/proto"
)

func isNativeAttributePath(path string) bool {
	for _, native := range []string{"user.email", "user.verified", "user.isbot", "user.createat"} {
		if path == native || strings.HasPrefix(path, native+".") {
			return true
		}
	}
	return false
}

func containsNativeAttribute(expr *exprpb.Expr) bool {
	found := false
	walkExpr(expr, func(node *exprpb.Expr) {
		if path, ok := selectPath(node); ok && isNativeAttributePath(path) {
			found = true
		}
	})
	return found
}

// stripNativePredicate removes native-user predicates while preserving the
// remaining boolean structure. The bool return is false when the entire
// subtree was native and should be treated as an unconstrained true value.
func stripNativePredicate(expr *exprpb.Expr) (*exprpb.Expr, bool) {
	if expr == nil {
		return nil, false
	}
	call := expr.GetCallExpr()
	if call == nil {
		if containsNativeAttribute(expr) {
			return nil, false
		}
		return expr, true
	}

	if (call.Function == operators.LogicalAnd || call.Function == operators.LogicalOr) && len(call.Args) == 2 {
		left, keepLeft := stripNativePredicate(call.Args[0])
		right, keepRight := stripNativePredicate(call.Args[1])
		switch {
		case keepLeft && keepRight:
			call.Args[0], call.Args[1] = left, right
			return expr, true
		case keepLeft:
			return left, true
		case keepRight:
			return right, true
		default:
			return nil, false
		}
	}
	if call.Function == operators.LogicalNot && len(call.Args) == 1 {
		child, keep := stripNativePredicate(call.Args[0])
		if !keep {
			return nil, false
		}
		call.Args[0] = child
		return expr, true
	}
	if containsNativeAttribute(expr) {
		return nil, false
	}
	return expr, true
}

func (s *Service) expressionWithoutNativeAttributes(rctx request.CTX, expression string) (string, *model.AppError) {
	if appErr := s.ensureEngine(rctx); appErr != nil {
		return "", appErr
	}
	ast, issues := s.engine.env.Parse(expression)
	if issues.Err() != nil {
		return "", badRequest("AccessControl.expressionWithoutNativeAttributes", "enterprise.access_control.invalid_expression.app_error", issues.Err().Error())
	}
	parsed, err := cel.AstToParsedExpr(ast)
	if err != nil {
		return "", internalError("AccessControl.expressionWithoutNativeAttributes", "enterprise.access_control.parse.app_error", err)
	}
	parsed = proto.Clone(parsed).(*exprpb.ParsedExpr)
	root, keep := stripNativePredicate(parsed.Expr)
	if !keep {
		return "true", nil
	}
	parsed.Expr = root
	result, err := cel.AstToString(cel.ParsedExprToAst(parsed))
	if err != nil {
		return "", internalError("AccessControl.expressionWithoutNativeAttributes", "enterprise.access_control.unparse.app_error", err)
	}
	return result, nil
}
