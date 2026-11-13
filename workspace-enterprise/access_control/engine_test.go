package access_control

import (
	"errors"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

func newTestEngine(t *testing.T) *engine {
	t.Helper()
	engine, err := newEngine()
	require.NoError(t, err)
	return engine
}

func TestEngineEvaluate(t *testing.T) {
	engine := newTestEngine(t)
	subject := model.Subject{
		Attributes: map[string]any{
			"department": "Engineering",
			"programs":   []string{"Orion", "Artemis"},
		},
		Session: map[string]any{
			"ip_address": "10.20.30.40",
			"os_version": "6.2.1",
		},
	}

	tests := []struct {
		name       string
		expression string
		want       bool
	}{
		{"attribute equality", `user.attributes.department == "Engineering"`, true},
		{"missing attribute denies", `user.attributes.missing == "value"`, false},
		{"list has any", `user.attributes.programs.hasAnyOf(["Other", "Orion"])`, true},
		{"list has all", `user.attributes.programs.hasAllOf(["Orion", "Artemis"])`, true},
		{"CIDR match", `user.session.ip_address.inCIDR("10.0.0.0/8")`, true},
		{"version comparison", `user.session.os_version.versionGTE("6.0.0")`, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := engine.evaluate(test.expression, subject, nil)
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

func TestEngineEvaluateRank(t *testing.T) {
	engine := newTestEngine(t)
	subject := model.Subject{Attributes: map[string]any{
		"clearance": map[string]any{"name": "Secret", "rank": int64(20)},
	}}
	lookup := func(fieldID, optionName string) (int, bool) {
		if fieldID != "clearance-field" {
			return 0, false
		}
		rank, ok := map[string]int{"Public": 10, "Secret": 20, "Top Secret": 30}[optionName]
		return rank, ok
	}

	allowed, err := engine.evaluateWithRanks(`_rank_ge(user.attributes.clearance, "Secret", "clearance-field")`, subject, nil, lookup)
	require.NoError(t, err)
	require.True(t, allowed)

	allowed, err = engine.evaluateWithRanks(`_rank_gt(user.attributes.clearance, "Top Secret", "clearance-field")`, subject, nil, lookup)
	require.NoError(t, err)
	require.False(t, allowed)

	allowed, err = engine.evaluateWithRanks(`_rank_eq(user.attributes.clearance, "Secret", "clearance-field")`, subject, nil, lookup)
	require.NoError(t, err)
	require.True(t, allowed)

	allowed, err = engine.evaluateWithRanks(`_rank_ge(user.attributes.clearance, resource.attributes.minimum, "clearance-field")`, subject, map[string]any{
		"minimum": map[string]any{"name": "Public", "rank": int64(10)},
	}, lookup)
	require.NoError(t, err)
	require.True(t, allowed)

	// Different rank fields may use the same option labels with different
	// numeric positions. The four-argument stored form resolves each side
	// against its own field instead of silently reusing the left schema.
	fieldLookup := func(fieldID, optionName string) (int, bool) {
		ranks := map[string]map[string]int{
			"user-clearance":     {"Secret": 20},
			"resource-clearance": {"Secret": 30},
		}
		rank, ok := ranks[fieldID][optionName]
		return rank, ok
	}
	allowed, err = engine.evaluateWithRanks(
		`_rank_ge(user.attributes.clearance, resource.attributes.minimum, "user-clearance", "resource-clearance")`,
		model.Subject{Attributes: map[string]any{"clearance": "Secret"}},
		map[string]any{"minimum": "Secret"},
		fieldLookup,
	)
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestDeletePolicyWithoutLicenseSupportsServerCleanupFallback(t *testing.T) {
	service := &Service{}
	appErr := service.DeletePolicy(request.TestContext(t), model.NewId())
	require.NotNil(t, appErr)
	require.Equal(t, 406, appErr.StatusCode)
}

func TestExpressionWithoutNativeAttributes(t *testing.T) {
	service := &Service{engine: newTestEngine(t)}
	rctx := request.TestContext(t)

	result, appErr := service.expressionWithoutNativeAttributes(rctx, `user.email.endsWith("example.com") && user.attributes.department == "Engineering"`)
	require.Nil(t, appErr)
	require.NotContains(t, result, "user.email")
	require.Contains(t, result, "user.attributes.department")

	result, appErr = service.expressionWithoutNativeAttributes(rctx, `user.verified && user.createat.youngerThanDays(30)`)
	require.Nil(t, appErr)
	require.Equal(t, "true", result)

	result, appErr = service.expressionWithoutNativeAttributes(rctx, `user.attributes.email == "private@example.com"`)
	require.Nil(t, appErr)
	require.Contains(t, result, "user.attributes.email")
}

func TestRoleFallback(t *testing.T) {
	require.True(t, roleMatches(model.SystemAdminRoleId, model.SystemUserRoleId))
	require.True(t, roleMatches(model.ChannelAdminRoleId, model.ChannelUserRoleId))
	require.False(t, roleMatches(model.ChannelGuestRoleId, model.ChannelUserRoleId))
	require.False(t, roleMatches(model.SystemUserRoleId, model.SystemAdminRoleId))
}

func TestMetadataCarrierIsNotEvaluatedAsCEL(t *testing.T) {
	rule := model.AccessControlPolicyRule{Actions: []string{model.AccessControlPolicyActionMembership}}
	rule.SetAutoAddMode(model.AccessControlAutoAddAlways)
	policy := &model.AccessControlPolicy{
		ID:    model.NewId(),
		Type:  model.AccessControlPolicyTypeChannel,
		Rules: []model.AccessControlPolicyRule{rule},
	}
	service := &Service{engine: newTestEngine(t)}
	rctx := request.TestContext(t)

	normalized, appErr := service.NormalizePolicy(rctx, policy)
	require.Nil(t, appErr)
	require.Equal(t, policy, normalized)

	allowed, applies, err := service.evaluatePolicy(rctx, policy, model.AccessRequest{
		Subject:  model.Subject{ID: model.NewId(), Type: "user"},
		Resource: model.Resource{ID: policy.ID, Type: policy.Type},
		Action:   model.AccessControlPolicyActionMembership,
	}, nil, map[string]bool{})
	require.NoError(t, err)
	require.True(t, allowed)
	require.False(t, applies, "metadata-only rules configure synchronization and must not restrict access")
}

func TestEngineCheckAndVisualExpression(t *testing.T) {
	engine := newTestEngine(t)
	require.Empty(t, engine.check(`user.attributes.department == "Engineering"`))
	require.NotEmpty(t, engine.check(`user.attributes.department ==`))

	visual, err := engine.visualExpression(`user.attributes.department == "Engineering" && "Orion" in user.attributes.programs`)
	require.NoError(t, err)
	require.Equal(t, []model.Condition{
		{Attribute: "user.attributes.department", Operator: "==", Value: "Engineering", ValueType: model.LiteralValue},
		{Attribute: "user.attributes.programs", Operator: "in", Value: "Orion", ValueType: model.LiteralValue},
	}, visual.Conditions)

	_, err = engine.visualExpression(`user.attributes.department == "Engineering" || user.attributes.department == "Sales"`)
	require.Error(t, err)
}

func TestEngineRuleAttributes(t *testing.T) {
	engine := newTestEngine(t)
	attributes, err := engine.ruleAttributes(`user.attributes.department == "Engineering" && user.attributes.program in ["Orion", "Artemis"] && user.email.endsWith("example.com")`)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"Engineering"}, attributes["department"])
	require.ElementsMatch(t, []string{"Orion", "Artemis"}, attributes["program"])
	require.ElementsMatch(t, []string{"example.com"}, attributes["email"])
}

type testResolver struct {
	fields map[string]*model.MaskingFieldInfo
	err    error
}

func (r testResolver) Resolve(objectType, fieldName string) (*model.MaskingFieldInfo, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.fields[objectType+":"+fieldName], nil
}

func TestMaskingRoundTrip(t *testing.T) {
	service := &Service{engine: newTestEngine(t)}
	resolver := testResolver{fields: map[string]*model.MaskingFieldInfo{
		model.PropertyFieldObjectTypeUser + ":department": {Access: model.MaskingFieldAccessPublic},
		model.PropertyFieldObjectTypeUser + ":clearance":  {Access: model.MaskingFieldAccessSourceOnly},
	}}
	rctx := request.TestContext(t)
	stored := `user.attributes.department == "Engineering" && user.attributes.clearance in ["Secret", "TopSecret"]`

	hasHidden, appErr := service.HasMaskedValuesForCaller(rctx, stored, resolver)
	require.Nil(t, appErr)
	require.True(t, hasHidden)

	masked, changed, appErr := service.MaskExpressionForCaller(rctx, stored, resolver)
	require.Nil(t, appErr)
	require.True(t, changed)
	require.Contains(t, masked, model.MaskingTokenValue)
	require.NotContains(t, masked, "Secret")

	merged, appErr := service.MergeExpressionWithMaskedValuesCanonical(rctx, masked, stored, resolver)
	require.Nil(t, appErr)
	allowed, err := service.engine.evaluate(merged, model.Subject{Attributes: map[string]any{
		"department": "Engineering",
		"clearance":  "TopSecret",
	}}, nil)
	require.NoError(t, err)
	require.True(t, allowed)

	appErr = service.ValidateExpressionValuesForCaller(rctx, stored, resolver)
	require.NotNil(t, appErr)
	require.Equal(t, 403, appErr.StatusCode)
}

func TestMaskingResolverFailureIsClosed(t *testing.T) {
	service := &Service{engine: newTestEngine(t)}
	rctx := request.TestContext(t)
	hidden, appErr := service.HasMaskedValuesForCaller(rctx, `user.attributes.clearance == "Secret"`, testResolver{err: errors.New("lookup failed")})
	require.True(t, hidden)
	require.NotNil(t, appErr)
}
