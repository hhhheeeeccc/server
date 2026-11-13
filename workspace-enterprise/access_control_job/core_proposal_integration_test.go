//go:build abac_core_proposal

package access_control_job

import (
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

// Run only with the reviewed Go overlay; these tests describe the two pending
// core corrections without changing or silently overlaying a production build.
func TestProposedCoreSelfExclusionFollowsMembershipPrivacy(t *testing.T) {
	th, _ := setupChannelRules(t)
	for _, tc := range []struct {
		name        string
		channelType model.ChannelType
		action      string
		wantStatus  int
	}{
		{"public membership is advisory", model.ChannelTypeOpen, model.AccessControlPolicyActionMembership, http.StatusOK},
		{"private membership cannot exclude its editor", model.ChannelTypePrivate, model.AccessControlPolicyActionMembership, http.StatusForbidden},
		{"file restrictions do not remove the editor", model.ChannelTypePrivate, downloadAction, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := th.CreateChannelWithClient(t, th.Client, tc.channelType)
			rule := model.AccessControlPolicyRule{Name: "rule", Actions: []string{tc.action}, Expression: "false"}
			if tc.action == downloadAction {
				rule.Role = model.ChannelUserRoleId
			}
			_, response, err := th.Client.CreateAccessControlPolicy(th.Context.Context(), &model.AccessControlPolicy{
				ID: channel.Id, Type: model.AccessControlPolicyTypeChannel, Rules: []model.AccessControlPolicyRule{rule},
			})
			if tc.wantStatus == http.StatusOK {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, tc.wantStatus, response.StatusCode)
		})
	}
	for _, public := range []bool{true, false} {
		name := "private team cannot exclude its editor"
		if public {
			name = "public team membership is advisory"
		}
		t.Run(name, func(t *testing.T) {
			team := th.CreateTeam(t)
			_, _, err := th.Client.PatchTeam(th.Context.Context(), team.Id, &model.TeamPatch{AllowOpenInvite: model.NewPointer(public)})
			require.NoError(t, err)
			_, response, err := th.Client.CreateAccessControlPolicy(th.Context.Context(), &model.AccessControlPolicy{
				ID: team.Id, Type: model.AccessControlPolicyTypeTeam,
				Rules: []model.AccessControlPolicyRule{{Name: "membership", Actions: []string{model.AccessControlPolicyActionMembership}, Expression: "false"}},
			})
			if public {
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.StatusCode)
			} else {
				require.Error(t, err)
				require.Equal(t, http.StatusBadRequest, response.StatusCode)
			}
		})
	}
}
