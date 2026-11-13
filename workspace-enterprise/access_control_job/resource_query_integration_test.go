package access_control_job

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestResourceUserQueriesRefreshPolicyAndResourceDataBetweenCalls(t *testing.T) {
	th, _ := setupChannelRules(t)
	service := th.App.Srv().Channels().AccessControl
	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypeOpen)
	group, appErr := th.App.GetPropertyGroup(th.Context, model.AccessControlPropertyGroupName)
	require.Nil(t, appErr)
	adminContext := th.Context.WithContext(model.WithCallerID(th.Context.Context(), th.SystemAdminUser.Id))
	field, appErr := th.App.CreatePropertyField(adminContext, &model.PropertyField{
		GroupID: group.ID, Name: "f_" + model.NewId(), Type: model.PropertyFieldTypeText,
		ObjectType: model.PropertyFieldObjectTypeChannel, TargetType: string(model.PropertyFieldTargetLevelSystem),
		Attrs: model.StringInterface{model.PropertyFieldAttrManaged: "admin"},
	}, false, "")
	require.Nil(t, appErr)
	value, appErr := th.App.CreatePropertyValue(adminContext, &model.PropertyValue{
		GroupID: group.ID, FieldID: field.ID, TargetID: channel.Id,
		TargetType: model.PropertyValueTargetTypeChannel, Value: json.RawMessage(`"Engineering"`),
	})
	require.Nil(t, appErr)
	require.NoError(t, th.Store.Attributes().RefreshAttributes())
	prefix := "query-refresh-" + model.NewId()[:8] + "-"
	var expected []string
	for i := range 3 {
		externalID := model.NewId()
		user, err := th.Store.User().Save(th.Context, &model.User{
			Username: prefix + externalID, Email: prefix + externalID + "@example.test",
			AuthService: model.UserAuthServiceSaml, AuthData: &externalID,
			Roles: model.SystemUserRoleId, EmailVerified: i != 1,
		})
		require.NoError(t, err)
		if user.EmailVerified {
			expected = append(expected, user.Id)
		}
	}
	slices.Sort(expected)
	parent := saveTestPolicy(t, th, &model.AccessControlPolicy{
		Type:  model.AccessControlPolicyTypeParent,
		Rules: []model.AccessControlPolicyRule{{Actions: []string{model.AccessControlPolicyActionMembership}, Expression: "user.verified"}},
	})
	policy := saveTestPolicy(t, th, &model.AccessControlPolicy{
		ID: channel.Id, Type: model.AccessControlPolicyTypeChannel, Imports: []string{parent.ID},
		Rules: []model.AccessControlPolicyRule{{Actions: []string{model.AccessControlPolicyActionMembership},
			Expression: fmt.Sprintf("resource.attributes.%s == 'Engineering'", field.Name)}},
	})
	query := func() ([]*model.User, int64, *model.AppError) {
		return service.QueryUsersForResource(th.Context, channel.Id, model.AccessControlPolicyActionMembership, model.SubjectSearchOptions{Term: prefix})
	}
	check := func(want []string) {
		t.Helper()
		users, count, appErr := query()
		require.Nil(t, appErr)
		ids := make([]string, 0, len(users))
		for _, user := range users {
			ids = append(ids, user.Id)
		}
		require.Equal(t, len(want), int(count))
		require.Equal(t, want, ids)
	}
	check(expected)
	parent.Rules[0].Expression = "false"
	parent = saveTestPolicy(t, th, parent)
	check([]string{})
	parent.Rules[0].Expression = "user.verified"
	saveTestPolicy(t, th, parent)
	check(expected)
	value.Value = json.RawMessage(`"Legal"`)
	_, err := th.Store.PropertyValue().Update(group.ID, []*model.PropertyValue{value})
	require.NoError(t, err)
	require.NoError(t, th.Store.Attributes().RefreshAttributes())
	check([]string{})
	value.Value = json.RawMessage(`"Engineering"`)
	_, err = th.Store.PropertyValue().Update(group.ID, []*model.PropertyValue{value})
	require.NoError(t, err)
	require.NoError(t, th.Store.Attributes().RefreshAttributes())
	check(expected)

	// Concurrent calls on the same service/context must own independent maps.
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			users, count, appErr := query()
			if appErr != nil || len(users) != len(expected) || count != int64(len(expected)) {
				t.Errorf("concurrent query: users=%d count=%d error=%v", len(users), count, appErr)
			}
		})
	}
	wg.Wait()

	// A missing import fails closed, but remains unread when a local rule denies
	// first or no users are candidates. Use the store to simulate a lost row.
	require.NoError(t, th.Store.AccessControlPolicy().Delete(th.Context, parent.ID))
	users, count, appErr := query()
	require.NotNil(t, appErr)
	require.Nil(t, users)
	require.Zero(t, count)
	policy.Rules[0].Expression = "false"
	_, err = th.Store.AccessControlPolicy().Save(th.Context, policy)
	require.NoError(t, err)
	check([]string{})
	policy.Rules[0].Expression = "true"
	_, err = th.Store.AccessControlPolicy().Save(th.Context, policy)
	require.NoError(t, err)
	users, count, appErr = service.QueryUsersForResource(th.Context, channel.Id, model.AccessControlPolicyActionMembership, model.SubjectSearchOptions{SubjectID: model.NewId()})
	require.Nil(t, appErr)
	require.Empty(t, users)
	require.Zero(t, count)
}

func TestResourceUserQueriesMatchIndividualPermissionDecisions(t *testing.T) {
	th, _ := setupChannelRules(t)
	service := th.App.Srv().Channels().AccessControl
	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypePrivate)
	guest := th.CreateGuestUser(t)
	th.LinkUserToTeam(t, guest, th.BasicTeam)
	saveTestPolicy(t, th, &model.AccessControlPolicy{
		ID: channel.Id, Type: model.AccessControlPolicyTypeChannel,
		Rules: []model.AccessControlPolicyRule{{Name: "channel-download", Actions: []string{downloadAction}, Expression: "true"}},
	})
	permission := saveTestPolicy(t, th, &model.AccessControlPolicy{
		Type: model.AccessControlPolicyTypePermission, Active: true, Roles: []string{model.SystemUserRoleId},
		Rules: []model.AccessControlPolicyRule{{Name: "restrict-download", Actions: []string{downloadAction},
			Expression: fmt.Sprintf("user.email != %q", th.BasicUser2.Email)}},
	})
	opts := model.SubjectSearchOptions{TeamID: th.BasicTeam.Id}
	candidates, _, err := th.Store.Attributes().SearchUsers(th.Context, opts)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(candidates), 3)
	for _, active := range []bool{true, false, true} {
		permission.Active = active
		permission = saveTestPolicy(t, th, permission)
		for _, action := range []string{downloadAction, model.AccessControlPolicyActionMembership, model.AccessControlPolicyActionUploadFileAttachment} {
			var expected []string
			for _, user := range candidates {
				subject, appErr := th.App.BuildAccessControlSubject(th.Context, user.Id, user.Roles, channel.Id)
				require.Nil(t, appErr)
				decision, appErr := service.AccessEvaluation(th.Context, model.AccessRequest{
					Subject: *subject, Resource: model.Resource{ID: channel.Id, Type: model.AccessControlPolicyTypeChannel}, Action: action,
				})
				require.Nil(t, appErr)
				if decision.Decision && !decision.IsNoPolicy() {
					expected = append(expected, user.Id)
				}
			}
			users, count, appErr := service.QueryUsersForResource(th.Context, channel.Id, action, opts)
			require.Nil(t, appErr)
			var ids []string
			for _, user := range users {
				ids = append(ids, user.Id)
			}
			require.EqualValues(t, len(expected), count)
			require.Equal(t, expected, ids, "active=%v action=%s", active, action)
			if action == downloadAction {
				require.Contains(t, ids, guest.Id, "a system-user-only permission policy does not restrict a guest's file rule")
				require.Equal(t, !active, slices.Contains(ids, th.BasicUser2.Id))
			}
		}
	}
	th.Server.SetLicense(nil)
	users, count, appErr := service.QueryUsersForResource(th.Context, channel.Id, downloadAction, opts)
	require.NotNil(t, appErr)
	require.Nil(t, users)
	require.Zero(t, count)
}
