package access_control_job

import (
	"slices"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestUserPolicyQueriesAcrossCandidatePages(t *testing.T) {
	th, _ := setupChannelRules(t)
	service := th.App.Srv().Channels().AccessControl
	var expected []string
	for i := 0; i < 205; i++ {
		externalID := model.NewId()
		user, err := th.App.Srv().Store().User().Save(th.Context, &model.User{
			Username: "paging-" + externalID, Email: "paging-" + externalID + "@example.test",
			AuthService: model.UserAuthServiceSaml, AuthData: &externalID,
			Roles: model.SystemUserRoleId, EmailVerified: i%3 == 0,
		})
		require.NoError(t, err)
		if user.EmailVerified {
			expected = append(expected, user.Id)
		}
	}
	slices.Sort(expected)
	expression := `user.email.startsWith("paging-") && user.verified`
	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypeOpen)
	saveTestPolicy(t, th, &model.AccessControlPolicy{
		ID: channel.Id, Type: model.AccessControlPolicyTypeChannel,
		Rules: []model.AccessControlPolicyRule{{Actions: []string{model.AccessControlPolicyActionMembership}, Expression: expression}},
	})
	queries := map[string]func(model.SubjectSearchOptions) ([]*model.User, int64, *model.AppError){
		"expression": func(opts model.SubjectSearchOptions) ([]*model.User, int64, *model.AppError) {
			return service.QueryUsersForExpression(th.Context, expression, opts)
		},
		"resource": func(opts model.SubjectSearchOptions) ([]*model.User, int64, *model.AppError) {
			return service.QueryUsersForResource(th.Context, channel.Id, model.AccessControlPolicyActionMembership, opts)
		},
	}
	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			for _, ignoreCount := range []bool{false, true} {
				var collected []string
				opts := model.SubjectSearchOptions{Limit: 17, IgnoreCount: ignoreCount}
				for page := 0; page < 10; page++ {
					users, total, appErr := query(opts)
					require.Nil(t, appErr)
					if ignoreCount {
						require.Zero(t, total)
					} else {
						require.EqualValues(t, len(expected), total, "count includes matches before the result cursor")
					}
					if len(users) == 0 {
						break
					}
					for _, user := range users {
						require.True(t, strings.HasPrefix(user.Email, "paging-"))
						require.True(t, user.EmailVerified)
						collected = append(collected, user.Id)
					}
					opts.Cursor.TargetID = users[len(users)-1].Id
				}
				require.Equal(t, expected, collected, "no skipped or duplicate users at SQL or result page boundaries")
			}
		})
	}
}
