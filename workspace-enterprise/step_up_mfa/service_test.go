package step_up_mfa

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dgryski/dgoogauth"
	"github.com/mattermost/enterprise/required_password_change"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	app.RegisterComplianceInterface(func(a *app.App) einterfaces.ComplianceInterface {
		Register(a)
		required_password_change.Register(a)
		return nil
	})
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestCandidateRoutes(t *testing.T) {
	id := model.NewId()
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodDelete, "/api/v4/teams/" + id, true},
		{http.MethodDelete, "/api/v4/users/" + id, true},
		{http.MethodPut, "/api/v4/users/" + id + "/active", true},
		{http.MethodPut, "/api/v4/users/" + id + "/password", true},
		{http.MethodPut, "/api/v4/workspace/users/" + id + "/temporary_password", true},
		{http.MethodPost, "/api/v4/workspace/users/" + id + "/temporary_password", false},
		{http.MethodPut, "/api/v4/workspace/users/invalid/temporary_password", false},
		{http.MethodPut, "/api/v4/users/" + id + "/mfa", true},
		{http.MethodPut, "/api/v4/config", true},
		{http.MethodPut, "/api/v4/config/patch", true},
		{http.MethodGet, "/api/v4/teams/" + id, false},
		{http.MethodDelete, "/api/v4/teams/invalid", false},
		{http.MethodPut, "/api/v4/users/" + id + "/status", false},
	} {
		require.Equal(t, tc.want, isCandidate(httptest.NewRequest(tc.method, tc.path, nil)), tc.path)
	}
}

func TestPolicyDefaultsAndInvalidValuesFailClosed(t *testing.T) {
	t.Setenv("WORKSPACE_STEP_UP_MFA_GRACE_MINUTES", "")
	t.Setenv("WORKSPACE_STEP_UP_MFA_TEAM_DELETION", "false")
	t.Setenv("WORKSPACE_STEP_UP_MFA_USER_DELETION", "invalid")
	configured := loadPolicySettings()
	require.False(t, configured.TeamDeletion)
	require.True(t, configured.UserDeletion)
	require.True(t, configured.UserPasswordChange)
	require.True(t, configured.UserMfaRemoval)
	require.True(t, configured.ConfigDisable)
	require.Equal(t, 30, configured.GraceMinutes)
}

func TestOperationPolicyCanExcludeTeamDeletionWithoutExcludingUserDeactivation(t *testing.T) {
	t.Setenv("WORKSPACE_STEP_UP_MFA_ENABLED", "true")
	t.Setenv("WORKSPACE_STEP_UP_MFA_TEAM_DELETION", "false")
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	enableAdminMFA(t, th)

	team := th.CreateTeam(t)
	response, err := th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodDelete, "/teams/"+team.Id, "", nil)
	require.NoError(t, err)
	response.Body.Close()

	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPut,
		"/users/"+th.BasicUser.Id+"/active", `{"active":false}`, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()
}

func TestTeamDeletionRequiresFreshMFAAndUsesSessionGrace(t *testing.T) {
	t.Setenv("WORKSPACE_STEP_UP_MFA_ENABLED", "false")
	t.Setenv("WORKSPACE_STEP_UP_MFA_GRACE_MINUTES", "5")
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	secret := enableAdminMFA(t, th)

	team := th.CreateTeam(t)
	path := "/teams/" + team.Id
	response, err := th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodDelete, path, "", nil)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()

	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodDelete, path, "", map[string]string{codeHeader: "000000"})
	require.Error(t, err)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	response.Body.Close()

	code := fmt.Sprintf("%06d", dgoogauth.ComputeCode(secret, time.Now().UTC().Unix()/30))
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodDelete, path, "", map[string]string{codeHeader: code})
	require.NoError(t, err)
	response.Body.Close()

	second := th.CreateTeam(t)
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodDelete, "/teams/"+second.Id, "", nil)
	require.NoError(t, err, "the verified session should be valid within the grace period")
	response.Body.Close()
}

func TestUserDeactivationAndMFADisablingRequireStepUp(t *testing.T) {
	t.Setenv("WORKSPACE_STEP_UP_MFA_ENABLED", "true")
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	enableAdminMFA(t, th)
	ctx := th.Context.Context()

	response, err := th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut,
		"/users/"+th.BasicUser.Id+"/active", `{"active":false}`, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()
	user, appErr := th.App.GetUser(th.Context, th.BasicUser.Id)
	require.Nil(t, appErr)
	require.Zero(t, user.DeleteAt)

	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut,
		"/config/patch", `{"ServiceSettings":{"EnableMultifactorAuthentication":false}}`, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()
	require.True(t, *th.App.Config().ServiceSettings.EnableMultifactorAuthentication)
}

func TestOAuthSessionIsRejectedByOriginalMFARouteWithoutStepUpChallenge(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	enableAdminMFA(t, th)

	session, appErr := th.App.GetSession(th.SystemAdminClient.AuthToken)
	require.Nil(t, appErr)
	session.IsOAuth = true
	th.App.AddSessionToCache(session)

	response, err := th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPut,
		"/users/"+th.BasicUser.Id+"/mfa", `{"activate":false}`, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()
	originalError, ok := err.(*model.AppError)
	require.True(t, ok)
	require.Equal(t, "api.context.permissions.app_error", originalError.Id)
}

func TestUserManagerDeletionRequiresStepUp(t *testing.T) {
	t.Setenv("WORKSPACE_STEP_UP_MFA_ENABLED", "true")
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	th.AddPermissionToRole(t, model.PermissionEditOtherUsers.Id, model.SystemUserManagerRoleId)
	_, appErr := th.App.UpdateUserRoles(th.Context, th.BasicUser.Id, model.SystemUserManagerRoleId, false)
	require.Nil(t, appErr)
	secret, appErr := th.App.GenerateMfaSecret(th.Context, th.BasicUser.Id)
	require.Nil(t, appErr)
	require.NoError(t, th.Store.User().UpdateMfaActive(th.BasicUser.Id, true))
	require.NoError(t, th.Store.User().UpdateMfaSecret(th.BasicUser.Id, secret.Secret))
	th.App.InvalidateCacheForUser(th.BasicUser.Id)
	path := "/users/" + th.BasicUser2.Id

	response, err := th.Client.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodDelete, path, "", nil)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()
	user, appErr := th.App.GetUser(th.Context, th.BasicUser2.Id)
	require.Nil(t, appErr)
	require.Zero(t, user.DeleteAt)

	code := fmt.Sprintf("%06d", dgoogauth.ComputeCode(secret.Secret, time.Now().UTC().Unix()/30))
	response, err = th.Client.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodDelete, path, "", map[string]string{codeHeader: code})
	require.NoError(t, err)
	response.Body.Close()
	user, appErr = th.App.GetUser(th.Context, th.BasicUser2.Id)
	require.Nil(t, appErr)
	require.Positive(t, user.DeleteAt)
}

func TestStepUpDoesNotRunWithoutLicense(t *testing.T) {
	t.Setenv("WORKSPACE_STEP_UP_MFA_ENABLED", "true")
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	enableAdminMFA(t, th)
	team := th.CreateTeam(t)
	th.Server.SetLicense(nil)

	response, err := th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodDelete, "/teams/"+team.Id, "", nil)
	require.NoError(t, err)
	response.Body.Close()
}

func enableAdminMFA(t *testing.T, th *api4.TestHelper) string {
	t.Helper()
	secret, appErr := th.App.GenerateMfaSecret(th.Context, th.SystemAdminUser.Id)
	require.Nil(t, appErr)
	require.NoError(t, th.Store.User().UpdateMfaActive(th.SystemAdminUser.Id, true))
	require.NoError(t, th.Store.User().UpdateMfaSecret(th.SystemAdminUser.Id, secret.Secret))
	th.App.InvalidateCacheForUser(th.SystemAdminUser.Id)
	return secret.Secret
}

func TestTemporaryPasswordResetRequiresStepUpMFA(t *testing.T) {
	t.Setenv("WORKSPACE_STEP_UP_MFA_ENABLED", "true")
	t.Setenv("WORKSPACE_STEP_UP_MFA_GRACE_MINUTES", "5")
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	secret := enableAdminMFA(t, th)
	path := "/workspace/users/" + th.BasicUser.Id + "/temporary_password"
	body := `{"new_password":"Temporary123!"}`
	response, err := th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPut, path, body, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPut, path, body, map[string]string{codeHeader: "000000"})
	require.Error(t, err)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	response.Body.Close()
	code := fmt.Sprintf("%06d", dgoogauth.ComputeCode(secret, time.Now().UTC().Unix()/30))
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPut, path, body, map[string]string{codeHeader: code})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	response.Body.Close()
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPut, path, body, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	response.Body.Close()
}
