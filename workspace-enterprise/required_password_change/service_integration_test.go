//go:build integration

package required_password_change

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/mattermost/enterprise/compliance_service"
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
		return compliance_service.New(a)
	})
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestTemporaryPasswordFlow(t *testing.T) {
	t.Run("with email", func(t *testing.T) { runTemporaryPasswordFlow(t, false) })
	t.Run("without email", func(t *testing.T) { runTemporaryPasswordFlow(t, true) })
}

func runTemporaryPasswordFlow(t *testing.T, withoutEmail bool) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	originalEmail := th.BasicUser.Email
	if withoutEmail {
		_, err := th.App.Srv().Store().GetInternalMasterDB().Exec(`UPDATE users SET email = '' WHERE id = $1`, th.BasicUser.Id)
		require.NoError(t, err)
		th.App.InvalidateCacheForUser(th.BasicUser.Id)
		originalEmail = ""
	}
	assertProfilePreserved := func() {
		t.Helper()
		user, appErr := th.App.GetUser(th.Context, th.BasicUser.Id)
		require.Nil(t, appErr)
		require.Equal(t, originalEmail, user.Email)
		require.Equal(t, "preserved", user.Props["unrelated_property"])
	}
	_, sqlErr := th.App.Srv().Store().GetInternalMasterDB().Exec(`UPDATE users SET props = COALESCE(props::jsonb, '{}'::jsonb) || '{"unrelated_property":"preserved"}'::jsonb WHERE id = $1`, th.BasicUser.Id)
	require.NoError(t, sqlErr)
	th.App.InvalidateCacheForUser(th.BasicUser.Id)

	resetPath := "/workspace/users/" + th.BasicUser.Id + "/temporary_password"
	// A normal user cannot use this administrative reset endpoint.
	_, err := th.Client.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPut, "/workspace/users/"+th.BasicUser2.Id+"/temporary_password", `{"new_password":"NotAllowed123!"}`, nil)
	require.Error(t, err)
	appError, ok := err.(*model.AppError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, appError.StatusCode)
	response, err := th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPut, resetPath, `{"new_password":"Temporary123!"}`, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())

	user, appErr := th.App.GetUser(th.Context, th.BasicUser.Id)
	require.Nil(t, appErr)
	require.Equal(t, "true", user.Props[propKey])
	assertProfilePreserved()
	// A property saved after the reset must survive completion too.
	_, sqlErr = th.App.Srv().Store().GetInternalMasterDB().Exec(`UPDATE users SET props = props::jsonb || '{"added_after_reset":"kept"}'::jsonb WHERE id = $1`, th.BasicUser.Id)
	require.NoError(t, sqlErr)
	th.App.InvalidateCacheForUser(th.BasicUser.Id)

	client := th.CreateClient()
	_, _, err = client.Login(context.Background(), th.BasicUser.Username, "Temporary123!")
	require.NoError(t, err)
	// A custom client cannot use the account or clear its profile hint.
	for _, attempt := range []struct{ method, path, body string }{
		{http.MethodPost, "/posts", `{"channel_id":"` + th.BasicChannel.Id + `","message":"blocked"}`},
		{http.MethodPut, "/users/" + th.BasicUser.Id + "/patch", `{"props":{}}`},
		{http.MethodPut, "/users/" + th.BasicUser.Id, `{"props":{}}`},
	} {
		denied, deniedErr := client.DoAPIRequestWithHeaders(th.Context.Context(), attempt.method, attempt.path, attempt.body, nil)
		require.Error(t, deniedErr)
		require.Equal(t, http.StatusForbidden, denied.StatusCode)
		denied.Body.Close()
	}
	// Even removal outside the profile API cannot remove the server-owned state.
	_, sqlErr = th.App.Srv().Store().GetInternalMasterDB().Exec(`UPDATE users SET props=props::jsonb - $2::text WHERE id=$1`, th.BasicUser.Id, propKey)
	require.NoError(t, sqlErr)
	th.App.InvalidateCacheForUser(th.BasicUser.Id)
	denied, deniedErr := client.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPost, "/posts", `{"channel_id":"`+th.BasicChannel.Id+`","message":"blocked"}`, nil)
	require.Error(t, deniedErr)
	require.Equal(t, http.StatusForbidden, denied.StatusCode)
	denied.Body.Close()
	response, err = client.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPut, "/workspace/users/me/required_password", `{"current_password":"Temporary123!","new_password":"Permanent123!"}`, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())

	user, appErr = th.App.GetUser(th.Context, th.BasicUser.Id)
	require.Nil(t, appErr)
	require.NotContains(t, user.Props, propKey)
	require.Equal(t, "kept", user.Props["added_after_reset"])
	assertProfilePreserved()
	_, _, err = client.Login(context.Background(), th.BasicUser.Username, "Permanent123!")
	require.NoError(t, err)

	// A later reset without enforcement must also remove an older requirement.
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPut, resetPath, `{"new_password":"TemporaryAgain123!"}`, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	policyResponse, policyErr := th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPut, "/workspace/required_password_change/policy", `{"require_on_admin_reset":false,"version":0}`, nil)
	require.NoError(t, policyErr)
	policyResponse.Body.Close()
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPut, resetPath, `{"new_password":"AdminChosen123!","require_password_change":false}`, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())

	user, appErr = th.App.GetUser(th.Context, th.BasicUser.Id)
	require.Nil(t, appErr)
	require.NotContains(t, user.Props, propKey)
	assertProfilePreserved()
	_, _, err = client.Login(context.Background(), th.BasicUser.Username, "AdminChosen123!")
	require.NoError(t, err)

	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodPut, resetPath, `{"new_password":"a","require_password_change":false}`, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	response.Body.Close()
	_, _, err = client.Login(context.Background(), th.BasicUser.Username, "AdminChosen123!")
	require.NoError(t, err)
	assertProfilePreserved()
}

func TestPrivateRequirementMigrationAndCompletionCAS(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
	db := th.Store.GetInternalMasterDB()
	s := &Service{db: db}
	_, err := db.Exec(`UPDATE users SET props=COALESCE(props::jsonb,'{}'::jsonb)||'{"require_password_change":"true"}'::jsonb WHERE id=$1`, th.BasicUser.Id)
	require.NoError(t, err)
	require.NoError(t, s.initState(context.Background()))
	required, version, err := s.requiredState(context.Background(), th.BasicUser.Id)
	require.NoError(t, err)
	require.True(t, required)
	// A subsequent admin reset invalidates the old completion token.
	nextVersion := model.NewId()
	_, err = db.Exec(`UPDATE WorkspaceRequiredPasswordChange SET Version=$2 WHERE UserId=$1`, th.BasicUser.Id, nextVersion)
	require.NoError(t, err)
	require.Error(t, s.clearRequired(context.Background(), th.BasicUser.Id, version))
	required, _, err = s.requiredState(context.Background(), th.BasicUser.Id)
	require.NoError(t, err)
	require.True(t, required)
	require.NoError(t, s.clearRequired(context.Background(), th.BasicUser.Id, nextVersion))
	// A stale UI property cannot resurrect a completed private requirement.
	require.NoError(t, s.initState(context.Background()))
	required, _, err = s.requiredState(context.Background(), th.BasicUser.Id)
	require.NoError(t, err)
	require.False(t, required)
}

func TestPasswordPolicyEnforcesShortTemporaryReset(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.PasswordSettings.MinimumLength = new(12)
		cfg.PasswordSettings.Lowercase = new(true)
		cfg.PasswordSettings.Uppercase = new(true)
		cfg.PasswordSettings.Number = new(true)
		cfg.PasswordSettings.Symbol = new(true)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	route := "/workspace/required_password_change/policy"
	response, err := th.SystemAdminClient.DoAPIRequestWithHeaders(context.Background(), http.MethodGet, route, "", nil)
	require.NoError(t, err)
	var p passwordPolicy
	require.NoError(t, json.NewDecoder(response.Body).Decode(&p))
	response.Body.Close()
	require.True(t, p.RequireOnAdminReset)
	_, err = th.Client.DoAPIRequestWithHeaders(context.Background(), http.MethodPut, route, `{"require_on_admin_reset":false,"version":0}`, nil)
	require.Error(t, err)
	reset := "/workspace/users/" + th.BasicUser.Id + "/temporary_password"
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(context.Background(), http.MethodPut, reset, `{"new_password":"1234","require_password_change":false}`, nil)
	require.NoError(t, err)
	response.Body.Close()
	profile, appErr := th.App.GetUser(th.Context, th.BasicUser.Id)
	require.Nil(t, appErr)
	require.Equal(t, "true", profile.Props[propKey])
	client := th.CreateClient()
	_, _, err = client.Login(context.Background(), th.BasicUser.Username, "1234")
	require.NoError(t, err)
	response, err = client.DoAPIRequestWithHeaders(context.Background(), http.MethodPut, "/workspace/users/me/required_password", `{"current_password":"1234","new_password":"abcd"}`, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	response.Body.Close()
	response, err = client.DoAPIRequestWithHeaders(context.Background(), http.MethodPut, "/workspace/users/me/required_password", `{"current_password":"1234","new_password":"Permanent123!"}`, nil)
	require.NoError(t, err)
	response.Body.Close()
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(context.Background(), http.MethodPut, route, `{"require_on_admin_reset":false,"version":0}`, nil)
	require.NoError(t, err)
	response.Body.Close()
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(context.Background(), http.MethodPut, route, `{"require_on_admin_reset":true,"version":0}`, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusConflict, response.StatusCode)
	response.Body.Close()
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(context.Background(), http.MethodPut, reset, `{"new_password":"1234","require_password_change":false}`, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	response.Body.Close()
}

func TestAdminCreatedAccountsRequireFirstLoginChange(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.PasswordSettings.MinimumLength = new(12)
		cfg.PasswordSettings.Lowercase = new(true)
		cfg.PasswordSettings.Uppercase = new(true)
		cfg.PasswordSettings.Number = new(true)
		cfg.PasswordSettings.Symbol = new(true)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	_, err := th.Client.DoAPIRequestWithHeaders(context.Background(), http.MethodPost, "/workspace/users", `{"username":"unauthorized","password":"1234"}`, nil)
	require.Error(t, err)
	// The global reset switch does not remove first-login enforcement for new accounts.
	response, err := th.SystemAdminClient.DoAPIRequestWithHeaders(context.Background(), http.MethodPut, "/workspace/required_password_change/policy", `{"require_on_admin_reset":false,"version":0}`, nil)
	require.NoError(t, err)
	response.Body.Close()
	for i, password := range []string{"1234", "a"} {
		username := "temporary" + model.NewId()
		email := ""
		if i == 1 {
			email = username + "@example.com"
		}
		payload, _ := json.Marshal(map[string]string{"username": username, "email": email, "password": password, "first_name": "Employee"})
		response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(context.Background(), http.MethodPost, "/workspace/users", string(payload), nil)
		require.NoError(t, err)
		var created model.User
		require.NoError(t, json.NewDecoder(response.Body).Decode(&created))
		response.Body.Close()
		require.Equal(t, http.StatusCreated, response.StatusCode)
		require.Equal(t, "", created.Password)
		require.Equal(t, "", created.AuthService)
		require.Equal(t, "true", created.Props[propKey])
		require.Equal(t, "Employee", created.FirstName)
		require.Equal(t, model.SystemUserRoleId, created.Roles)
		client := th.CreateClient()
		_, _, err = client.Login(context.Background(), username, password)
		require.NoError(t, err)
		response, err = client.DoAPIRequestWithHeaders(context.Background(), http.MethodPost, "/posts", `{"channel_id":"`+th.BasicChannel.Id+`","message":"blocked"}`, nil)
		require.Error(t, err)
		require.Equal(t, http.StatusForbidden, response.StatusCode)
		response.Body.Close()
		response, err = client.DoAPIRequestWithHeaders(context.Background(), http.MethodPut, "/workspace/users/me/required_password", fmt.Sprintf(`{"current_password":%q,"new_password":"Permanent123!"}`, password), nil)
		require.NoError(t, err)
		response.Body.Close()
	}
	// Administrative creation accepts only local-user fields, never roles/auth data.
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(context.Background(), http.MethodPost, "/workspace/users", `{"username":"injected","password":"1234","roles":"system_admin"}`, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	response.Body.Close()
}
