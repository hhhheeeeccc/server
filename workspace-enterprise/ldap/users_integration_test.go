package ldap

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"testing"
	"time"

	ldapclient "github.com/mattermost/ldap"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestLDAPCanceledGroupPlanKeepsExistingMembership(t *testing.T) {
	th, _ := setupDirectoryAPI(t)
	group, appErr := th.App.CreateGroup(&model.Group{DisplayName: "Canceled plan", Source: model.GroupSourceLdap, RemoteId: new("group-cancel")})
	require.Nil(t, appErr)
	_, appErr = th.App.UpsertGroupMember(group.Id, th.BasicUser.Id)
	require.Nil(t, appErr)
	ctx, cancel := context.WithCancel(th.Context.Context())
	cancel()
	plan := []groupMembershipPlan{{
		id: group.Id, previous: map[string]bool{th.BasicUser.Id: true}, next: map[string]bool{},
	}}
	appErr = th.App.Ldap().(*Service).applyGroupMemberships(th.Context.WithContext(ctx), plan)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusRequestTimeout, appErr.StatusCode)
	members, appErr := th.App.GetGroupMemberUsers(group.Id)
	require.Nil(t, appErr)
	require.Len(t, members, 1)
	require.Equal(t, th.BasicUser.Id, members[0].Id)
}

func (f *testDirectory) setUser(t *testing.T, attrs map[string][]string) {
	t.Helper()
	f.entriesMu.Lock()
	defer f.entriesMu.Unlock()
	groups := f.entries[:3:3]
	f.entries = append(groups, ldapclient.NewEntry("uid=one,dc=example,dc=test", attrs))
}

func userFixture() map[string][]string {
	return map[string][]string{"objectClass": {"person"}, "uid": {"one"}, "entryUUID": {"user-1"}, "mail": {"one@example.test"}, "givenName": {"First"}}
}

func TestLDAPLoginCreatesOnlyAfterBindAndKeepsStableIdentity(t *testing.T) {
	th, f := setupDirectoryAPI(t)
	f.setUser(t, userFixture())
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.LdapSettings.FirstNameAttribute = new("givenName") })
	s := th.App.Ldap()
	u, appErr := s.GetUser(th.Context, "one")
	require.Nil(t, appErr)
	require.Empty(t, u.Id)
	_, appErr = s.DoLogin(th.Context, *u.AuthData, "wrong")
	require.NotNil(t, appErr)
	require.Equal(t, "ent.ldap.do_login.invalid_password.app_error", appErr.Id)
	_, err := th.App.GetUserByAuth(u.AuthData, model.UserAuthServiceLdap)
	require.NotNil(t, err, "invalid credentials must not create an account")
	_, appErr = s.DoLogin(th.Context, *u.AuthData, "")
	require.NotNil(t, appErr)
	_, appErr = s.GetUser(th.Context, "*)(uid=*)")
	require.NotNil(t, appErr, "LDAP filter input must be escaped")

	client := th.CreateClient()
	loggedIn, _, loginErr := client.Login(th.Context.Context(), "one", "user-password")
	require.NoError(t, loginErr)
	require.Equal(t, "First", loggedIn.FirstName)
	id := loggedIn.Id
	require.Equal(t, model.UserAuthServiceLdap, loggedIn.AuthService)
	attrs := userFixture()
	attrs["uid"], attrs["mail"], attrs["givenName"] = []string{"renamed"}, []string{"new@example.test"}, []string{"Updated"}
	f.setUser(t, attrs)
	loggedIn, appErr = s.DoLogin(th.Context, "user-1", "user-password")
	require.Nil(t, appErr)
	require.Equal(t, id, loggedIn.Id)
	require.Equal(t, "renamed", loggedIn.Username)
	require.Equal(t, "new@example.test", loggedIn.Email)
	require.True(t, loggedIn.EmailVerified)
	require.Equal(t, "Updated", loggedIn.FirstName)
	got, appErr := s.GetUserAttributes(th.Context, "user-1", []string{"givenName", "mail"})
	require.Nil(t, appErr)
	require.Equal(t, "Updated", got["givenName"])
	gotUser, dn, appErr := s.GetLDAPUserForMMUser(th.Context, loggedIn)
	require.Nil(t, appErr)
	require.Equal(t, "uid=one,dc=example,dc=test", dn)
	require.Equal(t, loggedIn.Username, gotUser.Username)
	require.Equal(t, "email", s.CheckProviderAttributes(th.Context, &th.App.Config().LdapSettings, loggedIn, &model.UserPatch{Email: new("other@example.test")}))
	th.Server.SetLicense(nil)
	before := f.binds.Load()
	_, appErr = s.DoLogin(th.Context, "user-1", "user-password")
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusForbidden, appErr.StatusCode)
	require.Equal(t, before, f.binds.Load())
}

func TestLDAPDoesNotClaimExistingEmailOrUsername(t *testing.T) {
	th, f := setupDirectoryAPI(t)
	attrs := userFixture()
	attrs["mail"] = []string{th.BasicUser.Email}
	f.setUser(t, attrs)
	_, appErr := th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
	require.NotNil(t, appErr)
	unchanged, appErr := th.App.GetUser(th.Context, th.BasicUser.Id)
	require.Nil(t, appErr)
	require.Empty(t, unchanged.AuthService)
	f.setUser(t, userFixture())
	appErr = th.App.Ldap().SwitchToLdap(th.Context, th.BasicUser.Id, "one", "wrong")
	require.NotNil(t, appErr)
	appErr = th.App.Ldap().SwitchToLdap(th.Context, th.BasicUser.Id, "one", "user-password")
	require.Nil(t, appErr)
	unchanged, appErr = th.App.GetUser(th.Context, th.BasicUser.Id)
	require.Nil(t, appErr)
	require.Equal(t, model.UserAuthServiceLdap, unchanged.AuthService)
	require.Equal(t, "user-1", *unchanged.AuthData)
	appErr = th.App.Ldap().SwitchToLdap(th.Context, th.BasicUser2.Id, "one", "user-password")
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusConflict, appErr.StatusCode)
}

func TestLDAPSynchronizationFailureAndDeactivation(t *testing.T) {
	th, f := setupDirectoryAPI(t)
	f.setUser(t, userFixture())
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.LdapSettings.EnableSync = new(true) })
	client := th.CreateClient()
	u, _, err := client.Login(th.Context.Context(), "one", "user-password")
	require.NoError(t, err)
	s := th.App.Ldap().(*Service)
	f.searchError.Store(ldapclient.LDAPResultSizeLimitExceeded)
	require.NotNil(t, s.synchronize(th.Context, false))
	stored, appErr := th.App.GetUser(th.Context, u.Id)
	require.Nil(t, appErr)
	require.Zero(t, stored.DeleteAt, "partial search must never deactivate accounts")
	f.searchError.Store(0)
	attrs := userFixture()
	attrs["userAccountControl"] = []string{"514"}
	f.setUser(t, attrs)
	require.Nil(t, s.synchronize(th.Context, false))
	stored, appErr = th.App.GetUser(th.Context, u.Id)
	require.Nil(t, appErr)
	require.Positive(t, stored.DeleteAt)
	_, _, err = client.GetMe(th.Context.Context(), "")
	require.Error(t, err, "deactivation revokes sessions")
	f.setUser(t, userFixture())
	require.Nil(t, s.synchronize(th.Context, false))
	stored, appErr = th.App.GetUser(th.Context, u.Id)
	require.Nil(t, appErr)
	require.Zero(t, stored.DeleteAt)
	job, appErr := s.StartSynchronizeJob(th.Context, true)
	require.Nil(t, appErr)
	require.Equal(t, model.JobStatusSuccess, job.Status)
	f.searchError.Store(ldapclient.LDAPResultSizeLimitExceeded)
	job, appErr = s.StartSynchronizeJob(th.Context, true)
	require.NotNil(t, appErr)
	require.Equal(t, model.JobStatusError, job.Status)
	f.searchError.Store(0)
	f.entriesMu.Lock()
	f.entries = append(f.entries, ldapclient.NewEntry("uid=duplicate,dc=example,dc=test", map[string][]string{
		"objectClass": {"person"}, "entryUUID": {"user-1"}, "userAccountControl": {"514"}}))
	f.entriesMu.Unlock()
	_, appErr = s.GetAllLdapUsers(th.Context)
	require.NotNil(t, appErr, "an ID shared by an active and a disabled entry is ambiguous")
}

func TestLDAPSyncRejectsDuplicateDirectoryIdentityBeforeUpdatingUsers(t *testing.T) {
	for _, duplicate := range []string{"mail", "uid"} {
		t.Run(duplicate, func(t *testing.T) {
			th, directory := setupDirectoryAPI(t)
			th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.LdapSettings.EnableSync = new(true)
				cfg.LdapSettings.FirstNameAttribute = new("givenName")
			})
			first := userFixture()
			directory.setUser(t, first)
			user, appErr := th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
			require.Nil(t, appErr)
			require.Equal(t, "First", user.FirstName)

			secondID := "user-2"
			_, err := th.App.Srv().Store().User().Save(th.Context, &model.User{
				Username: "two", Email: "two@example.test", AuthService: model.UserAuthServiceLdap,
				AuthData: &secondID, Roles: model.SystemUserRoleId,
			})
			require.NoError(t, err)

			first["givenName"] = []string{"Changed"}
			directory.setUser(t, first)
			second := map[string][]string{
				"objectClass": {"person"}, "uid": {"two"}, "entryUUID": {"user-2"},
				"mail": {"two@example.test"}, "givenName": {"Second"},
			}
			second[duplicate] = first[duplicate]
			directory.entriesMu.Lock()
			directory.entries = append(directory.entries, ldapclient.NewEntry("uid=two,dc=example,dc=test", second))
			directory.entriesMu.Unlock()

			appErr = th.App.Ldap().(*Service).synchronize(th.Context, false)
			require.NotNil(t, appErr)
			require.Equal(t, http.StatusConflict, appErr.StatusCode)
			stored, appErr := th.App.GetUser(th.Context, user.Id)
			require.Nil(t, appErr)
			require.Equal(t, "First", stored.FirstName)
		})
	}
}

func TestLDAPSyncRejectsExistingLocalIdentityBeforeUpdatingUsers(t *testing.T) {
	for _, collision := range []string{"mail", "uid"} {
		t.Run(collision, func(t *testing.T) {
			th, directory := setupDirectoryAPI(t)
			th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.LdapSettings.EnableSync = new(true)
				cfg.LdapSettings.FirstNameAttribute = new("givenName")
			})
			first := userFixture()
			directory.setUser(t, first)
			user, appErr := th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
			require.Nil(t, appErr)

			secondID := "user-2"
			secondLocal, err := th.App.Srv().Store().User().Save(th.Context, &model.User{
				Username: "two", Email: "two@example.test", AuthService: model.UserAuthServiceLdap,
				AuthData: &secondID, Roles: model.SystemUserRoleId,
			})
			require.NoError(t, err)

			first["givenName"] = []string{"Changed"}
			directory.setUser(t, first)
			second := map[string][]string{
				"objectClass": {"person"}, "uid": {"two"}, "entryUUID": {"user-2"},
				"mail": {"two@example.test"}, "givenName": {"Second"},
			}
			if collision == "mail" {
				second["mail"] = []string{th.BasicUser.Email}
			} else {
				second["uid"] = []string{th.BasicUser.Username}
			}
			directory.entriesMu.Lock()
			directory.entries = append(directory.entries, ldapclient.NewEntry("uid=two,dc=example,dc=test", second))
			directory.entriesMu.Unlock()

			appErr = th.App.Ldap().(*Service).synchronize(th.Context, false)
			require.NotNil(t, appErr)
			require.Equal(t, http.StatusConflict, appErr.StatusCode)
			storedFirst, appErr := th.App.GetUser(th.Context, user.Id)
			require.Nil(t, appErr)
			require.Equal(t, "First", storedFirst.FirstName)
			storedSecond, appErr := th.App.GetUser(th.Context, secondLocal.Id)
			require.Nil(t, appErr)
			require.Equal(t, secondLocal.Username, storedSecond.Username)
			require.Equal(t, secondLocal.Email, storedSecond.Email)
		})
	}
}

func TestLDAPGroupsFirstLoginAndNestedMembership(t *testing.T) {
	th, f := setupDirectoryAPI(t)
	f.setUser(t, userFixture())
	f.entriesMu.Lock()
	f.entries[0] = ldapclient.NewEntry("cn=alpha,dc=example,dc=test", map[string][]string{
		"objectClass": {"groupOfNames"}, "cn": {"Alpha"}, "entryUUID": {"group-a"}, "member": {"cn=beta,dc=example,dc=test"}})
	f.entries[1] = ldapclient.NewEntry("cn=beta,dc=example,dc=test", map[string][]string{
		"objectClass": {"groupOfNames"}, "cn": {"Beta"}, "entryUUID": {"group-b"}, "member": {"uid=one,dc=example,dc=test", "cn=alpha,dc=example,dc=test"}})
	f.entriesMu.Unlock()
	group, appErr := th.App.CreateGroup(&model.Group{DisplayName: "Alpha", Source: model.GroupSourceLdap, RemoteId: new("group-a")})
	require.Nil(t, appErr)
	_, appErr = th.App.UpsertGroupSyncable(model.NewGroupTeam(group.Id, th.BasicTeam.Id, true))
	require.Nil(t, appErr)
	_, appErr = th.App.UpsertGroupSyncable(model.NewGroupChannel(group.Id, th.BasicChannel.Id, true))
	require.Nil(t, appErr)
	u, appErr := th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
	require.Nil(t, appErr)
	members, appErr := th.App.GetGroupMemberUsers(group.Id)
	require.Nil(t, appErr)
	require.Len(t, members, 1)
	require.Equal(t, u.Id, members[0].Id)
	_, appErr = th.App.GetTeamMember(th.Context, th.BasicTeam.Id, u.Id)
	require.Nil(t, appErr)
	_, appErr = th.App.GetChannelMember(th.Context, th.BasicChannel.Id, u.Id)
	require.Nil(t, appErr)
	f.entriesMu.Lock()
	f.entries[1] = ldapclient.NewEntry("cn=beta,dc=example,dc=test", map[string][]string{
		"objectClass": {"groupOfNames"}, "cn": {"Beta"}, "entryUUID": {"group-b"}, "member;range=0-1499": {"uid=one,dc=example,dc=test"}})
	f.entriesMu.Unlock()
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.LdapSettings.EnableSync = new(true) })
	require.NotNil(t, th.App.Ldap().(*Service).synchronize(th.Context, false))
	members, appErr = th.App.GetGroupMemberUsers(group.Id)
	require.Nil(t, appErr)
	require.Len(t, members, 1)
	f.entriesMu.Lock()
	f.entries[1] = ldapclient.NewEntry("cn=beta,dc=example,dc=test", map[string][]string{
		"objectClass": {"groupOfNames"}, "cn": {"Beta"}, "entryUUID": {"group-b"},
		"member;range=0-1": {"cn=alpha,dc=example,dc=test", "cn=gamma,dc=example,dc=test"},
	})
	f.rangeReplies["cn=beta,dc=example,dc=test|member;range=2-*"] = ldapclient.NewEntry("cn=beta,dc=example,dc=test", map[string][]string{
		"member;range=2-*": {"uid=one,dc=example,dc=test"},
	})
	f.entriesMu.Unlock()
	_, appErr = th.App.DeleteGroupMember(group.Id, u.Id)
	require.Nil(t, appErr)
	require.Nil(t, th.App.Ldap().(*Service).synchronize(th.Context, false))
	members, appErr = th.App.GetGroupMemberUsers(group.Id)
	require.Nil(t, appErr)
	require.Len(t, members, 1, "the member from the final AD range must be restored")
	require.Nil(t, th.App.UpdateUserActive(th.Context, u.Id, false))
	f.entriesMu.Lock()
	f.entries[1] = ldapclient.NewEntry("cn=beta,dc=example,dc=test", map[string][]string{
		"objectClass": {"groupOfNames"}, "cn": {"Beta"}, "entryUUID": {"group-b"}})
	f.entriesMu.Unlock()
	require.Nil(t, th.App.Ldap().(*Service).synchronize(th.Context, false))
	members, appErr = th.App.GetGroupMemberUsers(group.Id)
	require.Nil(t, appErr)
	require.Empty(t, members)
	u, appErr = th.App.GetUser(th.Context, u.Id)
	require.Nil(t, appErr)
	require.Zero(t, u.DeleteAt, "reactivation must not restore the old group membership")
}

func TestLDAPGroupMemberUIDAndUniqueMemberReachParent(t *testing.T) {
	th, directory := setupDirectoryAPI(t)
	directory.setUser(t, userFixture())
	directory.entriesMu.Lock()
	directory.entries[0] = ldapclient.NewEntry("cn=alpha,dc=example,dc=test", map[string][]string{
		"objectClass": {"groupOfNames"}, "cn": {"Alpha"}, "entryUUID": {"group-a"},
		"member": {"cn=beta,dc=example,dc=test", "cn=gamma,dc=example,dc=test"},
	})
	directory.entries[1] = ldapclient.NewEntry("cn=beta,dc=example,dc=test", map[string][]string{
		"objectClass": {"groupOfNames"}, "cn": {"Beta"}, "entryUUID": {"group-b"}, "memberUid": {"one"},
	})
	directory.entries[2] = ldapclient.NewEntry("cn=gamma,dc=example,dc=test", map[string][]string{
		"objectClass": {"groupOfNames"}, "cn": {"Gamma"}, "entryUUID": {"group-c"},
		"uniqueMember": {"UID=one,DC=example,DC=test"},
	})
	directory.entriesMu.Unlock()
	var groups []*model.Group
	for _, item := range []struct{ name, remoteID string }{{"Alpha", "group-a"}, {"Beta", "group-b"}, {"Gamma", "group-c"}} {
		group, appErr := th.App.CreateGroup(&model.Group{DisplayName: item.name, Source: model.GroupSourceLdap, RemoteId: &item.remoteID})
		require.Nil(t, appErr)
		groups = append(groups, group)
	}
	user, appErr := th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
	require.Nil(t, appErr)
	for _, group := range groups {
		members, err := th.App.GetGroupMemberUsers(group.Id)
		require.Nil(t, err)
		require.Len(t, members, 1, group.DisplayName)
		require.Equal(t, user.Id, members[0].Id)
	}
	directory.entriesMu.Lock()
	directory.entries[1] = ldapclient.NewEntry("cn=beta,dc=example,dc=test", map[string][]string{
		"objectClass": {"groupOfNames"}, "cn": {"Beta"}, "entryUUID": {"group-b"},
	})
	directory.entries[2] = ldapclient.NewEntry("cn=gamma,dc=example,dc=test", map[string][]string{
		"objectClass": {"groupOfNames"}, "cn": {"Gamma"}, "entryUUID": {"group-c"},
	})
	directory.entriesMu.Unlock()
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.LdapSettings.EnableSync = new(true) })
	require.Nil(t, th.App.Ldap().(*Service).synchronize(th.Context, false))
	for _, group := range groups {
		members, err := th.App.GetGroupMemberUsers(group.Id)
		require.Nil(t, err)
		require.Empty(t, members, "stale membership in "+group.DisplayName)
	}
}

func TestLDAPAdminFilterAndMFACannotBeBypassed(t *testing.T) {
	th, f := setupDirectoryAPI(t)
	f.setUser(t, userFixture())
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.LdapSettings.EnableAdminFilter = new(true)
		cfg.LdapSettings.AdminFilter = new("(uid=one)")
		cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
	})
	u, appErr := th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
	require.Nil(t, appErr)
	require.True(t, u.IsSystemAdmin())
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.LdapSettings.AdminFilter = new("(uid=other)") })
	_, appErr = th.App.Ldap().DoLogin(th.Context, "user-1", "wrong")
	require.NotNil(t, appErr)
	u, appErr = th.App.GetUser(th.Context, u.Id)
	require.Nil(t, appErr)
	require.True(t, u.IsSystemAdmin(), "wrong password must not update roles")
	u, appErr = th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
	require.Nil(t, appErr)
	require.False(t, u.IsSystemAdmin())
	require.NoError(t, th.App.Srv().Store().User().UpdateMfaSecret(u.Id, "JBSWY3DPEHPK3PXP"))
	require.NoError(t, th.App.Srv().Store().User().UpdateMfaActive(u.Id, true))
	th.App.InvalidateCacheForUser(u.Id)
	client := th.CreateClient()
	_, _, err := client.Login(th.Context.Context(), "one", "user-password")
	require.Error(t, err, "password-only login must still require MFA")
	require.Empty(t, client.AuthToken)
}

func TestLDAPGuestAndProfilePictureSynchronization(t *testing.T) {
	th, f := setupDirectoryAPI(t)
	attrs := userFixture()
	var imageData bytes.Buffer
	require.NoError(t, png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 32, 32))))
	attrs["jpegPhoto"] = []string{imageData.String()}
	f.setUser(t, attrs)
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.GuestAccountsSettings.Enable = new(true)
		cfg.LdapSettings.GuestFilter = new("(uid=one)")
		cfg.LdapSettings.PictureAttribute = new("jpegPhoto")
	})
	u, appErr := th.App.Ldap().DoLogin(th.Context, "user-1", "user-password")
	require.Nil(t, appErr)
	require.True(t, u.IsGuest())
	th.App.Ldap().UpdateProfilePictureIfNecessary(th.Context, *u, model.Session{})
	u, appErr = th.App.GetUser(th.Context, u.Id)
	require.Nil(t, appErr)
	require.Positive(t, u.LastPictureUpdate)
	f.setUser(t, userFixture())
	th.App.Ldap().UpdateProfilePictureIfNecessary(th.Context, *u, model.Session{})
	u, appErr = th.App.GetUser(th.Context, u.Id)
	require.Nil(t, appErr)
	require.LessOrEqual(t, u.LastPictureUpdate, int64(0))
}

func TestLDAPSchedulerUsesConfigurationAndPendingJobs(t *testing.T) {
	th, _ := setupDirectoryAPI(t)
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.LdapSettings.EnableSync = new(true)
		cfg.LdapSettings.SyncIntervalMinutes = new(7)
	})
	j := NewSyncJob(th.App).MakeScheduler()
	now := time.Now()
	require.Equal(t, now.Add(7*time.Minute), *j.NextScheduleTime(th.App.Config(), now, false, nil))
	require.Nil(t, j.NextScheduleTime(th.App.Config(), now, true, nil))
}

func TestLDAPSyncJobCancellationStopsDirectorySearch(t *testing.T) {
	th, f := setupDirectoryAPI(t)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.LdapSettings.EnableSync = new(true) })
	f.stall.Store(true)
	job, appErr := th.App.Srv().Jobs.CreateJob(th.Context, model.JobTypeLdapSync, nil)
	require.Nil(t, appErr)
	worker := NewSyncJob(th.App).MakeWorker().(*syncWorker)
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.DoJob(job)
	}()
	require.Eventually(t, func() bool { return f.searches.Load() > 0 }, 5*time.Second, 10*time.Millisecond)
	require.Nil(t, th.App.CancelJob(th.Context, job.Id))
	require.Eventually(t, func() bool {
		current, err := th.App.Srv().Jobs.GetJob(th.Context, job.Id)
		return err == nil && current.Status == model.JobStatusCanceled
	}, 5*time.Second, 25*time.Millisecond)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("LDAP sync did not stop after cancellation")
	}
}

func TestLDAPSyncWorkerStopReturnsJobToPending(t *testing.T) {
	th, f := setupDirectoryAPI(t)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.LdapSettings.EnableSync = new(true) })
	f.stall.Store(true)
	job, appErr := th.App.Srv().Jobs.CreateJob(th.Context, model.JobTypeLdapSync, nil)
	require.Nil(t, appErr)
	worker := NewSyncJob(th.App).MakeWorker().(*syncWorker)
	go worker.Run()
	t.Cleanup(worker.Stop)
	worker.JobChannel() <- *job
	require.Eventually(t, func() bool { return f.searches.Load() > 0 }, 5*time.Second, 10*time.Millisecond)
	worker.Stop()
	current, appErr := th.App.Srv().Jobs.GetJob(th.Context, job.Id)
	require.Nil(t, appErr)
	require.Equal(t, model.JobStatusPending, current.Status)
}
