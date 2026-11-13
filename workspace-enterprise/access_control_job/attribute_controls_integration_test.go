package access_control_job

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func createAttribute(t *testing.T, th *api4.TestHelper, field *model.PropertyField, userID, value string) *model.PropertyField {
	t.Helper()
	group, appErr := th.App.GetPropertyGroup(th.Context, model.AccessControlPropertyGroupName)
	require.Nil(t, appErr)
	field.GroupID = group.ID
	field.ObjectType = model.PropertyFieldObjectTypeUser
	field.TargetType = string(model.PropertyFieldTargetLevelSystem)
	field.Name = "f_" + model.NewId()
	adminContext := th.Context.WithContext(model.WithCallerID(th.Context.Context(), th.SystemAdminUser.Id))
	saved, appErr := th.App.CreatePropertyField(adminContext, field, false, "")
	require.Nil(t, appErr)
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	_, appErr = th.App.CreatePropertyValue(adminContext, &model.PropertyValue{
		GroupID: group.ID, FieldID: saved.ID, TargetID: userID,
		TargetType: model.PropertyValueTargetTypeUser, Value: raw,
	})
	require.Nil(t, appErr)
	require.NoError(t, th.App.Srv().Store().Attributes().RefreshAttributes())
	return saved
}

func TestUserManagedAttributesAreExcludedFromRulesAndEvaluation(t *testing.T) {
	th, _ := setupChannelRules(t)
	service := th.App.Srv().Channels().AccessControl
	for _, managed := range []string{"", "admin"} {
		t.Run(map[string]string{"": "user", "admin": "admin"}[managed], func(t *testing.T) {
			field := createAttribute(t, th, &model.PropertyField{
				Type:  model.PropertyFieldTypeText,
				Attrs: model.StringInterface{model.PropertyFieldAttrManaged: managed},
			}, th.BasicUser.Id, "Engineering")
			for _, expression := range []string{
				fmt.Sprintf("user.attributes.%s == 'Engineering'", field.Name),
				fmt.Sprintf("user['attributes']['%s'] == 'Engineering'", field.Name),
			} {
				policy := &model.AccessControlPolicy{
					ID: model.NewId(), Name: "attribute-" + model.NewId(), Type: model.AccessControlPolicyTypeParent,
					Rules: []model.AccessControlPolicyRule{{Actions: []string{model.AccessControlPolicyActionMembership}, Expression: expression}},
				}
				_, appErr := service.SavePolicy(th.Context, policy)
				if managed == "" {
					require.NotNil(t, appErr)
					require.Equal(t, http.StatusBadRequest, appErr.StatusCode)
				} else {
					require.Nil(t, appErr)
				}
			}
			th.App.UpdateConfig(func(cfg *model.Config) {
				*cfg.AccessControlSettings.EnableUserManagedAttributes = true
			})
			policy := saveTestPolicy(t, th, &model.AccessControlPolicy{
				Type:  model.AccessControlPolicyTypeParent,
				Rules: []model.AccessControlPolicyRule{{Actions: []string{model.AccessControlPolicyActionMembership}, Expression: fmt.Sprintf("user.attributes.%s == 'Engineering'", field.Name)}},
			})
			subject, appErr := th.App.BuildAccessControlSubject(th.Context, th.BasicUser.Id, th.BasicUser.Roles, "")
			require.Nil(t, appErr)
			require.Equal(t, "Engineering", subject.Attributes[field.Name])
			req := model.AccessRequest{
				Subject: *subject, Resource: model.Resource{ID: policy.ID, Type: policy.Type}, Action: model.AccessControlPolicyActionMembership,
			}
			decision, appErr := service.AccessEvaluation(th.Context, req)
			require.Nil(t, appErr)
			require.True(t, decision.Decision)
			th.App.UpdateConfig(func(cfg *model.Config) {
				*cfg.AccessControlSettings.EnableUserManagedAttributes = false
			})
			decision, appErr = service.AccessEvaluation(th.Context, req)
			require.Nil(t, appErr)
			require.Equal(t, managed == "admin", decision.Decision)
			require.Equal(t, "Engineering", subject.Attributes[field.Name], "evaluation must not mutate a cached subject")
		})
	}
}

func TestRankPreviewUsesOrderedValuesInsteadOfAlphabeticalOrder(t *testing.T) {
	th, _ := setupChannelRules(t)
	highOptionID := model.NewId()
	field := createAttribute(t, th, &model.PropertyField{
		Type: model.PropertyFieldTypeRank,
		Attrs: model.StringInterface{
			model.PropertyFieldAttrManaged: "admin",
			model.PropertyFieldAttributeOptions: []map[string]any{
				{"id": model.NewId(), "name": "Z-low", "rank": 1},
				{"id": highOptionID, "name": "A-high", "rank": 2},
			},
		},
	}, th.BasicUser.Id, highOptionID)
	service := th.App.Srv().Channels().AccessControl
	for _, expression := range []string{
		fmt.Sprintf("user.attributes.%s >= 'Z-low'", field.Name),
		fmt.Sprintf("'Z-low' <= user.attributes.%s", field.Name),
	} {
		users, count, appErr := service.QueryUsersForExpression(th.Context, expression, model.SubjectSearchOptions{SubjectID: th.BasicUser.Id})
		require.Nil(t, appErr)
		require.EqualValues(t, 1, count)
		require.Len(t, users, 1)
		require.Equal(t, th.BasicUser.Id, users[0].Id)
	}
}

func TestSessionAttributesCannotDetermineMembership(t *testing.T) {
	th, _ := setupChannelRules(t)
	th.ConfigStore.SetReadOnlyFF(false)
	service := th.App.Srv().Channels().AccessControl
	for _, enabled := range []bool{false, true} {
		th.App.UpdateConfig(func(cfg *model.Config) {
			cfg.FeatureFlags.SessionAttributes = enabled
		})
		for _, action := range []string{model.AccessControlPolicyActionMembership, "download_file_attachment"} {
			policy := &model.AccessControlPolicy{
				ID: model.NewId(), Name: "session-" + model.NewId(), Type: model.AccessControlPolicyTypeParent,
				Rules: []model.AccessControlPolicyRule{{Name: "session-rule", Actions: []string{action}, Expression: "user.session.ip.inCIDR('10.0.0.0/8')"}},
			}
			_, appErr := service.SavePolicy(th.Context, policy)
			if enabled && action != model.AccessControlPolicyActionMembership {
				require.Nil(t, appErr)
			} else {
				require.NotNil(t, appErr)
				require.Equal(t, http.StatusBadRequest, appErr.StatusCode)
			}
		}
	}
}

func TestParentPolicyDeletionRequiresUnassigningChannels(t *testing.T) {
	th, _ := setupChannelRules(t)
	service := th.App.Srv().Channels().AccessControl
	parent := saveTestPolicy(t, th, &model.AccessControlPolicy{
		Type:  model.AccessControlPolicyTypeParent,
		Rules: []model.AccessControlPolicyRule{{Actions: []string{model.AccessControlPolicyActionMembership}, Expression: "true"}},
	})
	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypePrivate)
	child := saveTestPolicy(t, th, &model.AccessControlPolicy{
		ID: channel.Id, Type: model.AccessControlPolicyTypeChannel, Imports: []string{parent.ID},
	})
	appErr := service.DeletePolicy(th.Context, parent.ID)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusBadRequest, appErr.StatusCode)
	_, appErr = service.GetPolicy(th.Context, parent.ID)
	require.Nil(t, appErr)
	require.Nil(t, service.DeletePolicy(th.Context, child.ID))
	require.Nil(t, service.DeletePolicy(th.Context, parent.ID))
	_, appErr = service.GetPolicy(th.Context, parent.ID)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusNotFound, appErr.StatusCode)
}
