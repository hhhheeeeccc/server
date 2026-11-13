package access_control

import (
	"context"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestEvaluationCostLimitDeniesNestedComprehensions(t *testing.T) {
	e := newTestEngine(t)
	values := make([]int, 400)
	for i := range values {
		values[i] = i
	}
	// A successful unbounded evaluation is true. Both all loops must finish;
	// there is no short-circuit that could accidentally hide missing metering.
	subject := model.Subject{Attributes: map[string]any{"values": values}}
	allowed, err := e.evaluate(`user.attributes.values.all(x, user.attributes.values.all(y, x + y >= 0))`, subject, nil)
	require.NoError(t, err)
	require.False(t, allowed, "cost exhaustion must deny access")
}

func TestEvaluationHonorsCanceledRequest(t *testing.T) {
	e := newTestEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	allowed, err := e.evaluateWithRanksContext(ctx, "true", model.Subject{}, nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, allowed)
	// No canceled request or rank closure can contaminate later requests.
	allowed, err = e.evaluate("true", model.Subject{}, nil)
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestCustomListMembershipAlsoHasBudget(t *testing.T) {
	e := newTestEngine(t)
	values := make([]int, 400)
	allowed, err := e.evaluate(`user.attributes.values.hasAllOf(user.attributes.values)`, model.Subject{Attributes: map[string]any{"values": values}}, nil)
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestEvaluationInterruptsAfterRequestIsCanceledMidProgram(t *testing.T) {
	e := newTestEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	values := make([]int, 200)
	called := false
	lookup := func(string, string) (int, bool) { called = true; cancel(); return 1, true }
	subject := model.Subject{Attributes: map[string]any{"rank": "Secret", "values": values}}
	allowed, err := e.evaluateWithRanksContext(ctx, `_rank_eq(user.attributes.rank, "Secret", "rank-field") && user.attributes.values.all(x, x >= 0)`, subject, nil, lookup)
	require.True(t, called)
	require.NoError(t, err)
	require.False(t, allowed, "a canceled program must stop even when its normal result is true and below the cost limit")
}
