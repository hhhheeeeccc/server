package access_control_job

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

// The public server contract for ResourceAttributesInPolicies gates authoring,
// but explicitly keeps enforcing previously stored policies when disabled.
// Reference: server/public/model/feature_flags.go at upstream 84ea31fcad.
func TestUpstreamResourceAttributesFlagGatesSaveButNotEnforcement(t *testing.T) {
	th, _ := setupChannelRules(t)
	th.ConfigStore.SetReadOnlyFF(false)
	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypePrivate)
	group, appErr := th.App.GetPropertyGroup(th.Context, model.AccessControlPropertyGroupName)
	require.Nil(t, appErr)
	adminContext := th.Context.WithContext(model.WithCallerID(th.Context.Context(), th.SystemAdminUser.Id))
	field, appErr := th.App.CreatePropertyField(adminContext, &model.PropertyField{
		GroupID: group.ID, Name: "f_" + model.NewId(), Type: model.PropertyFieldTypeText,
		ObjectType: model.PropertyFieldObjectTypeChannel, TargetType: string(model.PropertyFieldTargetLevelSystem),
		Attrs: model.StringInterface{model.PropertyFieldAttrManaged: "admin"},
	}, false, "")
	require.Nil(t, appErr)
	_, appErr = th.App.CreatePropertyValue(adminContext, &model.PropertyValue{
		GroupID: group.ID, FieldID: field.ID, TargetID: channel.Id,
		TargetType: model.PropertyValueTargetTypeChannel, Value: json.RawMessage(`"Engineering"`),
	})
	require.Nil(t, appErr)
	require.NoError(t, th.App.Srv().Store().Attributes().RefreshAttributes())

	for _, expression := range []string{
		fmt.Sprintf("resource.attributes.%s == 'Engineering'", field.Name),
		fmt.Sprintf("resource['attributes']['%s'] == 'Engineering'", field.Name),
	} {
		t.Run(expression, func(t *testing.T) {
			th.App.UpdateConfig(func(cfg *model.Config) { cfg.FeatureFlags.ResourceAttributesInPolicies = false })
			policy := &model.AccessControlPolicy{
				ID: channel.Id, Type: model.AccessControlPolicyTypeChannel,
				Rules: []model.AccessControlPolicyRule{{Name: "membership", Actions: []string{model.AccessControlPolicyActionMembership}, Expression: expression}},
			}
			_, response, err := th.SystemAdminClient.CreateAccessControlPolicy(th.Context.Context(), policy)
			require.Error(t, err, "disabled authoring must reject a new resource-attribute rule")
			require.Equal(t, http.StatusBadRequest, response.StatusCode)
			var apiErr *model.AppError
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, "app.pap.save_policy.resource_attributes_disabled", apiErr.Id)

			th.App.UpdateConfig(func(cfg *model.Config) { cfg.FeatureFlags.ResourceAttributesInPolicies = true })
			saved, _, err := th.SystemAdminClient.CreateAccessControlPolicy(th.Context.Context(), policy)
			require.NoError(t, err)
			service := th.App.Srv().Channels().AccessControl
			subject, appErr := th.App.BuildAccessControlSubject(th.Context, th.BasicUser.Id, th.BasicUser.Roles, channel.Id)
			require.Nil(t, appErr)
			request := model.AccessRequest{
				Subject: *subject, Resource: model.Resource{ID: channel.Id, Type: model.AccessControlPolicyTypeChannel},
				Action: model.AccessControlPolicyActionMembership,
			}
			for _, enabled := range []bool{true, false} {
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.FeatureFlags.ResourceAttributesInPolicies = enabled })
				decision, appErr := service.AccessEvaluation(th.Context, request)
				require.Nil(t, appErr)
				require.False(t, decision.IsNoPolicy())
				require.True(t, decision.Decision, "stored matching rule remains evaluated, flag=%v", enabled)
			}

			th.App.UpdateConfig(func(cfg *model.Config) { cfg.FeatureFlags.ResourceAttributesInPolicies = true })
			saved.Rules[0].Expression = fmt.Sprintf("resource.attributes.%s == 'Legal'", field.Name)
			_, _, err = th.SystemAdminClient.CreateAccessControlPolicy(th.Context.Context(), saved)
			require.NoError(t, err)
			service.InvalidateAllPolicyCaches(th.Context)
			th.App.UpdateConfig(func(cfg *model.Config) { cfg.FeatureFlags.ResourceAttributesInPolicies = false })
			decision, appErr := service.AccessEvaluation(th.Context, request)
			require.Nil(t, appErr)
			require.False(t, decision.IsNoPolicy())
			require.False(t, decision.Decision, "stored non-matching rule must still deny access")

			// Ordinary rules remain authorable when the resource-attribute flag is off.
			saved.Rules[0].Expression = "true"
			_, _, err = th.SystemAdminClient.CreateAccessControlPolicy(th.Context.Context(), saved)
			require.NoError(t, err)
		})
	}
}
