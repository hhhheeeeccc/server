package access_control_job

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

const downloadAction = "download_file_attachment"

func simulateViaAPI(t *testing.T, th *api4.TestHelper, client *model.Client4, params model.PolicySimulationByUsersParams) *model.PolicySimulationResponse {
	t.Helper()
	body, err := json.Marshal(params)
	require.NoError(t, err)
	response, err := client.DoAPIPost(th.Context.Context(), "/access_control_policies/cel/simulate_users", string(body))
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var result model.PolicySimulationResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
	require.Len(t, result.Results, len(params.Users))
	return &result
}

func TestOfficialSimulationIncludesChannelPolicyForPermissionDraft(t *testing.T) {
	th, _ := setupChannelRules(t)
	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypePrivate)
	saveTestPolicy(t, th, &model.AccessControlPolicy{
		ID: channel.Id, Type: model.AccessControlPolicyTypeChannel,
		Rules: []model.AccessControlPolicyRule{{Name: "channel-download", Actions: []string{downloadAction}, Expression: "false", Role: model.ChannelUserRoleId}},
	})
	draft := &model.AccessControlPolicy{
		Type: model.AccessControlPolicyTypePermission, Name: "draft-permission",
		Roles: []string{model.SystemUserRoleId},
		Rules: []model.AccessControlPolicyRule{{Name: "download", Actions: []string{downloadAction}, Expression: "true"}},
	}
	params := model.PolicySimulationByUsersParams{
		Policy: draft, ChannelID: channel.Id, RuleName: "download", Actions: []string{downloadAction},
		Users: []model.PolicySimulationUserOverride{{UserID: th.BasicUser.Id}},
	}
	for _, scope := range []string{model.PolicyEvaluationScopeThisRule, model.PolicyEvaluationScopeAll} {
		params.EvaluationScope = scope
		result := simulateViaAPI(t, th, th.SystemAdminClient, params)
		require.Equal(t, scope == model.PolicyEvaluationScopeThisRule, result.Results[0].Decisions[downloadAction].Decision, scope)
	}
}

func TestOfficialSimulationReplacesPersistedParentWithDraft(t *testing.T) {
	th, _ := setupChannelRules(t)
	parent := saveTestPolicy(t, th, &model.AccessControlPolicy{
		Type:  model.AccessControlPolicyTypeParent,
		Rules: []model.AccessControlPolicyRule{{Name: "download", Actions: []string{downloadAction}, Expression: "false"}},
	})
	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypePrivate)
	child := saveTestPolicy(t, th, &model.AccessControlPolicy{
		ID: channel.Id, Type: model.AccessControlPolicyTypeChannel, Imports: []string{parent.ID},
		Rules: []model.AccessControlPolicyRule{{Name: "channel-download", Actions: []string{downloadAction}, Expression: "true", Role: model.ChannelUserRoleId}},
	})
	parent.Rules[0].Expression = "true"
	params := model.PolicySimulationByUsersParams{
		Policy: parent, ChannelID: channel.Id, RuleName: "download", Actions: []string{downloadAction},
		Users: []model.PolicySimulationUserOverride{{UserID: th.BasicUser.Id}}, EvaluationScope: model.PolicyEvaluationScopeAll,
	}
	result := simulateViaAPI(t, th, th.SystemAdminClient, params)
	require.True(t, result.Results[0].Decisions[downloadAction].Decision, "the saved version must be replaced by the draft")
	child.Rules[0].Expression = "false"
	saveTestPolicy(t, th, child)
	result = simulateViaAPI(t, th, th.SystemAdminClient, params)
	require.False(t, result.Results[0].Decisions[downloadAction].Decision, "other channel restrictions must still apply")
	stored, appErr := th.App.Srv().Channels().AccessControl.GetPolicy(th.Context, parent.ID)
	require.Nil(t, appErr)
	require.Equal(t, "false", stored.Rules[0].Expression, "simulation must not save the draft")
}

func TestOfficialParentActivationKeepsChildStateThroughAPI(t *testing.T) {
	th, _ := setupChannelRules(t)
	parent := saveTestPolicy(t, th, &model.AccessControlPolicy{
		Type:  model.AccessControlPolicyTypeParent,
		Rules: []model.AccessControlPolicyRule{{Actions: []string{model.AccessControlPolicyActionMembership}, Expression: "true"}},
	})
	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypePrivate)
	saveTestPolicy(t, th, &model.AccessControlPolicy{
		ID: channel.Id, Type: model.AccessControlPolicyTypeChannel, Imports: []string{parent.ID}, Active: false,
	})
	_, _, err := th.SystemAdminClient.SetAccessControlPolicyActive(th.Context.Context(), model.AccessControlPolicyActiveUpdateRequest{
		Entries: []model.AccessControlPolicyActiveUpdate{{ID: parent.ID, Active: true}},
	})
	require.NoError(t, err)
	child, _, err := th.SystemAdminClient.GetAccessControlPolicy(th.Context.Context(), channel.Id)
	require.NoError(t, err)
	require.False(t, child.AutoAddMembers())
	_, _, err = th.Client.SetAccessControlPolicyActive(th.Context.Context(), model.AccessControlPolicyActiveUpdateRequest{
		Entries: []model.AccessControlPolicyActiveUpdate{{ID: channel.Id, Active: true}},
	})
	require.NoError(t, err)
	_, _, err = th.SystemAdminClient.SetAccessControlPolicyActive(th.Context.Context(), model.AccessControlPolicyActiveUpdateRequest{
		Entries: []model.AccessControlPolicyActiveUpdate{{ID: parent.ID, Active: false}},
	})
	require.NoError(t, err)
	child, _, err = th.SystemAdminClient.GetAccessControlPolicy(th.Context.Context(), channel.Id)
	require.NoError(t, err)
	require.True(t, child.AutoAddMembers())
}

func TestOfficialPolicyAPIDeniesUnauthorizedEdits(t *testing.T) {
	th, _ := setupChannelRules(t)
	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypePrivate)
	policy := &model.AccessControlPolicy{
		ID: channel.Id, Type: model.AccessControlPolicyTypeChannel,
		Rules: []model.AccessControlPolicyRule{{Actions: []string{model.AccessControlPolicyActionMembership}, Expression: "true"}},
	}
	saved, _, err := th.Client.CreateAccessControlPolicy(th.Context.Context(), policy)
	require.NoError(t, err, "channel creator can manage their rules")
	outsider := th.CreateUser(t)
	client := th.CreateClient()
	_, _, err = client.Login(th.Context.Context(), outsider.Email, outsider.Password)
	require.NoError(t, err)
	saved.Rules[0].Expression = "false"
	_, response, err := client.CreateAccessControlPolicy(th.Context.Context(), saved)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	stored, _, err := th.SystemAdminClient.GetAccessControlPolicy(th.Context.Context(), channel.Id)
	require.NoError(t, err)
	require.Equal(t, "true", stored.Rules[0].Expression)
}

func TestOfficialPolicyAPIRequiresLicenseWithoutDeletingSavedPolicy(t *testing.T) {
	th, _ := setupChannelRules(t)
	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypePrivate)
	policy := &model.AccessControlPolicy{
		ID: channel.Id, Type: model.AccessControlPolicyTypeChannel,
		Rules: []model.AccessControlPolicyRule{{Actions: []string{model.AccessControlPolicyActionMembership}, Expression: "true"}},
	}
	saved, _, err := th.Client.CreateAccessControlPolicy(th.Context.Context(), policy)
	require.NoError(t, err)

	th.Server.SetLicense(nil)
	_, response, err := th.SystemAdminClient.GetAccessControlPolicy(th.Context.Context(), saved.ID)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	saved.Rules[0].Expression = "false"
	_, response, err = th.SystemAdminClient.CreateAccessControlPolicy(th.Context.Context(), saved)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	stored, _, err := th.SystemAdminClient.GetAccessControlPolicy(th.Context.Context(), saved.ID)
	require.NoError(t, err)
	require.Equal(t, "true", stored.Rules[0].Expression)
}

func TestOfficialSimulationCombinesGlobalPolicyWithoutExposingItsExpression(t *testing.T) {
	th, _ := setupChannelRules(t)
	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypePrivate)
	global := saveTestPolicy(t, th, &model.AccessControlPolicy{
		Type: model.AccessControlPolicyTypePermission, Active: true,
		Roles: []string{model.SystemUserRoleId},
		Rules: []model.AccessControlPolicyRule{{Name: "global-download", Actions: []string{downloadAction}, Expression: `user.email == "restricted@example.invalid"`}},
	})
	draft := &model.AccessControlPolicy{
		ID: channel.Id, Type: model.AccessControlPolicyTypeChannel,
		Rules: []model.AccessControlPolicyRule{{Name: "download", Actions: []string{downloadAction}, Expression: "true", Role: model.ChannelUserRoleId}},
	}
	params := model.PolicySimulationByUsersParams{
		Policy: draft, ChannelID: channel.Id, RuleName: "download", Actions: []string{downloadAction},
		Users: []model.PolicySimulationUserOverride{{UserID: th.BasicUser.Id}}, EvaluationScope: model.PolicyEvaluationScopeAll,
	}
	result := simulateViaAPI(t, th, th.Client, params)
	decision := result.Results[0].Decisions[downloadAction]
	require.False(t, decision.Decision)
	found := false
	for _, blame := range decision.Blame {
		if blame.PolicyID == global.ID {
			found = true
			require.Equal(t, model.PolicySimulationBlameSourceSystemPermission, blame.Source)
			require.Empty(t, blame.Expression)
			require.Nil(t, blame.EvaluationTree)
			require.Empty(t, blame.MergedRules)
		}
	}
	require.True(t, found, "denial still identifies another policy as its source")
	params.EvaluationScope = model.PolicyEvaluationScopeThisRule
	result = simulateViaAPI(t, th, th.Client, params)
	require.True(t, result.Results[0].Decisions[downloadAction].Decision)
}

func TestOfficialSimulationPrivateGuestMatchesEnforcement(t *testing.T) {
	th, _ := setupChannelRules(t)
	guest := th.CreateGuestUser(t)
	for _, channelType := range []model.ChannelType{model.ChannelTypePrivate, model.ChannelTypeOpen} {
		channel := th.CreateChannelWithClient(t, th.Client, channelType)
		draft := &model.AccessControlPolicy{
			ID: channel.Id, Type: model.AccessControlPolicyTypeChannel,
			Rules: []model.AccessControlPolicyRule{{Name: "membership", Actions: []string{model.AccessControlPolicyActionMembership}, Expression: "true"}},
		}
		params := model.PolicySimulationByUsersParams{
			Policy: draft, ChannelID: channel.Id, RuleName: "membership", Actions: []string{model.AccessControlPolicyActionMembership},
			Users: []model.PolicySimulationUserOverride{{UserID: guest.Id}}, EvaluationScope: model.PolicyEvaluationScopeAll,
		}
		result := simulateViaAPI(t, th, th.SystemAdminClient, params)
		require.Equal(t, channelType == model.ChannelTypeOpen, result.Results[0].Decisions[model.AccessControlPolicyActionMembership].Decision)
		params.EvaluationScope = model.PolicyEvaluationScopeThisRule
		result = simulateViaAPI(t, th, th.SystemAdminClient, params)
		require.True(t, result.Results[0].Decisions[model.AccessControlPolicyActionMembership].Decision, "an isolated rule still matches the guest's attributes")
	}
}
