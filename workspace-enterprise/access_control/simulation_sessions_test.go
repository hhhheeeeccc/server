package access_control

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSimulationSessionExpressions(t *testing.T) {
	engine := newTestEngine(t)
	for _, test := range []struct {
		expression string
		want       bool
	}{
		{`user.session.user_agent_os == "Linux"`, true},
		{`user.session["user_agent_os"] == "Linux"`, true},
		{`user["session"]["user_agent_os"] == "Linux"`, true},
		{`has(user.session.user_agent_os)`, true},
		{`user.attributes.department == "user.session"`, false},
		{`user.email == "user.session@example.test"`, false},
	} {
		t.Run(test.expression, func(t *testing.T) {
			checked, err := engine.checkedExpression(test.expression)
			require.NoError(t, err)
			require.Equal(t, test.want, expressionUsesSession(checked.Expr))
		})
	}
}

func TestSimulationSessionOverridesPreserveTypesAndBaseline(t *testing.T) {
	baseline := map[string]any{"user_agent_os": "Windows", "vpn_active": false}
	overrides := map[string]any{"user_agent_os": "Linux", "vpn_active": true}
	session := newSimulationSession(baseline, overrides)
	require.Equal(t, "Windows", baseline["user_agent_os"])
	require.Equal(t, false, baseline["vpn_active"])
	require.Equal(t, "Linux", session.attributes["user_agent_os"])
	require.Equal(t, true, session.attributes["vpn_active"])
	require.Equal(t, "true", session.result.Attributes["vpn_active"])
}
