package user_no_email_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	_ "github.com/mattermost/enterprise"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
)

func TestMain(m *testing.M) {
	mainHelper := testlib.NewMainHelper()
	defer mainHelper.Close()

	api4.SetMainHelper(mainHelper)
	mainHelper.Main(m)
}

func TestCreateUsersWithoutEmailFallsBackToCoreWithoutLicenseAndStoresEmptyEmailWithLicense(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	})
	ctx := context.Background()

	ordinaryUser := &model.User{
		Email:    "unlicensed-with-email-" + model.NewId() + "@example.com",
		Username: "unlicensed-with-email-" + model.NewId(),
		Password: "Test-password1",
	}
	created, response, err := th.SystemAdminClient.CreateUser(ctx, ordinaryUser)
	require.NoError(t, err)
	require.NotNil(t, created)
	require.NotNil(t, response)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	require.Equal(t, ordinaryUser.Email, created.Email)

	userWithoutLicense := &model.User{
		Username: "unlicensed-no-email-" + model.NewId(),
		Password: "Test-password1",
	}
	created, response, err = th.SystemAdminClient.CreateUser(ctx, userWithoutLicense)
	require.Error(t, err)
	require.Nil(t, created)
	require.NotNil(t, response)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	api4.CheckErrorID(t, err, "model.user.is_valid.email.app_error")

	th.App.Srv().SetLicense(model.NewTestLicense())
	invalidUsername := "invalid-create-" + model.NewId()
	validBody := `{"username":"` + invalidUsername + `","password":"Test-password1"}`
	for _, tc := range []struct {
		body   string
		status int
	}{
		{validBody + ` true`, http.StatusBadRequest},
		{validBody + strings.Repeat(" ", 1<<20), http.StatusRequestEntityTooLarge},
	} {
		response, requestErr := th.SystemAdminClient.DoAPIPost(ctx, "/users", tc.body)
		require.Error(t, requestErr)
		require.Equal(t, tc.status, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
	_, _, err = th.SystemAdminClient.GetUserByUsername(ctx, invalidUsername, "")
	require.Error(t, err)

	for range 2 {
		requested := &model.User{
			Username: "licensed-no-email-" + model.NewId(),
			Password: "Test-password1",
		}

		created, response, err = th.SystemAdminClient.CreateUser(ctx, requested)
		require.NoError(t, err)
		require.NotNil(t, created)
		require.NotNil(t, response)
		require.Equal(t, http.StatusCreated, response.StatusCode)
		require.Empty(t, created.Email)
		require.False(t, created.EmailVerified)
		require.True(t, created.DisableWelcomeEmail)

		var storedEmail string
		err = th.Store.GetInternalMasterDB().QueryRowContext(ctx,
			`SELECT email FROM users WHERE id = $1`, created.Id,
		).Scan(&storedEmail)
		require.NoError(t, err)
		require.Empty(t, storedEmail)

		fetched, _, err := th.SystemAdminClient.GetUser(ctx, created.Id, "")
		require.NoError(t, err)
		require.Empty(t, fetched.Email)

		loginClient := th.CreateClient()
		loggedIn, loginResponse, err := loginClient.Login(ctx, created.Username, "Test-password1")
		require.NoError(t, err)
		require.NotNil(t, loginResponse)
		require.Equal(t, http.StatusOK, loginResponse.StatusCode)
		require.Equal(t, created.Id, loggedIn.Id)
		require.Empty(t, loggedIn.Email)
	}
}

func TestCustomStatusForUserWithoutEmail(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.TeamSettings.EnableCustomUserStatuses = new(true)
	})
	ctx := context.Background()
	th.App.Srv().SetLicense(model.NewTestLicense())
	user, _, err := th.SystemAdminClient.CreateUser(ctx, &model.User{
		Username: "custom-no-email-" + model.NewId(),
		Password: "Test-password1",
	})
	require.NoError(t, err)
	client := th.CreateClient()
	_, _, err = client.Login(ctx, user.Username, "Test-password1")
	require.NoError(t, err)
	validStatusBody := `{"text":"يجب ألا تحفظ"}`
	for _, tc := range []struct {
		body   string
		status int
	}{
		{validStatusBody + ` true`, http.StatusBadRequest},
		{validStatusBody + strings.Repeat(" ", 16<<10), http.StatusRequestEntityTooLarge},
	} {
		response, requestErr := client.DoAPIRequestWithHeaders(ctx, http.MethodPut, "/users/me/status/custom", tc.body, nil)
		require.Error(t, requestErr)
		require.Equal(t, tc.status, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
	unchanged, _, err := client.GetUser(ctx, user.Id, "")
	require.NoError(t, err)
	require.Empty(t, unchanged.Email)
	require.Nil(t, unchanged.GetCustomStatus())

	_, response, err := client.UpdateUserCustomStatus(ctx, "me", &model.CustomStatus{Text: "في اجتماع"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	stored, _, err := client.GetUser(ctx, user.Id, "")
	require.NoError(t, err)
	require.Empty(t, stored.Email)
	require.Equal(t, "في اجتماع", stored.GetCustomStatus().Text)

	response, err = client.RemoveUserCustomStatus(ctx, "me")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	stored, _, err = client.GetUser(ctx, user.Id, "")
	require.NoError(t, err)
	require.Empty(t, stored.Email)
	require.Nil(t, stored.GetCustomStatus())
}

func TestOnlyAdminCanChangeLastActiveSharing(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	ctx := context.Background()
	user := th.BasicUser.DeepCopy()
	user.Props = model.CopyStringMap(user.Props)
	if user.Props == nil {
		user.Props = model.StringMap{}
	}
	user.Props["show_last_active"] = "false"
	_, response, err := th.Client.UpdateUser(ctx, user)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	_, response, err = th.Client.PatchUser(ctx, user.Id, &model.UserPatch{Props: user.Props})
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	updated, response, err := th.SystemAdminClient.PatchUser(ctx, user.Id, &model.UserPatch{Props: user.Props})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "false", updated.Props["show_last_active"])
}

func TestSystemAdminCanClearUserEmail(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	})
	ctx := context.Background()
	th.App.Srv().SetLicense(model.NewTestLicense())

	requested := &model.User{
		Email:    "clear-email-" + model.NewId() + "@example.com",
		Username: "clear-email-" + model.NewId(),
		Password: "Test-password1",
	}
	created, response, err := th.SystemAdminClient.CreateUser(ctx, requested)
	require.NoError(t, err)
	require.NotNil(t, created)
	require.NotNil(t, response)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	require.NotEmpty(t, created.Email)

	emptyEmail := ""
	updated, response, err := th.SystemAdminClient.PatchUser(ctx, created.Id, &model.UserPatch{Email: &emptyEmail})
	require.NoError(t, err)
	require.NotNil(t, updated)
	require.NotNil(t, response)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Empty(t, updated.Email)
	require.False(t, updated.EmailVerified)

	var storedEmail string
	err = th.Store.GetInternalMasterDB().QueryRowContext(ctx,
		`SELECT email FROM users WHERE id = $1`, created.Id,
	).Scan(&storedEmail)
	require.NoError(t, err)
	require.Empty(t, storedEmail)
}
