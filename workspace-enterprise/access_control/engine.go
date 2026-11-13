package access_control

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/google/cel-go/interpreter"
	"github.com/google/cel-go/interpreter/functions"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/mattermost/mattermost/server/public/model"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

type engine struct {
	env            *cel.Env
	evaluationASTs *lru.Cache[string, *cel.Ast]
}

type rankLookup func(fieldID, optionName string) (int, bool)

const (
	// Bound both retained policy count and source size. Large expressions still
	// evaluate normally, but cannot fill the long-lived engine's cache.
	evaluationCostLimit         uint64 = 100_000
	evaluationTimeout                  = 100 * time.Millisecond
	evaluationASTCacheSize             = 256
	evaluationASTMaxSourceBytes        = 16 * 1024

	rankGEOverload       = "workspace_rank_ge"
	rankGTOverload       = "workspace_rank_gt"
	rankLEOverload       = "workspace_rank_le"
	rankLTOverload       = "workspace_rank_lt"
	rankEQOverload       = "workspace_rank_eq"
	rankNEOverload       = "workspace_rank_ne"
	rankGEFieldsOverload = "workspace_rank_ge_fields"
	rankGTFieldsOverload = "workspace_rank_gt_fields"
	rankLEFieldsOverload = "workspace_rank_le_fields"
	rankLTFieldsOverload = "workspace_rank_lt_fields"
	rankEQFieldsOverload = "workspace_rank_eq_fields"
	rankNEFieldsOverload = "workspace_rank_ne_fields"
)

func newEngine() (*engine, error) {
	env, err := cel.NewEnv(
		cel.Variable("user", cel.DynType),
		cel.Variable("resource", cel.DynType),
		cel.Function("hasAnyOf", cel.MemberOverload("workspace_list_has_any_of", []*cel.Type{cel.DynType, cel.DynType}, cel.BoolType, cel.BinaryBinding(hasAnyOf))),
		cel.Function("hasAllOf", cel.MemberOverload("workspace_list_has_all_of", []*cel.Type{cel.DynType, cel.DynType}, cel.BoolType, cel.BinaryBinding(hasAllOf))),
		cel.Function("inCIDR", cel.MemberOverload("workspace_string_in_cidr", []*cel.Type{cel.StringType, cel.StringType}, cel.BoolType, cel.BinaryBinding(inCIDR))),
		cel.Function("versionGTE", cel.MemberOverload("workspace_string_version_gte", []*cel.Type{cel.StringType, cel.StringType}, cel.BoolType, cel.BinaryBinding(versionGTE))),
		cel.Function("youngerThanDays", cel.MemberOverload("workspace_timestamp_younger_than_days", []*cel.Type{cel.IntType, cel.IntType}, cel.BoolType, cel.BinaryBinding(youngerThanDays))),
		cel.Function("_rank_ge",
			cel.Overload(rankGEOverload, []*cel.Type{cel.DynType, cel.DynType, cel.StringType}, cel.BoolType),
			cel.Overload(rankGEFieldsOverload, []*cel.Type{cel.DynType, cel.DynType, cel.StringType, cel.StringType}, cel.BoolType)),
		cel.Function("_rank_gt",
			cel.Overload(rankGTOverload, []*cel.Type{cel.DynType, cel.DynType, cel.StringType}, cel.BoolType),
			cel.Overload(rankGTFieldsOverload, []*cel.Type{cel.DynType, cel.DynType, cel.StringType, cel.StringType}, cel.BoolType)),
		cel.Function("_rank_le",
			cel.Overload(rankLEOverload, []*cel.Type{cel.DynType, cel.DynType, cel.StringType}, cel.BoolType),
			cel.Overload(rankLEFieldsOverload, []*cel.Type{cel.DynType, cel.DynType, cel.StringType, cel.StringType}, cel.BoolType)),
		cel.Function("_rank_lt",
			cel.Overload(rankLTOverload, []*cel.Type{cel.DynType, cel.DynType, cel.StringType}, cel.BoolType),
			cel.Overload(rankLTFieldsOverload, []*cel.Type{cel.DynType, cel.DynType, cel.StringType, cel.StringType}, cel.BoolType)),
		cel.Function("_rank_eq",
			cel.Overload(rankEQOverload, []*cel.Type{cel.DynType, cel.DynType, cel.StringType}, cel.BoolType),
			cel.Overload(rankEQFieldsOverload, []*cel.Type{cel.DynType, cel.DynType, cel.StringType, cel.StringType}, cel.BoolType)),
		cel.Function("_rank_ne",
			cel.Overload(rankNEOverload, []*cel.Type{cel.DynType, cel.DynType, cel.StringType}, cel.BoolType),
			cel.Overload(rankNEFieldsOverload, []*cel.Type{cel.DynType, cel.DynType, cel.StringType, cel.StringType}, cel.BoolType)),
	)
	if err != nil {
		return nil, err
	}
	cache, err := lru.New[string, *cel.Ast](evaluationASTCacheSize)
	if err != nil {
		return nil, err
	}
	return &engine{env: env, evaluationASTs: cache}, nil
}

func (e *engine) compile(expression string) (*cel.Ast, *cel.Issues) {
	if strings.TrimSpace(expression) == "" {
		expression = "false"
	}
	return e.env.Compile(expression)
}

// Only evaluation shares these checked ASTs. Editor/desugaring paths compile
// independently so their expression rewrites cannot change a cached policy.
// Programs are rebuilt: rank overloads close over the current request's lookup
// and must never retain another request's schema or rank values.
func (e *engine) evaluationAST(expression string) (*cel.Ast, error) {
	if strings.TrimSpace(expression) == "" {
		expression = "false"
	}
	cacheable := len(expression) <= evaluationASTMaxSourceBytes
	if cacheable {
		if ast, ok := e.evaluationASTs.Get(expression); ok {
			return ast, nil
		}
	}
	ast, issues := e.compile(expression)
	if issues.Err() != nil {
		return nil, issues.Err()
	}
	if cacheable {
		e.evaluationASTs.Add(expression, ast)
	}
	return ast, nil
}

func (e *engine) check(expression string) []model.CELExpressionError {
	ast, issues := e.compile(expression)
	if issues.Err() == nil && ast.OutputType() != cel.BoolType && ast.OutputType() != cel.DynType {
		return []model.CELExpressionError{{Line: 1, Column: 1, Message: "expression must return a boolean"}}
	}
	result := make([]model.CELExpressionError, 0, len(issues.Errors()))
	for _, issue := range issues.Errors() {
		result = append(result, model.CELExpressionError{
			Line:    issue.Location.Line(),
			Column:  issue.Location.Column() + 1,
			Message: issue.Message,
		})
	}
	return result
}

func (e *engine) evaluate(expression string, subject model.Subject, resourceAttributes map[string]any) (bool, error) {
	return e.evaluateWithRanks(expression, subject, resourceAttributes, nil)
}

func (e *engine) evaluateWithRanks(expression string, subject model.Subject, resourceAttributes map[string]any, lookup rankLookup) (bool, error) {
	return e.evaluateWithRanksContext(context.Background(), expression, subject, resourceAttributes, lookup)
}

func (e *engine) evaluateWithRanksContext(ctx context.Context, expression string, subject model.Subject, resourceAttributes map[string]any, lookup rankLookup) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, evaluationTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	ast, err := e.evaluationAST(expression)
	if err != nil {
		return false, err
	}
	program, err := e.env.Program(ast, cel.CostLimit(evaluationCostLimit), cel.InterruptCheckFrequency(64),
		cel.CostTrackerOptions(interpreter.OverloadCostTracker("workspace_list_has_any_of", listMembershipCost), interpreter.OverloadCostTracker("workspace_list_has_all_of", listMembershipCost)), cel.Functions(
			rankOverload(rankGEOverload, lookup, func(actual, expected int) bool { return actual >= expected }),
			rankFieldsOverload(rankGEFieldsOverload, lookup, func(actual, expected int) bool { return actual >= expected }),
			rankOverload(rankGTOverload, lookup, func(actual, expected int) bool { return actual > expected }),
			rankFieldsOverload(rankGTFieldsOverload, lookup, func(actual, expected int) bool { return actual > expected }),
			rankOverload(rankLEOverload, lookup, func(actual, expected int) bool { return actual <= expected }),
			rankFieldsOverload(rankLEFieldsOverload, lookup, func(actual, expected int) bool { return actual <= expected }),
			rankOverload(rankLTOverload, lookup, func(actual, expected int) bool { return actual < expected }),
			rankFieldsOverload(rankLTFieldsOverload, lookup, func(actual, expected int) bool { return actual < expected }),
			rankOverload(rankEQOverload, lookup, func(actual, expected int) bool { return actual == expected }),
			rankFieldsOverload(rankEQFieldsOverload, lookup, func(actual, expected int) bool { return actual == expected }),
			rankOverload(rankNEOverload, lookup, func(actual, expected int) bool { return actual != expected }),
			rankFieldsOverload(rankNEFieldsOverload, lookup, func(actual, expected int) bool { return actual != expected }),
		))
	if err != nil {
		return false, err
	}
	result, _, err := program.ContextEval(ctx, map[string]any{
		"user": map[string]any{
			"attributes": subject.Attributes,
			"session":    subject.Session,
			"email":      subject.Email,
			"verified":   subject.EmailVerified,
			"isbot":      subject.IsBot,
			"createat":   subject.CreateAt,
		},
		"resource": map[string]any{"attributes": resourceAttributes},
	})
	if err != nil {
		// CEL reports missing attributes and type mismatches as evaluation
		// errors. ABAC defines both cases as a non-match (deny), not as a
		// server failure.
		return false, nil
	}
	decision, ok := result.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expression returned %T instead of bool", result.Value())
	}
	return decision, nil
}

// rankFieldsOverload evaluates comparisons between two independent rank
// fields. Each side must be resolved with its own option list: two fields may
// reuse the same option label while assigning it a different numeric rank.
func rankFieldsOverload(name string, lookup rankLookup, compare func(int, int) bool) *functions.Overload {
	return &functions.Overload{
		Operator: name,
		Function: func(args ...ref.Val) ref.Val {
			if len(args) != 4 || lookup == nil {
				return types.False
			}
			leftFieldID, leftOK := args[2].(types.String)
			rightFieldID, rightOK := args[3].(types.String)
			if !leftOK || !rightOK {
				return types.False
			}
			actualRank, ok := rankFromValue(args[0], lookup, string(leftFieldID))
			if !ok {
				return types.False
			}
			expectedRank, ok := rankFromValue(args[1], lookup, string(rightFieldID))
			if !ok {
				return types.False
			}
			return types.Bool(compare(actualRank, expectedRank))
		},
	}
}

func rankOverload(name string, lookup rankLookup, compare func(int, int) bool) *functions.Overload {
	return &functions.Overload{
		Operator: name,
		Function: func(args ...ref.Val) ref.Val {
			if len(args) != 3 || lookup == nil {
				return types.False
			}
			fieldID, ok := args[2].(types.String)
			if !ok {
				return types.False
			}
			actualRank, ok := rankFromValue(args[0], lookup, string(fieldID))
			if !ok {
				return types.False
			}
			expectedRank, ok := rankFromValue(args[1], lookup, string(fieldID))
			if !ok {
				return types.False
			}
			return types.Bool(compare(actualRank, expectedRank))
		},
	}
}

func rankFromValue(value ref.Val, lookup rankLookup, fieldID string) (int, bool) {
	if mapper, ok := value.(traits.Mapper); ok {
		rankValue := mapper.Get(types.String("rank"))
		if rankValue == nil || types.IsError(rankValue) {
			return 0, false
		}
		switch rank := rankValue.(type) {
		case types.Int:
			return int(rank), true
		case types.Uint:
			return int(rank), true
		case types.Double:
			return int(rank), true
		}
	}
	if name, ok := value.(types.String); ok {
		return lookup(fieldID, string(name))
	}
	return 0, false
}

// Native CEL meters comprehensions; the custom membership overloads also need
// to charge their worst-case pair comparisons and bound a single function call.
func listMembershipTooLarge(left, right traits.Lister) bool {
	a, b := uint64(left.Size().(types.Int)), uint64(right.Size().(types.Int))
	return a > evaluationCostLimit || b > evaluationCostLimit || (a > 0 && b > evaluationCostLimit/a)
}

func listMembershipCost(args []ref.Val, _ ref.Val) *uint64 {
	cost := uint64(1)
	left, okLeft := args[0].(traits.Lister)
	right, okRight := args[1].(traits.Lister)
	if okLeft && okRight {
		if listMembershipTooLarge(left, right) {
			cost = evaluationCostLimit + 1
		} else {
			cost += uint64(left.Size().(types.Int)) * uint64(right.Size().(types.Int))
		}
	}
	return &cost
}

func hasAnyOf(lhs, rhs ref.Val) ref.Val {
	left, okLeft := lhs.(traits.Lister)
	right, okRight := rhs.(traits.Lister)
	if !okLeft || !okRight {
		return types.NewErr("hasAnyOf requires two lists")
	}
	if listMembershipTooLarge(left, right) {
		return types.NewErr("list membership cost limit exceeded")
	}
	iterator := left.Iterator()
	for iterator.HasNext() == types.True {
		if right.Contains(iterator.Next()) == types.True {
			return types.True
		}
	}
	return types.False
}

func hasAllOf(lhs, rhs ref.Val) ref.Val {
	left, okLeft := lhs.(traits.Lister)
	right, okRight := rhs.(traits.Lister)
	if !okLeft || !okRight {
		return types.NewErr("hasAllOf requires two lists")
	}
	if listMembershipTooLarge(left, right) {
		return types.NewErr("list membership cost limit exceeded")
	}
	iterator := right.Iterator()
	for iterator.HasNext() == types.True {
		if left.Contains(iterator.Next()) != types.True {
			return types.False
		}
	}
	return types.True
}

func inCIDR(lhs, rhs ref.Val) ref.Val {
	ip := net.ParseIP(string(lhs.(types.String)))
	_, network, err := net.ParseCIDR(string(rhs.(types.String)))
	if ip == nil || err != nil {
		return types.False
	}
	return types.Bool(network.Contains(ip))
}

func versionGTE(lhs, rhs ref.Val) ref.Val {
	parse := func(value string) []int {
		var parts []int
		for _, part := range strings.Split(strings.TrimPrefix(value, "v"), ".") {
			var n int
			if _, err := fmt.Sscanf(part, "%d", &n); err != nil {
				return nil
			}
			parts = append(parts, n)
		}
		return parts
	}
	a, b := parse(string(lhs.(types.String))), parse(string(rhs.(types.String)))
	if a == nil || b == nil {
		return types.False
	}
	for len(a) < len(b) {
		a = append(a, 0)
	}
	for len(b) < len(a) {
		b = append(b, 0)
	}
	for i := range a {
		if a[i] != b[i] {
			return types.Bool(a[i] > b[i])
		}
	}
	return types.True
}

func youngerThanDays(lhs, rhs ref.Val) ref.Val {
	createdAt := int64(lhs.(types.Int))
	days := int64(rhs.(types.Int))
	if createdAt <= 0 || days < 0 {
		return types.False
	}
	return types.Bool(createdAt >= time.Now().Add(-time.Duration(days)*24*time.Hour).UnixMilli())
}

func (e *engine) checkedExpression(expression string) (*exprpb.CheckedExpr, error) {
	ast, issues := e.compile(expression)
	if issues.Err() != nil {
		return nil, issues.Err()
	}
	return cel.AstToCheckedExpr(ast)
}

func selectPath(expr *exprpb.Expr) (string, bool) {
	selectExpr := expr.GetSelectExpr()
	if selectExpr == nil {
		if ident := expr.GetIdentExpr(); ident != nil {
			return ident.Name, true
		}
		return "", false
	}
	prefix, ok := selectPath(selectExpr.Operand)
	if !ok {
		return "", false
	}
	return prefix + "." + selectExpr.Field, true
}

func literalValues(expr *exprpb.Expr) []string {
	if constant := expr.GetConstExpr(); constant != nil {
		switch value := constant.ConstantKind.(type) {
		case *exprpb.Constant_StringValue:
			return []string{value.StringValue}
		case *exprpb.Constant_BoolValue:
			return []string{fmt.Sprint(value.BoolValue)}
		case *exprpb.Constant_Int64Value:
			return []string{fmt.Sprint(value.Int64Value)}
		case *exprpb.Constant_Uint64Value:
			return []string{fmt.Sprint(value.Uint64Value)}
		case *exprpb.Constant_DoubleValue:
			return []string{fmt.Sprint(value.DoubleValue)}
		}
	}
	if list := expr.GetListExpr(); list != nil {
		var result []string
		for _, element := range list.Elements {
			result = append(result, literalValues(element)...)
		}
		return result
	}
	return nil
}

func callAttributeAndValue(call *exprpb.Expr_Call) (string, *exprpb.Expr, bool) {
	if call == nil {
		return "", nil, false
	}
	if call.Target != nil && len(call.Args) > 0 {
		if path, ok := selectPath(call.Target); ok {
			return path, call.Args[0], true
		}
	}
	if strings.HasPrefix(call.Function, "_rank_") && (len(call.Args) == 3 || len(call.Args) == 4) {
		if path, ok := selectPath(call.Args[0]); ok {
			return path, call.Args[1], true
		}
	}
	if len(call.Args) != 2 {
		return "", nil, false
	}
	if path, ok := selectPath(call.Args[0]); ok {
		return path, call.Args[1], true
	}
	if path, ok := selectPath(call.Args[1]); ok {
		return path, call.Args[0], true
	}
	return "", nil, false
}

func walkExpr(expr *exprpb.Expr, visit func(*exprpb.Expr)) {
	if expr == nil {
		return
	}
	visit(expr)
	if call := expr.GetCallExpr(); call != nil {
		walkExpr(call.Target, visit)
		for _, arg := range call.Args {
			walkExpr(arg, visit)
		}
	}
	if list := expr.GetListExpr(); list != nil {
		for _, element := range list.Elements {
			walkExpr(element, visit)
		}
	}
	if structExpr := expr.GetStructExpr(); structExpr != nil {
		for _, entry := range structExpr.Entries {
			walkExpr(entry.GetMapKey(), visit)
			walkExpr(entry.Value, visit)
		}
	}
	if comprehension := expr.GetComprehensionExpr(); comprehension != nil {
		walkExpr(comprehension.IterRange, visit)
		walkExpr(comprehension.AccuInit, visit)
		walkExpr(comprehension.LoopCondition, visit)
		walkExpr(comprehension.LoopStep, visit)
		walkExpr(comprehension.Result, visit)
	}
}

func fieldName(path string) (string, bool) {
	for _, prefix := range []string{"user.attributes.", "user."} {
		if strings.HasPrefix(path, prefix) {
			name := strings.TrimPrefix(path, prefix)
			if name != "" && !strings.Contains(name, ".") {
				return name, true
			}
		}
	}
	return "", false
}

func (e *engine) ruleAttributes(expression string) (map[string][]string, error) {
	checked, err := e.checkedExpression(expression)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]string)
	walkExpr(checked.Expr, func(expr *exprpb.Expr) {
		call := expr.GetCallExpr()
		path, valueExpr, ok := callAttributeAndValue(call)
		if !ok {
			return
		}
		name, ok := fieldName(path)
		if !ok {
			return
		}
		result[name] = append(result[name], literalValues(valueExpr)...)
	})
	return result, nil
}

func operatorSymbol(function string) string {
	switch function {
	case operators.Equals:
		return "=="
	case operators.NotEquals:
		return "!="
	case operators.Less:
		return "<"
	case operators.LessEquals:
		return "<="
	case operators.Greater:
		return ">"
	case operators.GreaterEquals:
		return ">="
	case operators.In, operators.OldIn:
		return "in"
	default:
		return function
	}
}

func flattenAnd(expr *exprpb.Expr, out *[]*exprpb.Expr) bool {
	call := expr.GetCallExpr()
	if call != nil && call.Function == operators.LogicalAnd && len(call.Args) == 2 {
		return flattenAnd(call.Args[0], out) && flattenAnd(call.Args[1], out)
	}
	if call != nil && call.Function == operators.LogicalOr {
		return false
	}
	*out = append(*out, expr)
	return true
}

func (e *engine) visualExpression(expression string) (*model.VisualExpression, error) {
	checked, err := e.checkedExpression(expression)
	if err != nil {
		return nil, err
	}
	var leaves []*exprpb.Expr
	if !flattenAnd(checked.Expr, &leaves) {
		return nil, fmt.Errorf("OR expressions cannot be represented by the visual editor")
	}
	visual := &model.VisualExpression{Conditions: []model.Condition{}}
	for _, leaf := range leaves {
		call := leaf.GetCallExpr()
		path, valueExpr, ok := callAttributeAndValue(call)
		if !ok {
			return nil, fmt.Errorf("unsupported visual condition")
		}
		values := literalValues(valueExpr)
		var value any
		if len(values) == 1 {
			value = values[0]
		} else {
			value = values
		}
		valueType := model.LiteralValue
		if otherPath, ok := selectPath(valueExpr); ok {
			value, valueType = otherPath, model.AttrValue
		}
		visual.Conditions = append(visual.Conditions, model.Condition{
			Attribute: path, Operator: operatorSymbol(call.Function), Value: value, ValueType: valueType,
		})
	}
	return visual, nil
}

func exprShapeEqual(a, b *exprpb.Expr) bool {
	if a == nil || b == nil {
		return a == b
	}
	if ac, bc := a.GetCallExpr(), b.GetCallExpr(); ac != nil || bc != nil {
		if ac == nil || bc == nil || ac.Function != bc.Function || len(ac.Args) != len(bc.Args) || !exprShapeEqual(ac.Target, bc.Target) {
			return false
		}
		for i := range ac.Args {
			if !exprShapeEqual(ac.Args[i], bc.Args[i]) {
				return false
			}
		}
		return true
	}
	if as, bs := a.GetSelectExpr(), b.GetSelectExpr(); as != nil || bs != nil {
		return as != nil && bs != nil && as.Field == bs.Field && exprShapeEqual(as.Operand, bs.Operand)
	}
	if ai, bi := a.GetIdentExpr(), b.GetIdentExpr(); ai != nil || bi != nil {
		return ai != nil && bi != nil && ai.Name == bi.Name
	}
	if al, bl := a.GetListExpr(), b.GetListExpr(); al != nil || bl != nil {
		if al == nil || bl == nil || len(al.Elements) != len(bl.Elements) {
			return false
		}
		for i := range al.Elements {
			if !exprShapeEqual(al.Elements[i], bl.Elements[i]) {
				return false
			}
		}
		return true
	}
	if a.GetConstExpr() != nil || b.GetConstExpr() != nil {
		return a.GetConstExpr() != nil && b.GetConstExpr() != nil
	}
	return reflect.TypeOf(a.ExprKind) == reflect.TypeOf(b.ExprKind)
}
