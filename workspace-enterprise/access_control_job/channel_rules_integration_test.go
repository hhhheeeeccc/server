package access_control_job

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/mattermost/enterprise/access_control"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func setupChannelRules(t *testing.T) (*api4.TestHelper, *Worker) {
	t.Helper()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		*cfg.AccessControlSettings.EnableAttributeBasedAccessControl = true
		cfg.FeatureFlags.PermissionPolicies = true
	}).InitBasic(t)
	require.True(t, th.App.Srv().SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	th.App.Srv().Channels().AccessControl = access_control.New(th.App)
	worker := NewChannel(th.App.Srv()).MakeWorker().(*Worker)
	th.App.Srv().Jobs.RegisterJobType(model.JobTypeAccessControlSync, worker, nil)
	return th, worker
}

func saveTestPolicy(t *testing.T, th *api4.TestHelper, policy *model.AccessControlPolicy) *model.AccessControlPolicy {
	t.Helper()
	if policy.ID == "" {
		policy.ID = model.NewId()
	}
	if policy.Name == "" {
		policy.Name = "test-policy-" + policy.ID
	}
	saved, appErr := th.App.Srv().Channels().AccessControl.SavePolicy(th.Context, policy)
	require.Nil(t, appErr)
	if policy.Type == model.AccessControlPolicyTypeChannel {
		th.App.Srv().Store().Channel().InvalidateChannel(policy.ID)
	}
	return saved
}

func requireMembership(t *testing.T, th *api4.TestHelper, channelID, userID string, want bool) {
	t.Helper()
	_, appErr := th.App.GetChannelMember(th.Context, channelID, userID)
	if want {
		require.Nil(t, appErr)
	} else {
		require.NotNil(t, appErr)
		require.Equal(t, http.StatusNotFound, appErr.StatusCode)
	}
}

func TestChannelRulesMembershipLifecycle(t *testing.T) {
	th, worker := setupChannelRules(t)
	for _, channelType := range []model.ChannelType{model.ChannelTypePrivate, model.ChannelTypeOpen} {
		for _, active := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/auto-add=%v", channelType, active), func(t *testing.T) {
				channel := th.CreateChannelWithClient(t, th.Client, channelType)
				_, appErr := th.App.AddChannelMember(th.Context, th.BasicUser2.Id, channel, app.ChannelMemberOpts{})
				require.Nil(t, appErr)
				candidate := th.CreateUser(t)
				th.LinkUserToTeam(t, candidate, th.BasicTeam)
				localExcluded := th.CreateUser(t)
				th.LinkUserToTeam(t, localExcluded, th.BasicTeam)
				parent := saveTestPolicy(t, th, &model.AccessControlPolicy{
					Type:  model.AccessControlPolicyTypeParent,
					Rules: []model.AccessControlPolicyRule{{Actions: []string{model.AccessControlPolicyActionMembership}, Expression: fmt.Sprintf("user.email != %q", th.BasicUser2.Email)}},
				})
				channelPolicy := &model.AccessControlPolicy{
					ID: channel.Id, Type: model.AccessControlPolicyTypeChannel, Imports: []string{parent.ID},
					Rules: []model.AccessControlPolicyRule{{Actions: []string{model.AccessControlPolicyActionMembership}, Expression: fmt.Sprintf("user.email != %q", localExcluded.Email)}},
				}
				if active {
					channelPolicy.SetAutoAddMode(model.AccessControlAutoAddAlways)
				}
				saveTestPolicy(t, th, channelPolicy)
				syncChannel := func() {
					t.Helper()
					job, appErr := th.App.Srv().Jobs.CreateJob(th.Context, model.JobTypeAccessControlSync, map[string]string{"channel_ids": channel.Id})
					require.Nil(t, appErr)
					require.False(t, worker.doJob(job))
					stored, err := th.App.Srv().Store().Job().Get(th.Context, job.Id)
					require.NoError(t, err)
					require.Equal(t, model.JobStatusSuccess, stored.Status, stored.Data)
					var results map[string]membershipSyncResult
					require.NoError(t, json.Unmarshal([]byte(stored.Data["sync_results"]), &results))
					require.Contains(t, results, channel.Id)
					if channelType == model.ChannelTypeOpen {
						require.Empty(t, results[channel.Id].MembersRemoved)
					}
					if !active {
						require.Empty(t, results[channel.Id].MembersAdded)
					}
				}
				syncChannel()
				requireMembership(t, th, channel.Id, th.BasicUser.Id, true)
				requireMembership(t, th, channel.Id, th.BasicUser2.Id, channelType == model.ChannelTypeOpen)
				requireMembership(t, th, channel.Id, candidate.Id, active)
				requireMembership(t, th, channel.Id, localExcluded.Id, false)
				if !active {
					return
				}
				parent.Rules[0].Expression += fmt.Sprintf(" && user.email != %q", candidate.Email)
				parent = saveTestPolicy(t, th, parent)
				syncChannel()
				requireMembership(t, th, channel.Id, candidate.Id, channelType == model.ChannelTypeOpen)
				parent.Rules[0].Expression = fmt.Sprintf("user.email != %q", th.BasicUser2.Email)
				saveTestPolicy(t, th, parent)
				syncChannel()
				requireMembership(t, th, channel.Id, candidate.Id, true)
			})
		}
	}
}

func TestPrivateMembershipExcludesGuestsWithoutBlockingFileRules(t *testing.T) {
	th, worker := setupChannelRules(t)
	guest := th.CreateGuestUser(t)
	guest, appErr := th.App.GetUser(th.Context, guest.Id)
	require.Nil(t, appErr)
	th.LinkUserToTeam(t, guest, th.BasicTeam)
	for _, channelType := range []model.ChannelType{model.ChannelTypePrivate, model.ChannelTypeOpen} {
		t.Run(string(channelType), func(t *testing.T) {
			channel := th.CreateChannelWithClient(t, th.Client, channelType)
			policy := &model.AccessControlPolicy{
				ID: channel.Id, Type: model.AccessControlPolicyTypeChannel,
				Rules: []model.AccessControlPolicyRule{
					{Actions: []string{model.AccessControlPolicyActionMembership}, Expression: "true"},
					{Name: "guest-download", Actions: []string{"download_file_attachment"}, Expression: "true", Role: model.ChannelGuestRoleId},
				},
			}
			policy.SetAutoAddMode(model.AccessControlAutoAddAlways)
			saveTestPolicy(t, th, policy)
			subject, appErr := th.App.BuildAccessControlSubject(th.Context, guest.Id, guest.Roles, channel.Id)
			require.Nil(t, appErr)
			service := th.App.Srv().Channels().AccessControl
			decision, appErr := service.AccessEvaluation(th.Context, model.AccessRequest{
				Subject: *subject, Resource: model.Resource{ID: channel.Id, Type: model.AccessControlPolicyTypeChannel}, Action: model.AccessControlPolicyActionMembership,
			})
			require.Nil(t, appErr)
			require.Equal(t, channelType == model.ChannelTypeOpen, decision.Decision)
			users, count, appErr := service.QueryUsersForResource(th.Context, channel.Id, model.AccessControlPolicyActionMembership, model.SubjectSearchOptions{SubjectID: guest.Id})
			require.Nil(t, appErr)
			if channelType == model.ChannelTypePrivate {
				require.Empty(t, users)
				require.Zero(t, count)
			}
			reason, err := worker.execute(th.Context, map[string]string{"channel_ids": channel.Id}, nil)
			require.NoError(t, err)
			require.Equal(t, continueWork, reason)
			requireMembership(t, th, channel.Id, guest.Id, channelType == model.ChannelTypeOpen)
			decision, appErr = service.AccessEvaluation(th.Context, model.AccessRequest{
				Subject: *subject, Resource: model.Resource{ID: channel.Id, Type: model.AccessControlPolicyTypeChannel}, Action: "download_file_attachment",
			})
			require.Nil(t, appErr)
			require.True(t, decision.Decision)
		})
	}
}

func TestDuplicateParentPolicyNameReturnsConflict(t *testing.T) {
	th, _ := setupChannelRules(t)
	first := saveTestPolicy(t, th, &model.AccessControlPolicy{
		Type:  model.AccessControlPolicyTypeParent,
		Rules: []model.AccessControlPolicyRule{{Actions: []string{model.AccessControlPolicyActionMembership}, Expression: "true"}},
	})
	duplicate := *first
	duplicate.ID = model.NewId()
	_, appErr := th.App.Srv().Channels().AccessControl.SavePolicy(th.Context, &duplicate)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusConflict, appErr.StatusCode)
	require.Equal(t, "app.pap.save_policy.name_exists.app_error", appErr.Id)
}

func TestTeamSyncReportsChangesRestoresMembershipAndChainsChannels(t *testing.T) {
	th, _ := setupChannelRules(t)
	team, appErr := th.App.GetTeam(th.BasicTeam.Id)
	require.Nil(t, appErr)
	team.AllowOpenInvite = false
	_, err := th.App.Srv().Store().Team().Update(team)
	require.NoError(t, err)
	for range 4 {
		th.LinkUserToTeam(t, th.CreateUser(t), team)
	}
	policy := saveTestPolicy(t, th, &model.AccessControlPolicy{
		ID: team.Id, Type: model.AccessControlPolicyTypeTeam,
		Rules: []model.AccessControlPolicyRule{{Actions: []string{model.AccessControlPolicyActionMembership}, Expression: fmt.Sprintf("user.email == %q", th.BasicUser.Email)}},
	})
	worker := NewTeam(th.App.Srv()).MakeWorker().(*Worker)
	th.App.Srv().Jobs.RegisterJobType(model.JobTypeAccessControlTeamSync, worker, nil)
	runTeamJob := func() (*model.Job, membershipSyncResult) {
		t.Helper()
		job, appErr := th.App.Srv().Jobs.CreateJob(th.Context, model.JobTypeAccessControlTeamSync, map[string]string{"policy_id": team.Id})
		require.Nil(t, appErr)
		require.False(t, worker.doJob(job))
		stored, err := th.App.Srv().Store().Job().Get(th.Context, job.Id)
		require.NoError(t, err)
		require.Equal(t, model.JobStatusSuccess, stored.Status, stored.Data)
		var results map[string]membershipSyncResult
		require.NoError(t, json.Unmarshal([]byte(stored.Data["sync_results"]), &results))
		require.Contains(t, results, team.Id)
		return stored, results[team.Id]
	}
	first, changes := runTeamJob()
	require.Contains(t, changes.MembersRemoved, th.BasicUser2.Id)
	require.Empty(t, changes.MembersAdded)
	require.True(t, changes.MassRemovalWarning)
	pending, err := th.App.Srv().Store().Job().GetByTypeAndData(th.Context, model.JobTypeAccessControlSync, map[string]string{"team_id": team.Id}, true, model.JobStatusPending)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, first.Id, pending[0].Data["parent_team_job_id"])

	policy.SetAutoAddMode(model.AccessControlAutoAddAlways)
	policy.Rules[0].Expression = "true"
	saveTestPolicy(t, th, policy)
	_, changes = runTeamJob()
	require.Contains(t, changes.MembersAdded, th.BasicUser2.Id, "soft-deleted team memberships must be restored")
	member, err := th.App.Srv().Store().Team().GetMember(th.Context, team.Id, th.BasicUser2.Id)
	require.NoError(t, err)
	require.Zero(t, member.DeleteAt)
	pending, err = th.App.Srv().Store().Job().GetByTypeAndData(th.Context, model.JobTypeAccessControlSync, map[string]string{"team_id": team.Id}, true, model.JobStatusPending)
	require.NoError(t, err)
	require.Len(t, pending, 1, "an existing pending channel sync must not be duplicated")
}
