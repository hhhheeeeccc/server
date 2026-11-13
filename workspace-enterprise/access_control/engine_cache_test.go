package access_control

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

func TestEvaluationASTCacheBounds(t *testing.T) {
	engine := newTestEngine(t)
	first, err := engine.evaluationAST("true")
	require.NoError(t, err)
	again, err := engine.evaluationAST("true")
	require.NoError(t, err)
	require.Same(t, first, again)
	for i := 0; i < evaluationASTCacheSize; i++ {
		_, err := engine.evaluationAST(fmt.Sprintf("user.createat > %d", i))
		require.NoError(t, err)
	}
	require.Equal(t, evaluationASTCacheSize, engine.evaluationASTs.Len())
	require.False(t, engine.evaluationASTs.Contains("true"), "least recently used policy must be evicted")

	large := "true" + strings.Repeat(" ", evaluationASTMaxSourceBytes)
	allowed, err := engine.evaluate(large, model.Subject{}, nil)
	require.NoError(t, err)
	require.True(t, allowed, "source size only limits caching, not existing policy evaluation")
	require.False(t, engine.evaluationASTs.Contains(large))
	_, err = engine.evaluationAST("user.attributes.")
	require.Error(t, err)
	require.False(t, engine.evaluationASTs.Contains("user.attributes."))
}

func TestEvaluationASTCacheDoesNotRetainDecisionsOrEditorChanges(t *testing.T) {
	engine := newTestEngine(t)
	expression := "user.verified"
	for _, verified := range []bool{true, false, true} {
		allowed, err := engine.evaluate(expression, model.Subject{EmailVerified: verified}, nil)
		require.NoError(t, err)
		require.Equal(t, verified, allowed)
	}
	checked, err := engine.checkedExpression(expression)
	require.NoError(t, err)
	checked.Expr.ExprKind = &exprpb.Expr_ConstExpr{ConstExpr: &exprpb.Constant{ConstantKind: &exprpb.Constant_BoolValue{BoolValue: false}}}
	allowed, err := engine.evaluate(expression, model.Subject{EmailVerified: true}, nil)
	require.NoError(t, err)
	require.True(t, allowed, "editor expression changes must not alter an evaluation AST")
	for _, expression := range []string{"", " \n "} {
		allowed, err := engine.evaluate(expression, model.Subject{}, nil)
		require.NoError(t, err)
		require.False(t, allowed)
	}
}

func TestEvaluationASTCacheUsesCurrentRankLookupConcurrently(t *testing.T) {
	engine := newTestEngine(t)
	expression := `_rank_ge(user.attributes.clearance, "Secret", "clearance-field")`
	subject := model.Subject{Attributes: map[string]any{"clearance": map[string]any{"rank": int64(20)}}}
	// All goroutines share an AST, but alternate between distinct rank schemas.
	_, err := engine.evaluationAST(expression)
	require.NoError(t, err)
	errors := make(chan error, 32)
	var workers sync.WaitGroup
	for i := 0; i < cap(errors); i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			threshold := 10 + (i%2)*20
			lookup := func(fieldID, option string) (int, bool) {
				return threshold, fieldID == "clearance-field" && option == "Secret"
			}
			for attempt := 0; attempt < 25; attempt++ {
				allowed, err := engine.evaluateWithRanks(expression, subject, nil, lookup)
				if err != nil || allowed != (threshold <= 20) {
					errors <- fmt.Errorf("schema %d: allowed=%v err=%v", threshold, allowed, err)
					return
				}
			}
		}(i)
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	allowed, err := engine.evaluateWithRanks(expression, subject, nil, nil)
	require.NoError(t, err)
	require.False(t, allowed, "a missing lookup must not reuse an earlier request's ranks")
}
