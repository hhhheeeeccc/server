package access_control_job

import (
	"net/http"
	"testing"

	"github.com/mattermost/enterprise/access_control"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestSimulationUsesTargetSessionsInsteadOfAdministrator(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		*cfg.AccessControlSettings.EnableAttributeBasedAccessControl = true
		cfg.FeatureFlags.PermissionPolicies = true
		cfg.FeatureFlags.ChannelPermissionPolicies = true
		cfg.FeatureFlags.PolicySimulation = true
		cfg.FeatureFlags.SessionAttributes = true
		*cfg.EmailSettings.SendEmailNotifications = false
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	th.Server.Channels().AccessControl = access_control.New(th.App)
	group, appErr := th.App.GetPropertyGroup(th.Context, model.SessionAttributesPropertyGroupName)
	require.Nil(t, appErr)
	// The system schema is seeded by the server migration. Ensure this
	// identity field is enabled using the same property API as the console.
	fields, appErr := th.App.SearchPropertyFields(th.Context, group.ID, model.PropertyFieldSearchOpts{ObjectType: model.PropertyFieldObjectTypeSession, PerPage: 100})
	require.Nil(t, appErr)
	if len(fields) == 0 {
		for _, field := range model.SessionAttributeSystemFields(group.ID) {
			_, err := th.Server.Store().PropertyField().Create(field)
			require.NoError(t, err)
		}
		fields, appErr = th.App.SearchPropertyFields(th.Context, group.ID, model.PropertyFieldSearchOpts{ObjectType: model.PropertyFieldObjectTypeSession, PerPage: 100})
		require.Nil(t, appErr)
	}
	var osField *model.PropertyField
	for _, field := range fields {
		if field.Name == model.SessionAttributesPropertyFieldUserAgentOS {
			osField = field
		}
	}
	require.NotNil(t, osField)
	osField.Attrs["enabled"] = true
	_, _, appErr = th.App.UpdatePropertyFields(th.Context, group.ID, []*model.PropertyField{osField}, true, "")
	require.Nil(t, appErr)

	linuxUA := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	windowsUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	th.SystemAdminClient.HTTPHeader["User-Agent"] = linuxUA
	login := func(user *model.User, ua string) (*model.Client4, *model.Session) {
		t.Helper()
		client := th.CreateClient()
		client.HTTPHeader["User-Agent"] = ua
		_, _, err := client.Login(th.Context.Context(), user.Email, user.Password)
		require.NoError(t, err)
		_, _, err = client.GetMe(th.Context.Context(), "")
		require.NoError(t, err)
		session, appErr := th.App.GetSession(client.AuthToken)
		require.Nil(t, appErr)
		attrs, appErr := th.App.GetSessionAttributes(th.Context, session.Id)
		require.Nil(t, appErr)
		require.NotEmpty(t, attrs[model.SessionAttributesPropertyFieldUserAgentOS])
		return client, session
	}
	winClient, windows := login(th.BasicUser, windowsUA)
	linuxClient, linux := login(th.BasicUser2, linuxUA)
	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypeOpen)
	_, _, err := th.SystemAdminClient.AddChannelMember(th.Context.Context(), channel.Id, th.BasicUser2.Id)
	require.NoError(t, err)
	actions := []string{model.AccessControlPolicyActionUploadFileAttachment, downloadAction, model.AccessControlPolicyActionCreateBurnOnReadPost}
	draft := &model.AccessControlPolicy{Type: model.AccessControlPolicyTypePermission, Roles: []string{model.SystemUserRoleId}, Rules: []model.AccessControlPolicyRule{{Name: "linux-only", Actions: actions, Expression: `user.session.user_agent_os == "Linux"`}}}
	params := model.PolicySimulationByUsersParams{Policy: draft, Actions: actions, RuleName: "linux-only", EvaluationScope: model.PolicyEvaluationScopeThisRule, Users: []model.PolicySimulationUserOverride{{UserID: th.BasicUser.Id}, {UserID: th.BasicUser2.Id}}}
	check := func(t *testing.T, row model.PolicySimulationUserResult, allowed bool) {
		t.Helper()
		for _, action := range actions {
			require.Equal(t, allowed, row.Decisions[action].Decision, action)
		}
	}
	t.Run("different users have different operating systems", func(t *testing.T) {
		result := simulateViaAPI(t, th, th.SystemAdminClient, params)
		check(t, result.Results[0], false)
		check(t, result.Results[1], true)
		require.Equal(t, windows.Id, result.Results[0].Sessions[0].ID)
		require.Equal(t, "Windows 10", result.Results[0].Sessions[0].Attributes["user_agent_os"])
		require.Equal(t, linux.Id, result.Results[1].Sessions[0].ID)
		require.Equal(t, "Linux", result.Results[1].Sessions[0].Attributes["user_agent_os"])
	})
	t.Run("overrides affect only simulation", func(t *testing.T) {
		params.Users[0].SessionOverrides = map[string]any{"user_agent_os": "Linux"}
		check(t, simulateViaAPI(t, th, th.SystemAdminClient, params).Results[0], true)
		attrs, appErr := th.App.GetSessionAttributes(th.Context, windows.Id)
		require.Nil(t, appErr)
		require.Equal(t, "Windows 10", attrs["user_agent_os"])
		params.Users[0].SessionOverrides = nil
		check(t, simulateViaAPI(t, th, th.SystemAdminClient, params).Results[0], false)
	})
	t.Run("latest session headline and per device decisions", func(t *testing.T) {
		_, newer := login(th.BasicUser, linuxUA)
		require.NoError(t, th.Server.Store().Session().UpdateLastActivityAt(windows.Id, model.GetMillis()-10000))
		require.NoError(t, th.Server.Store().Session().UpdateLastActivityAt(newer.Id, model.GetMillis()))
		result := simulateViaAPI(t, th, th.SystemAdminClient, params).Results[0]
		check(t, result, true)
		require.Equal(t, newer.Id, result.Sessions[0].ID)
		var oldFound bool
		for _, session := range result.Sessions {
			if session.ID == windows.Id {
				oldFound = true
				require.False(t, session.Decisions[downloadAction].Decision)
			}
		}
		require.True(t, oldFound)
		require.NoError(t, th.Server.Store().Session().UpdateExpiresAt(newer.Id, model.GetMillis()-1))
		require.NoError(t, th.Server.Store().Session().UpdateLastActivityAt(windows.Id, model.GetMillis()))
		result = simulateViaAPI(t, th, th.SystemAdminClient, params).Results[0]
		check(t, result, false)
		for _, session := range result.Sessions {
			require.NotEqual(t, newer.Id, session.ID)
		}
	})
	t.Run("missing data is neutral not a device match", func(t *testing.T) {
		absent := th.CreateUser(t)
		params.Users = []model.PolicySimulationUserOverride{{UserID: absent.Id}}
		result := simulateViaAPI(t, th, th.SystemAdminClient, params).Results[0]
		for _, action := range actions {
			// Core maps unknown-session markers to its neutral rule-scoped
			// response. In all-policies mode the original marker survives.
			require.Equal(t, model.PolicySimulationBlameSourceNoApplicableRule, result.Decisions[action].Blame[0].Source)
		}
		require.Empty(t, result.Sessions[0].ID)
		require.Empty(t, result.Sessions[0].Attributes)
		allParams := params
		allParams.EvaluationScope = model.PolicyEvaluationScopeAll
		allResult := simulateViaAPI(t, th, th.SystemAdminClient, allParams).Results[0]
		for _, action := range actions {
			require.False(t, allResult.Decisions[action].Decision)
			require.Equal(t, model.PolicySimulationBlameSourceNoSessionData, allResult.Decisions[action].Blame[0].Source)
		}
		params.Users[0].SessionOverrides = map[string]any{"user_agent_os": "Windows"}
		check(t, simulateViaAPI(t, th, th.SystemAdminClient, params).Results[0], false)
		params.Users[0].SessionOverrides = map[string]any{"user_agent_os": "Linux"}
		check(t, simulateViaAPI(t, th, th.SystemAdminClient, params).Results[0], true)
	})
	t.Run("delegated admin cannot inspect target devices", func(t *testing.T) {
		params.ChannelID = channel.Id
		params.Users = []model.PolicySimulationUserOverride{{UserID: th.BasicUser2.Id}}
		result := simulateViaAPI(t, th, th.Client, params).Results[0]
		require.Empty(t, result.Sessions[0].ID)
		require.Empty(t, result.Sessions[0].Attributes)
		require.Equal(t, model.PolicySimulationBlameSourceNoApplicableRule, result.Decisions[downloadAction].Blame[0].Source)
	})
	t.Run("live evaluation still denies a Windows session", func(t *testing.T) {
		// Direct service evaluation uses the real stored session snapshot;
		// simulation overrides must not become live policy input.
		subject, appErr := th.App.BuildAccessControlSubjectForSession(th.Context.WithSession(windows), channel.Id)
		require.Nil(t, appErr)
		saveTestPolicy(t, th, &model.AccessControlPolicy{ID: channel.Id, Type: model.AccessControlPolicyTypeChannel, Rules: []model.AccessControlPolicyRule{{Name: "linux-files", Role: model.ChannelUserRoleId, Actions: []string{downloadAction, model.AccessControlPolicyActionUploadFileAttachment}, Expression: `user.session.user_agent_os == "Linux"`}}})
		decision, appErr := th.Server.Channels().AccessControl.AccessEvaluation(th.Context, model.AccessRequest{Subject: *subject, Resource: model.Resource{ID: channel.Id, Type: model.AccessControlPolicyTypeChannel}, Action: downloadAction})
		require.Nil(t, appErr)
		require.False(t, decision.Decision)
		// A normal HTTP call must still be authenticated after simulations.
		_, response, err := winClient.GetMe(th.Context.Context(), "")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		_, response, err = winClient.UploadFile(th.Context.Context(), []byte("Windows fixture"), channel.Id, "windows.txt")
		require.Error(t, err)
		require.Equal(t, http.StatusForbidden, response.StatusCode)
		files, _, err := linuxClient.UploadFile(th.Context.Context(), []byte("Linux fixture"), channel.Id, "linux.txt")
		require.NoError(t, err)
		require.Len(t, files.FileInfos, 1)
		_, _, err = linuxClient.CreatePost(th.Context.Context(), &model.Post{ChannelId: channel.Id, Message: "Synthetic session fixture", FileIds: []string{files.FileInfos[0].Id}})
		require.NoError(t, err)
		_, response, err = winClient.GetFile(th.Context.Context(), files.FileInfos[0].Id)
		require.Error(t, err)
		require.Equal(t, http.StatusForbidden, response.StatusCode)
		data, _, err := linuxClient.GetFile(th.Context.Context(), files.FileInfos[0].Id)
		require.NoError(t, err)
		require.Equal(t, "Linux fixture", string(data))
	})
}
