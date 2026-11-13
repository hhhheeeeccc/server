package channel_last_post

import (
	"slices"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestPreviewChannelPermissionsMatchNative(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	s := &Service{app: th.App}
	member, appErr := th.App.GetSession(th.Client.AuthToken)
	require.Nil(t, appErr)
	admin, appErr := th.App.GetSession(th.SystemAdminClient.AuthToken)
	require.Nil(t, appErr)
	public := th.CreateChannelWithClient(t, th.Client, model.ChannelTypeOpen)
	private := th.CreateChannelWithClient(t, th.Client, model.ChannelTypePrivate)
	otherClient := th.CreateClient()
	th.LoginBasic2WithClient(t, otherClient)
	otherPublic := th.CreateChannelWithClient(t, otherClient, model.ChannelTypeOpen)
	otherPrivate := th.CreateChannelWithClient(t, otherClient, model.ChannelTypePrivate)
	guest := th.CreateGuestUser(t)
	th.LinkUserToTeam(t, guest, th.BasicTeam)
	th.AddUserToChannel(t, guest, public)
	th.AddUserToChannel(t, guest, private)
	guestSession := &model.Session{UserId: guest.Id, Roles: model.SystemGuestRoleId}
	staleAdmin := member.DeepCopy()
	staleAdmin.Roles = model.SystemAdminRoleId
	contentRole, err := th.Store.Role().Save(&model.Role{
		Name: "review_content_" + model.NewId(), DisplayName: "Content session role",
		Permissions: []string{model.PermissionReadChannelContent.Id},
	})
	require.NoError(t, err)
	staleContent := member.DeepCopy()
	staleContent.Roles = contentRole.Name
	local := member.DeepCopy()
	local.Local = true
	all := []string{public.Id, private.Id, otherPublic.Id, otherPrivate.Id, model.NewId()}
	assertNative := func(t *testing.T, session *model.Session, ids []string, expected int) {
		t.Helper()
		rctx := th.Context.WithSession(session)
		want := make([]string, 0, len(ids))
		for _, id := range ids {
			channel, appErr := th.App.GetChannel(rctx, id)
			if appErr != nil {
				continue
			}
			if allowed, _ := th.App.SessionHasPermissionToReadChannel(rctx, *session, channel); allowed {
				want = append(want, id)
			}
		}
		require.Len(t, want, expected, "the native oracle must exercise both grants and denials")
		require.Equal(t, want, s.readablePreviewChannelIDs(rctx, ids))
	}
	for _, compliance := range []bool{false, true} {
		name := "public_fallback"
		memberCount := 3
		if compliance {
			name = "compliance"
			memberCount = 2
		}
		t.Run(name, func(t *testing.T) {
			th.App.UpdateConfig(func(cfg *model.Config) { cfg.ComplianceSettings.Enable = model.NewPointer(compliance) })
			for _, tc := range []struct {
				name     string
				session  *model.Session
				ids      []string
				expected int
			}{
				{"all_members", member, []string{public.Id, private.Id}, 2},
				{"mixed_membership", member, all, memberCount},
				{"single", member, []string{public.Id}, 1},
				{"guest_members", guestSession, []string{public.Id, private.Id}, 2},
				{"guest_mixed", guestSession, all, memberCount},
				{"administrator", admin, all, 4},
				{"stale_admin_session", staleAdmin, all, memberCount},
				{"stale_content_session", staleContent, all, memberCount},
				{"local_session", local, all, 4},
			} {
				t.Run(tc.name, func(t *testing.T) { assertNative(t, tc.session, tc.ids, tc.expected) })
			}
		})
	}

	// A cached positive batch result must never survive a subsequent role change.
	th.AddUserToChannel(t, th.BasicUser2, public)
	th.AddUserToChannel(t, th.BasicUser2, private)
	regularMember, appErr := th.App.GetSession(otherClient.AuthToken)
	require.Nil(t, appErr)
	assertNative(t, regularMember, []string{public.Id, private.Id}, 2)
	role, err := th.Store.Role().GetByName(th.Context, model.ChannelUserRoleId)
	require.NoError(t, err)
	original := role.Clone()
	t.Cleanup(func() { _, err := th.App.Srv().Store().Role().Save(original); require.NoError(t, err) })
	role.Permissions = slices.DeleteFunc(role.Permissions, func(id string) bool { return id == model.PermissionReadChannelContent.Id })
	_, err = th.App.Srv().Store().Role().Save(role)
	require.NoError(t, err)
	t.Run("revoked_channel_content", func(t *testing.T) { assertNative(t, regularMember, []string{public.Id, private.Id}, 0) })
}
