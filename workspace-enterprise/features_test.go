package enterprise_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	_ "github.com/mattermost/enterprise"
	"github.com/mattermost/enterprise/audit_file"
	"github.com/mattermost/enterprise/file_storage"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

const channelMemberViewedEvent = model.WebsocketEventType("channel_member_viewed")

func TestMain(m *testing.M) {
	mainHelper := testlib.NewMainHelper()
	defer mainHelper.Close()

	api4.SetMainHelper(mainHelper)
	mainHelper.Main(m)
}

func setupEnterpriseTest(tb testing.TB) *api4.TestHelper {
	return api4.SetupConfig(tb, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	})
}

func TestChannelReadReceiptWebSocketEvent(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())

	senderClient := th.CreateClient()
	th.LoginBasicWithClient(t, senderClient)
	readerClient := th.CreateClient()
	th.LoginBasic2WithClient(t, readerClient)

	webSocketClient := th.CreateConnectedWebSocketClientWithClient(t, senderClient)
	defer webSocketClient.Close()
	response := <-webSocketClient.ResponseChannel
	require.Equal(t, model.StatusOk, response.Status)

	post := th.CreatePostWithClient(t, senderClient, th.BasicChannel)
	viewBody, err := json.Marshal(&model.ChannelView{ChannelId: th.BasicChannel.Id, CollapsedThreadsSupported: true})
	require.NoError(t, err)
	viewResponse, err := readerClient.DoAPIPost(context.Background(), "/channels/members/me/view", string(viewBody))
	require.NoError(t, err)
	require.NoError(t, viewResponse.Body.Close())

	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-webSocketClient.EventChannel:
			if event.EventType() != channelMemberViewedEvent {
				continue
			}
			require.Equal(t, th.BasicUser2.Id, event.GetData()["user_id"])
			require.Equal(t, th.BasicChannel.Id, event.GetData()["channel_id"])
			require.GreaterOrEqual(t, int64(event.GetData()["last_viewed_at"].(float64)), post.CreateAt)
			return
		case <-deadline:
			t.Fatal("timed out waiting for channel_member_viewed websocket event")
		}
	}
}

func TestNewPostRequiresExplicitViewForReadReceipt(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())

	senderClient := th.CreateClient()
	th.LoginBasicWithClient(t, senderClient)
	readerClient := th.CreateClient()
	th.LoginBasic2WithClient(t, readerClient)

	webSocketClient := th.CreateConnectedWebSocketClientWithClient(t, senderClient)
	defer webSocketClient.Close()
	response := <-webSocketClient.ResponseChannel
	require.Equal(t, model.StatusOk, response.Status)

	_, _, err := readerClient.ViewChannel(context.Background(), model.Me, &model.ChannelView{
		ChannelId:                 th.BasicChannel.Id,
		CollapsedThreadsSupported: true,
	})
	require.NoError(t, err)

	var before, after int64
	readCursor := func(cursor *int64) {
		t.Helper()
		require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT lastviewedat
			FROM channelmembers WHERE channelid=$1 AND userid=$2`, th.BasicChannel.Id, th.BasicUser2.Id).Scan(cursor))
	}
	readCursor(&before)
	post := th.CreatePostWithClient(t, senderClient, th.BasicChannel)
	readCursor(&after)
	require.Equal(t, before, after, "receiving a post in an active channel is not proof of reading it")
	_, _, err = readerClient.ViewChannel(context.Background(), model.Me, &model.ChannelView{
		ChannelId: th.BasicChannel.Id, CollapsedThreadsSupported: true,
	})
	require.NoError(t, err)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-webSocketClient.EventChannel:
			if event.EventType() != channelMemberViewedEvent || event.GetData()["user_id"] != th.BasicUser2.Id {
				continue
			}
			lastViewedAt := int64(event.GetData()["last_viewed_at"].(float64))
			if lastViewedAt < post.CreateAt {
				continue
			}
			require.Equal(t, th.BasicChannel.Id, event.GetData()["channel_id"])
			require.GreaterOrEqual(t, lastViewedAt, post.CreateAt)
			return
		case <-deadline:
			t.Fatal("timed out waiting for explicit-view channel_member_viewed websocket event")
		}
	}
}

func TestOpenIDUserWithoutEmailIsStoredWithEmptyEmail(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	th.App.UpdateConfig(func(config *model.Config) {
		config.OpenIdSettings.Enable = model.NewPointer(true)
		config.OpenIdSettings.AuthEndpoint = model.NewPointer("https://idp.example/authorize")
		config.OpenIdSettings.TokenEndpoint = model.NewPointer("https://idp.example/token")
		config.OpenIdSettings.UserAPIEndpoint = model.NewPointer("https://idp.example/userinfo")
		config.OpenIdSettings.DiscoveryEndpoint = model.NewPointer("")
		config.OpenIdSettings.UsePreferredUsername = model.NewPointer(true)
	})

	subject := "openid-no-email-" + model.NewId()
	username := "openid-no-email-" + model.NewId()
	userJSON := fmt.Sprintf(`{"sub":%q,"preferred_username":%q,"name":"OpenID User"}`, subject, username)
	createdUser, wasCreated, appErr := th.App.CreateOAuthUser(th.Context, model.ServiceOpenid, strings.NewReader(userJSON), "", "", nil)
	require.Nil(t, appErr)
	require.NotNil(t, createdUser)
	require.True(t, wasCreated)

	require.Eventually(t, func() bool {
		storedUser, err := th.Store.User().GetByAuth(&subject, model.ServiceOpenid)
		return err == nil && storedUser.Email == "" && !storedUser.EmailVerified
	}, 3*time.Second, 20*time.Millisecond)
}

func TestComplianceReportCreatesDownloadableArchive(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense("compliance"))

	outputDir := t.TempDir() + string(os.PathSeparator)
	th.App.UpdateConfig(func(config *model.Config) {
		config.ComplianceSettings.Enable = model.NewPointer(true)
		config.ComplianceSettings.EnableDaily = model.NewPointer(false)
		config.ComplianceSettings.Directory = model.NewPointer(outputDir)
		config.ComplianceSettings.BatchSize = model.NewPointer(1)
	})

	post := th.CreatePost(t)
	report, appErr := th.App.SaveComplianceReport(th.Context, &model.Compliance{
		Desc:    "workspace compliance integration",
		StartAt: post.CreateAt - 1,
		EndAt:   post.CreateAt + 1,
	})
	require.Nil(t, appErr)

	var stored *model.Compliance
	require.Eventually(t, func() bool {
		var err error
		stored, err = th.Store.Compliance().Get(report.Id)
		return err == nil && stored.Status == model.ComplianceStatusFinished
	}, 10*time.Second, 20*time.Millisecond)
	require.Positive(t, stored.Count)

	archive, appErr := th.App.GetComplianceFile(stored)
	require.Nil(t, appErr)
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	require.Len(t, reader.File, 1)

	csvFile, err := reader.File[0].Open()
	require.NoError(t, err)
	defer csvFile.Close()
	csvContents, err := io.ReadAll(csvFile)
	require.NoError(t, err)
	require.Contains(t, string(csvContents), post.Id)
}

func TestUpdateOpenIDAuthRejectsPartialConsolidation(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	ctx := context.Background()

	target := th.BasicUser
	source := th.BasicUser2
	sourceClient := th.CreateClient()
	th.LoginBasic2WithClient(t, sourceClient)
	post := th.CreatePostWithClient(t, sourceClient, th.BasicChannel)
	subject := "openid-existing-" + model.NewId()
	_, storeErr := th.Store.User().UpdateAuthData(source.Id, model.ServiceOpenid, &subject, "", false)
	require.NoError(t, storeErr)
	th.App.InvalidateCacheForUser(source.Id)

	_, response, err := th.SystemAdminClient.UpdateUserAuth(ctx, target.Id, &model.UserAuth{
		AuthService: model.ServiceOpenid,
		AuthData:    &subject,
	})
	require.Error(t, err)
	require.Equal(t, http.StatusConflict, response.StatusCode)

	storedTarget, storeErr := th.Store.User().Get(th.Context, target.Id)
	require.NoError(t, storeErr)
	require.Empty(t, storedTarget.AuthService)
	require.Nil(t, storedTarget.AuthData)
	storedSource, storeErr := th.Store.User().Get(th.Context, source.Id)
	require.NoError(t, storeErr)
	require.Zero(t, storedSource.DeleteAt)
	require.Equal(t, model.ServiceOpenid, storedSource.AuthService)
	require.Equal(t, subject, *storedSource.AuthData)
	storedPost, storeErr := th.Store.Post().GetSingle(th.Context, post.Id, false)
	require.NoError(t, storeErr)
	require.Equal(t, source.Id, storedPost.UserId)
}

func TestLinkUnusedOpenIDIdentityKeepsLocalHistoryAndPassword(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	ctx := context.Background()
	username := "openid-link-local-" + model.NewId()
	password := "Test-password1"
	target, response, err := th.SystemAdminClient.CreateUser(ctx, &model.User{
		Username: username,
		Email:    username + "@example.com",
		Password: password,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	th.LinkUserToTeam(t, target, th.BasicTeam)
	th.AddUserToChannel(t, target, th.BasicChannel)
	localClient := th.CreateClient()
	_, _, err = localClient.Login(ctx, username, password)
	require.NoError(t, err)
	before := th.CreatePostWithClient(t, localClient, th.BasicChannel)

	subject := "openid-unused-" + model.NewId()
	authBody := fmt.Sprintf(`{"auth_service":"openid","auth_data":%q}`, subject)
	for _, tc := range []struct {
		body   string
		status int
	}{
		{fmt.Sprintf(`{"auth_service":"openid","auth_data":%q,"unexpected":true}`, subject), http.StatusBadRequest},
		{authBody + ` true`, http.StatusBadRequest},
		{authBody + strings.Repeat(" ", 4096), http.StatusRequestEntityTooLarge},
	} {
		response, requestErr := th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut,
			"/users/"+target.Id+"/auth", tc.body, nil)
		require.Error(t, requestErr)
		require.Equal(t, tc.status, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
	beforeLink, storeErr := th.Store.User().Get(th.Context, target.Id)
	require.NoError(t, storeErr)
	require.Empty(t, beforeLink.AuthService)
	var savedCredentials int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(
		`SELECT COUNT(*) FROM workspace_linked_credentials WHERE userid=$1`, target.Id).Scan(&savedCredentials))
	require.Zero(t, savedCredentials)
	_, response, err = th.SystemAdminClient.UpdateUserAuth(ctx, target.Id, &model.UserAuth{
		AuthService: model.ServiceOpenid,
		AuthData:    &subject,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)

	linkedClient := th.CreateClient()
	loggedIn, response, err := linkedClient.Login(ctx, username, password)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, target.Id, loggedIn.Id)
	after := th.CreatePostWithClient(t, linkedClient, th.BasicChannel)
	for _, postID := range []string{before.Id, after.Id} {
		storedPost, storeErr := th.Store.Post().GetSingle(th.Context, postID, false)
		require.NoError(t, storeErr)
		require.Equal(t, target.Id, storedPost.UserId)
	}
	resolved, storeErr := th.Store.User().GetByAuth(&subject, model.ServiceOpenid)
	require.NoError(t, storeErr)
	require.Equal(t, target.Id, resolved.Id)
	provider := einterfaces.GetOAuthProvider(model.ServiceOpenid)
	require.NotNil(t, provider)
	settings := th.App.Config().OpenIdSettings
	oauthUser, providerErr := provider.GetUserFromJSON(th.Context, strings.NewReader(fmt.Sprintf(
		`{"sub":%q,"email":%q,"preferred_username":"changed-at-provider"}`,
		subject, target.Email,
	)), nil, &settings)
	require.NoError(t, providerErr)
	require.Equal(t, username, oauthUser.Username)
	_, response, err = th.SystemAdminClient.UpdateUserAuth(ctx, target.Id, &model.UserAuth{
		AuthService: model.UserAuthServiceEmail,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var localService, localPassword string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT authservice,password FROM users WHERE id=$1`, target.Id).
		Scan(&localService, &localPassword))
	require.Empty(t, localService)
	require.NotEmpty(t, localPassword)
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(
		`SELECT COUNT(*) FROM workspace_linked_credentials WHERE userid=$1`, target.Id).Scan(&savedCredentials))
	require.Zero(t, savedCredentials)
	restoredClient := th.CreateClient()
	restoredUser, response, err := restoredClient.Login(ctx, username, password)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, target.Id, restoredUser.Id)
}

func TestUpdateOpenIDAuthRequiresAdminAndLicense(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	ctx := context.Background()
	target := th.BasicUser
	requested := &model.UserAuth{AuthService: model.ServiceOpenid, AuthData: model.NewPointer("openid-subject-" + model.NewId())}

	th.App.Srv().SetLicense(model.NewTestLicense())
	_, response, err := th.Client.UpdateUserAuth(ctx, target.Id, requested)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	th.App.Srv().SetLicense(nil)
	_, response, err = th.SystemAdminClient.UpdateUserAuth(ctx, target.Id, requested)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	stored, storeErr := th.Store.User().Get(th.Context, target.Id)
	require.NoError(t, storeErr)
	require.Empty(t, stored.AuthService)
	require.Nil(t, stored.AuthData)
}

func TestOpenIDLinkCandidatesRequireAdminAndLicense(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	ctx := context.Background()
	path := "/users/openid_link_candidates"

	th.App.Srv().SetLicense(model.NewTestLicense())
	response, err := th.Client.DoAPIGet(ctx, path, "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())

	th.App.Srv().SetLicense(nil)
	response, err = th.SystemAdminClient.DoAPIGet(ctx, path, "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())

	th.App.Srv().SetLicense(model.NewTestLicense())
	username := "openid-link-candidate-" + model.NewId()
	source, createResponse, err := th.SystemAdminClient.CreateUser(ctx, &model.User{
		Username: username,
		Email:    username + "@example.com",
		Password: "Test-password1",
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createResponse.StatusCode)
	subject := "openid-candidate-" + model.NewId()
	_, storeErr := th.Store.User().UpdateAuthData(source.Id, model.ServiceOpenid, &subject, "", false)
	require.NoError(t, storeErr)
	th.Store.User().ClearCaches()

	response, err = th.SystemAdminClient.DoAPIGet(ctx, path+"?q="+username+"&page=0", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var candidates struct {
		Users []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			AuthData string `json:"auth_data"`
		} `json:"users"`
		HasMore bool `json:"has_more"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&candidates))
	require.NoError(t, response.Body.Close())
	require.Len(t, candidates.Users, 1)
	require.Equal(t, source.Id, candidates.Users[0].ID)
	require.Equal(t, username, candidates.Users[0].Username)
	require.Equal(t, subject, candidates.Users[0].AuthData)
	require.False(t, candidates.HasMore)
	_, err = th.Store.GetInternalMasterDB().Exec(`
		INSERT INTO workspace_linked_credentials (userid, password, createat, updateat)
		VALUES ($1, $2, $3, $3)
	`, source.Id, "linked-test-hash", model.GetMillis())
	require.NoError(t, err)
	response, err = th.SystemAdminClient.DoAPIGet(ctx, path+"?q="+username+"&page=0", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, json.NewDecoder(response.Body).Decode(&candidates))
	require.NoError(t, response.Body.Close())
	require.Empty(t, candidates.Users)
}

func TestOpenIDMergePreviewRequiresAdminAndReportsReferences(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	ctx := context.Background()
	target := th.BasicUser
	source := th.BasicUser2
	sourceClient := th.CreateClient()
	th.LoginBasic2WithClient(t, sourceClient)
	post := th.CreatePostWithClient(t, sourceClient, th.BasicChannel)
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET props = $2 WHERE id = $1`, post.Id, fmt.Sprintf(`{"linked_user":%q}`, source.Id))
	require.NoError(t, err)
	subject := "openid-preview-" + model.NewId()
	_, storeErr := th.Store.User().UpdateAuthData(source.Id, model.ServiceOpenid, &subject, "", false)
	require.NoError(t, storeErr)
	th.Store.User().ClearCaches()
	th.App.InvalidateCacheForUser(source.Id)
	path := "/users/" + target.Id + "/openid_merge_preview?source_user_id=" + source.Id

	th.App.Srv().SetLicense(model.NewTestLicense())
	response, err := th.Client.DoAPIGet(ctx, path, "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())

	th.App.Srv().SetLicense(nil)
	response, err = th.SystemAdminClient.DoAPIGet(ctx, path, "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())

	th.App.Srv().SetLicense(model.NewTestLicense())
	storedSource, storeErr := th.Store.User().Get(th.Context, source.Id)
	require.NoError(t, storeErr)
	require.Equal(t, model.ServiceOpenid, storedSource.AuthService)
	require.Equal(t, subject, *storedSource.AuthData)
	storedTarget, storeErr := th.Store.User().Get(th.Context, target.Id)
	require.NoError(t, storeErr)
	require.Empty(t, storedTarget.AuthService)
	require.Zero(t, storedTarget.DeleteAt)
	require.Zero(t, storedSource.DeleteAt)
	require.NotEqual(t, target.Id, source.Id)
	response, err = th.SystemAdminClient.DoAPIGet(ctx, path, "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var preview struct {
		SourceUserID string `json:"source_user_id"`
		TargetUserID string `json:"target_user_id"`
		Complete     bool   `json:"complete"`
		References   []struct {
			Table  string `json:"table"`
			Column string `json:"column"`
			Rows   int64  `json:"rows"`
			Kind   string `json:"kind"`
		} `json:"references"`
		Conflicts []struct {
			Table  string `json:"table"`
			Column string `json:"column"`
			Rows   int64  `json:"rows"`
		} `json:"conflicts"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&preview))
	require.NoError(t, response.Body.Close())
	require.Equal(t, source.Id, preview.SourceUserID)
	require.Equal(t, target.Id, preview.TargetUserID)
	require.False(t, preview.Complete)
	require.NotEmpty(t, post.Id)
	foundPosts := false
	foundEmbeddedReference := false
	for _, reference := range preview.References {
		if reference.Table == "posts" && reference.Column == "userid" {
			require.GreaterOrEqual(t, reference.Rows, int64(1))
			require.Equal(t, "exact", reference.Kind)
			foundPosts = true
		}
		if reference.Table == "posts" && reference.Column == "props" {
			require.GreaterOrEqual(t, reference.Rows, int64(1))
			require.Equal(t, "embedded", reference.Kind)
			foundEmbeddedReference = true
		}
	}
	require.True(t, foundPosts)
	require.True(t, foundEmbeddedReference)
	foundTeamMembershipConflict := false
	for _, conflict := range preview.Conflicts {
		if conflict.Table == "teammembers" && conflict.Column == "userid" {
			require.GreaterOrEqual(t, conflict.Rows, int64(1))
			foundTeamMembershipConflict = true
		}
	}
	require.True(t, foundTeamMembershipConflict)
}

func TestOpenIDMergeExecutionRejectsInvalidSourceWithoutChangingAccounts(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	path := "/users/" + th.BasicUser.Id + "/openid_merge"
	body := fmt.Sprintf(`{"source_user_id":%q}`, th.BasicUser2.Id)
	th.App.Srv().SetLicense(model.NewTestLicense())
	response, err := th.Client.DoAPIPost(context.Background(), path, body)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())
	th.App.Srv().SetLicense(nil)
	response, err = th.SystemAdminClient.DoAPIPost(context.Background(), path, body)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())
	th.App.Srv().SetLicense(model.NewTestLicense())
	response, err = th.SystemAdminClient.DoAPIPost(context.Background(), path, body)
	require.Error(t, err)
	require.Equal(t, http.StatusConflict, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

func TestOpenIDMergeExecutionLinksAccountsAndReportsStatus(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	source, target := th.BasicUser2, th.BasicUser
	sourceClient := th.CreateClient()
	th.LoginBasic2WithClient(t, sourceClient)
	before := th.CreatePostWithClient(t, sourceClient, th.BasicChannel)
	subject := "merge-route-" + model.NewId()
	_, err := th.Store.User().UpdateAuthData(source.Id, model.ServiceOpenid, &subject, "", false)
	require.NoError(t, err)
	th.Store.User().ClearCaches()
	mergePath := "/users/" + target.Id + "/openid_merge"
	for _, tc := range []struct {
		body   string
		status int
	}{
		{fmt.Sprintf(`{"source_user_id":%q,"unexpected":true}`, source.Id), http.StatusBadRequest},
		{fmt.Sprintf(`{"source_user_id":%q} true`, source.Id), http.StatusBadRequest},
		{fmt.Sprintf(`{"source_user_id":%q}`, source.Id) + strings.Repeat(" ", 4096), http.StatusRequestEntityTooLarge},
	} {
		response, requestErr := th.SystemAdminClient.DoAPIPost(context.Background(), mergePath, tc.body)
		require.Error(t, requestErr)
		require.Equal(t, tc.status, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
	originalOwner, err := th.Store.User().GetByAuth(&subject, model.ServiceOpenid)
	require.NoError(t, err)
	require.Equal(t, source.Id, originalOwner.Id)
	response, err := th.SystemAdminClient.DoAPIPost(context.Background(),
		mergePath, fmt.Sprintf(`{"source_user_id":%q}`, source.Id))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	response, err = th.SystemAdminClient.DoAPIGet(context.Background(), "/users/"+target.Id+"/openid_merge_status", "")
	require.NoError(t, err)
	defer response.Body.Close()
	var status struct {
		Linked         bool   `json:"linked"`
		SourceUserID   string `json:"source_user_id"`
		SourceUsername string `json:"source_username"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&status))
	require.True(t, status.Linked)
	require.Equal(t, source.Id, status.SourceUserID)
	require.Equal(t, source.Username, status.SourceUsername)
	for _, userID := range []string{target.Id, source.Id} {
		_, authResponse, authErr := th.SystemAdminClient.UpdateUserAuth(context.Background(), userID, &model.UserAuth{
			AuthService: model.UserAuthServiceEmail,
		})
		require.Error(t, authErr)
		require.Equal(t, http.StatusConflict, authResponse.StatusCode)
	}
	linkedOwner, ownerErr := th.Store.User().GetByAuth(&subject, model.ServiceOpenid)
	require.NoError(t, ownerErr)
	require.Equal(t, target.Id, linkedOwner.Id)
	unmergePath := "/users/" + target.Id + "/openid_unmerge"
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`null`, http.StatusBadRequest},
		{`{"preserve_new_on_local":true,"unexpected":true}`, http.StatusBadRequest},
		{`{"preserve_new_on_local":true} false`, http.StatusBadRequest},
		{`{"preserve_new_on_local":true}` + strings.Repeat(" ", 4096), http.StatusRequestEntityTooLarge},
	} {
		response, requestErr := th.SystemAdminClient.DoAPIPost(context.Background(), unmergePath, tc.body)
		require.Error(t, requestErr)
		require.Equal(t, tc.status, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
	resolved, err := th.Store.User().GetByAuth(&subject, model.ServiceOpenid)
	require.NoError(t, err)
	require.Equal(t, target.Id, resolved.Id)
	localClient := th.CreateClient()
	loggedIn, loginResponse, err := localClient.Login(context.Background(), target.Username, target.Password)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, loginResponse.StatusCode)
	require.Equal(t, target.Id, loggedIn.Id)
	after := th.CreatePostWithClient(t, localClient, th.BasicChannel)
	// Fast linking changes the login identity; it does not transfer the
	// former OpenID account's messages to the local account.
	formerPost, storeErr := th.Store.Post().GetSingle(th.Context, before.Id, false)
	require.NoError(t, storeErr)
	require.Equal(t, source.Id, formerPost.UserId)
	localPost, storeErr := th.Store.Post().GetSingle(th.Context, after.Id, false)
	require.NoError(t, storeErr)
	require.Equal(t, target.Id, localPost.UserId)
	provider := einterfaces.GetOAuthProvider(model.ServiceOpenid)
	require.NotNil(t, provider)
	settings := th.App.Config().OpenIdSettings
	profile, providerErr := provider.GetUserFromJSON(th.Context, strings.NewReader(fmt.Sprintf(
		`{"sub":%q,"email":%q,"preferred_username":"changed-at-provider"}`,
		subject, target.Email,
	)), nil, &settings)
	require.NoError(t, providerErr)
	require.Equal(t, target.Username, profile.Username)
}

func TestUnlinkedOpenIDUserCannotCreateLocalPassword(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	th.LoginBasicWithClient(t, th.Client)
	ctx := context.Background()
	subject := "openid-unlinked-" + model.NewId()
	_, err := th.Store.User().UpdateAuthData(th.BasicUser.Id, model.ServiceOpenid, &subject, "", false)
	require.NoError(t, err)
	th.Store.User().ClearCaches()

	response, err := th.Client.UpdateUserPassword(ctx, th.BasicUser.Id, "", "New-password1")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	var count int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT COUNT(*) FROM workspace_linked_credentials WHERE userid=$1`, th.BasicUser.Id).Scan(&count))
	require.Zero(t, count)

	response, err = th.SystemAdminClient.UpdateUserPassword(ctx, th.BasicUser.Id, "", "Admin-password1")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT COUNT(*) FROM workspace_linked_credentials WHERE userid=$1`, th.BasicUser.Id).Scan(&count))
	require.Equal(t, 1, count)
}

func TestReactionMarksPostReadAndPublishesReceipt(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())

	senderClient := th.CreateClient()
	th.LoginBasicWithClient(t, senderClient)
	readerClient := th.CreateClient()
	th.LoginBasic2WithClient(t, readerClient)

	webSocketClient := th.CreateConnectedWebSocketClientWithClient(t, senderClient)
	defer webSocketClient.Close()
	response := <-webSocketClient.ResponseChannel
	require.Equal(t, model.StatusOk, response.Status)

	post := th.CreatePostWithClient(t, senderClient, th.BasicChannel)
	_, _, err := readerClient.SaveReaction(context.Background(), &model.Reaction{
		UserId:    th.BasicUser2.Id,
		PostId:    post.Id,
		EmojiName: "thumbsup",
	})
	require.NoError(t, err)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-webSocketClient.EventChannel:
			if event.EventType() != channelMemberViewedEvent || event.GetData()["user_id"] != th.BasicUser2.Id {
				continue
			}
			lastViewedAt := int64(event.GetData()["last_viewed_at"].(float64))
			if lastViewedAt < post.CreateAt {
				continue
			}
			require.Equal(t, th.BasicChannel.Id, event.GetData()["channel_id"])
			return
		case <-deadline:
			t.Fatal("timed out waiting for reaction read-receipt websocket event")
		}
	}
}

func TestEnterpriseRoutesRequireLicenseAndServeExpectedPayloads(t *testing.T) {
	logRoot := t.TempDir()
	t.Setenv("MM_LOG_PATH", logRoot)
	th := setupEnterpriseTest(t).InitBasic(t)
	ctx := context.Background()
	openIDProvider := einterfaces.GetOAuthProvider(model.ServiceOpenid)
	require.NotNil(t, openIDProvider)
	openIDConfig := &model.Config{}
	openIDConfig.SetDefaults()
	openIDConfig.OpenIdSettings.Enable = model.NewPointer(true)
	openIDConfig.OpenIdSettings.AuthEndpoint = model.NewPointer("https://idp.example/auth")
	openIDConfig.OpenIdSettings.TokenEndpoint = model.NewPointer("https://idp.example/token")
	openIDConfig.OpenIdSettings.UserAPIEndpoint = model.NewPointer("https://idp.example/userinfo")
	_, openIDErr := openIDProvider.GetSSOSettings(th.Context, openIDConfig, model.ServiceOpenid)
	require.Error(t, openIDErr)

	response, err := th.SystemAdminClient.DoAPIGet(ctx, "/audits/file/status", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/channels/"+th.BasicChannel.Id+"/read_receipts", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/data_retention/policies_count", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/ldap/groups?page=0&per_page=200&q=", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/file_storage/usage", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	response, err = th.SystemAdminClient.DoAPIPost(ctx, "/users/ids", "[]")
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/terms_of_service", "")
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/brand/image", "")
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)

	teamForDeletion := th.CreateTeam(t)
	response, err = th.SystemAdminClient.DoAPIDelete(ctx, "/teams/"+teamForDeletion.Id+"?permanent=true")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	legacyPreference := model.Preference{
		UserId:   th.SystemAdminUser.Id,
		Category: model.PreferenceCategorySidebarSettings,
		Name:     model.PreferenceLimitVisibleDmsGms,
		Value:    "0",
	}
	legacyPreferenceJSON, err := json.Marshal(model.Preferences{legacyPreference})
	require.NoError(t, err)
	response, err = th.SystemAdminClient.DoAPIPut(ctx, "/users/"+th.SystemAdminUser.Id+"/preferences", string(legacyPreferenceJSON))
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	response, err = th.SystemAdminClient.DoAPIPost(ctx, "/users/deactivate/inactive", `{"days":30}`)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/reports/users?show_online_only=true&page_size=100", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	response, err = th.Client.DoAPIPost(ctx, "/users/"+th.BasicUser.Id+"/posts/ack_batch", `{"post_ids":["`+th.BasicPost.Id+`"]}`)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	response, err = th.Client.DoAPIPost(ctx, "/posts/ids/reactions", `["`+th.BasicPost.Id+`"]`)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/channels/count_by_name?name=town-square", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/channels/autocomplete_names?term=town", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	response, err = th.SystemAdminClient.DoAPIPost(ctx, "/channels/bulk_rename", `{"old_name":"town-square","new_display_name":"Workspace Main"}`)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	require.NotNil(t, th.App.Notification().CheckLicense())

	testLicense := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	testLicense.Features.AutoTranslation = model.NewPointer(true)
	testLicense.Features.OpenId = model.NewPointer(true)
	th.App.Srv().SetLicense(testLicense)
	openIDSettings, openIDErr := openIDProvider.GetSSOSettings(th.Context, openIDConfig, model.ServiceOpenid)
	require.NoError(t, openIDErr)
	require.Equal(t, "https://idp.example/auth", *openIDSettings.AuthEndpoint)

	translationProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/detect":
			_, _ = w.Write([]byte(`[{"confidence":0.99,"language":"en"}]`))
		case "/translate":
			_, _ = w.Write([]byte(`{"translatedText":"مرحبا","detectedLanguage":{"confidence":0.99,"language":"en"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer translationProvider.Close()
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.FeatureFlags.AutoTranslation = true
		cfg.AutoTranslationSettings.Enable = model.NewPointer(true)
		cfg.AutoTranslationSettings.Provider = model.NewPointer("libretranslate")
		cfg.AutoTranslationSettings.LibreTranslate.URL = model.NewPointer(translationProvider.URL)
	})
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE users SET locale = 'ar' WHERE id = $1`, th.BasicUser.Id)
	require.NoError(t, err)
	th.Store.User().ClearCaches()
	translationChannel := th.BasicChannel.DeepCopy()
	translationChannel.AutoTranslation = true
	translationChannel, appErr := th.App.UpdateChannel(th.Context, translationChannel)
	require.Nil(t, appErr)
	translated, appErr := th.App.AutoTranslation().Translate(
		ctx,
		model.TranslationObjectTypePost,
		th.BasicPost.Id,
		translationChannel.Id,
		th.BasicUser.Id,
		th.BasicPost,
	)
	require.Nil(t, appErr)
	require.NotNil(t, translated)
	require.Equal(t, model.TranslationStateReady, translated.State)
	require.Equal(t, "مرحبا", translated.Text)
	storedTranslation, storeErr := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, th.BasicPost.Id, "ar")
	require.NoError(t, storeErr)
	require.Equal(t, "مرحبا", storedTranslation.Text)

	reaction, appErr := th.App.SaveReactionForPost(th.Context, &model.Reaction{
		UserId:    th.BasicUser.Id,
		PostId:    th.BasicPost.Id,
		EmojiName: "smile",
	})
	require.Nil(t, appErr)
	require.NotNil(t, reaction)

	response, err = th.Client.DoAPIPost(ctx, "/posts/ids/reactions", `["`+th.BasicPost.Id+`"]`)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var reactionsByPost map[string][]*model.Reaction
	require.NoError(t, json.NewDecoder(response.Body).Decode(&reactionsByPost))
	require.NoError(t, response.Body.Close())
	require.Len(t, reactionsByPost[th.BasicPost.Id], 1)
	require.Equal(t, "smile", reactionsByPost[th.BasicPost.Id][0].EmojiName)

	response, err = th.Client.DoAPIPost(ctx, "/users/"+th.BasicUser.Id+"/posts/ack_batch", `{"post_ids":["`+th.BasicPost.Id+`"]}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	var batchAcknowledgements []*model.PostAcknowledgement
	require.NoError(t, json.NewDecoder(response.Body).Decode(&batchAcknowledgements))
	require.NoError(t, response.Body.Close())
	require.Len(t, batchAcknowledgements, 1)
	require.Equal(t, th.BasicPost.Id, batchAcknowledgements[0].PostId)
	require.Equal(t, th.BasicUser.Id, batchAcknowledgements[0].UserId)

	auditPath := filepath.Join(logRoot, "audit.log")
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ExperimentalAuditSettings.FileEnabled = model.NewPointer(true)
		cfg.ExperimentalAuditSettings.FileName = model.NewPointer(auditPath)
	})
	require.NoError(t, os.WriteFile(auditPath, []byte("{\"sequence\":1}\n{\"sequence\":2}\n{\"sequence\":3}\n"), 0o600))

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/audits/file/status", "")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)

	var status audit_file.Status
	require.NoError(t, json.NewDecoder(response.Body).Decode(&status))
	require.Equal(t, audit_file.Status{Enabled: true, HasRecords: true}, status)

	var townSquareDisplayName, offTopicDisplayName string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(
		`SELECT displayname FROM channels WHERE teamid = $1 AND name = 'town-square'`, th.BasicTeam.Id,
	).Scan(&townSquareDisplayName))
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(
		`SELECT displayname FROM channels WHERE teamid = $1 AND name = 'off-topic'`, th.BasicTeam.Id,
	).Scan(&offTopicDisplayName))
	require.Equal(t, "المجموعة الرئيسية", townSquareDisplayName)
	require.Equal(t, "المجموعة الفرعية", offTopicDisplayName)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/channels/count_by_name?name=town-square", "")
	require.NoError(t, err)
	var matchingChannelCount struct {
		Count int64 `json:"count"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&matchingChannelCount))
	require.NoError(t, response.Body.Close())
	require.Positive(t, matchingChannelCount.Count)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/channels/autocomplete_names?term=town", "")
	require.NoError(t, err)
	var channelNames []struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&channelNames))
	require.NoError(t, response.Body.Close())
	require.NotEmpty(t, channelNames)
	require.Equal(t, "town-square", channelNames[0].Name)

	response, err = th.SystemAdminClient.DoAPIPost(ctx, "/channels/bulk_rename", `{"old_name":"town-square","new_display_name":"Workspace Main"}`)
	require.NoError(t, err)
	var renamedChannels struct {
		Count int64 `json:"count"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&renamedChannels))
	require.NoError(t, response.Body.Close())
	require.Equal(t, matchingChannelCount.Count, renamedChannels.Count)
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(
		`SELECT displayname FROM channels WHERE teamid = $1 AND name = 'town-square'`, th.BasicTeam.Id,
	).Scan(&townSquareDisplayName))
	require.Equal(t, "Workspace Main", townSquareDisplayName)

	systemBotUser, appErr := th.App.GetUserByUsername(model.BotSystemBotUsername)
	require.Nil(t, appErr)
	require.Equal(t, "نظام", systemBotUser.FirstName)
	contentReviewBotUser, appErr := th.App.GetUserByUsername(model.ContentFlaggingBotUsername)
	require.Nil(t, appErr)
	require.Equal(t, "مراجعة المحتوى", contentReviewBotUser.FirstName)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, th.SystemAdminClient.APIURL+"/workspace-route-that-does-not-exist", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", th.SystemAdminClient.AuthType+" "+th.SystemAdminClient.AuthToken)
	request.Header.Set("Accept-Language", "ar")
	response, err = th.SystemAdminClient.HTTPClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	require.Equal(t, "ar", response.Header.Get("Content-Language"))
	var arabicError struct {
		Message string `json:"message"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&arabicError))
	require.Equal(t, "عذراً، لم نتمكن من العثور على الصفحة.", arabicError.Message)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/audits/file?limit=2", "")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)

	var records []map[string]int
	require.NoError(t, json.NewDecoder(response.Body).Decode(&records))
	require.Equal(t, []map[string]int{{"sequence": 3}, {"sequence": 2}}, records)

	members, appErr := th.App.GetChannelMembersPage(th.Context, th.BasicChannel.Id, 0, 200)
	require.Nil(t, appErr)
	expectedReceipts := make(map[string]int64, len(members))
	for _, member := range members {
		expectedReceipts[member.UserId] = member.LastViewedAt
	}

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/channels/"+th.BasicChannel.Id+"/read_receipts", "")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)

	var receipts map[string]int64
	require.NoError(t, json.NewDecoder(response.Body).Decode(&receipts))
	require.Equal(t, expectedReceipts, receipts)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/data_retention/policies_count", "")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var policyCount struct {
		TotalCount int64 `json:"total_count"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&policyCount))
	require.Zero(t, policyCount.TotalCount)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/data_retention/policies?page=0&per_page=10", "")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var policies model.RetentionPolicyWithTeamAndChannelCountsList
	require.NoError(t, json.NewDecoder(response.Body).Decode(&policies))
	require.Empty(t, policies.Policies)
	require.Zero(t, policies.TotalCount)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/ldap/groups?page=0&per_page=200&q=", "")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var ldapGroups struct {
		Count  int           `json:"count"`
		Groups []model.Group `json:"groups"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&ldapGroups))
	require.Zero(t, ldapGroups.Count)
	require.Empty(t, ldapGroups.Groups)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/file_storage/usage", "")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var storageUsage file_storage.Usage
	require.NoError(t, json.NewDecoder(response.Body).Decode(&storageUsage))
	require.NotEmpty(t, storageUsage.DriverName)
	require.GreaterOrEqual(t, storageUsage.UsedBytes, int64(0))

	initialFileAnalytics, _, err := th.SystemAdminClient.GetAnalyticsOld(ctx, "extra_counts", "")
	require.NoError(t, err)
	initialAttached, found := analyticsCount(initialFileAnalytics, "total_attached_file_count")
	require.True(t, found)
	initialDownloaded, found := analyticsCount(initialFileAnalytics, "total_downloaded_file_count")
	require.True(t, found)

	uploaded, uploadResponse, err := th.Client.UploadFile(ctx, []byte("workspace enterprise file statistics"), th.BasicChannel.Id, "statistics.txt")
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, uploadResponse.StatusCode)
	require.Len(t, uploaded.FileInfos, 1)
	fileID := uploaded.FileInfos[0].Id
	_, createPostResponse, err := th.Client.CreatePost(ctx, &model.Post{
		ChannelId: th.BasicChannel.Id,
		Message:   "file statistics",
		FileIds:   []string{fileID},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createPostResponse.StatusCode)
	_, downloadResponse, err := th.Client.GetFile(ctx, fileID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, downloadResponse.StatusCode)

	require.Eventually(t, func() bool {
		rows, _, analyticsErr := th.SystemAdminClient.GetAnalyticsOld(ctx, "extra_counts", "")
		if analyticsErr != nil {
			return false
		}
		attached, attachedFound := analyticsCount(rows, "total_attached_file_count")
		downloaded, downloadedFound := analyticsCount(rows, "total_downloaded_file_count")
		return attachedFound && downloadedFound && attached >= initialAttached+1 && downloaded >= initialDownloaded+1
	}, 5*time.Second, 50*time.Millisecond)

	response, err = th.SystemAdminClient.DoAPIPost(ctx, "/users/ids", "[]")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var emptyUsers []model.User
	require.NoError(t, json.NewDecoder(response.Body).Decode(&emptyUsers))
	require.Empty(t, emptyUsers)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/terms_of_service", "")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var terms model.TermsOfService
	require.NoError(t, json.NewDecoder(response.Body).Decode(&terms))
	require.Empty(t, terms.Id)
	require.Empty(t, terms.Text)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/brand/image", "")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "image/png", response.Header.Get("Content-Type"))
	brandImage, readErr := io.ReadAll(response.Body)
	require.NoError(t, readErr)
	require.True(t, len(brandImage) > 8)
	require.Equal(t, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, brandImage[:8])

	th.App.UpdateConfig(func(config *model.Config) {
		config.ServiceSettings.EnableAPITeamDeletion = model.NewPointer(true)
	})
	response, err = th.SystemAdminClient.DoAPIDelete(ctx, "/teams/"+teamForDeletion.Id+"?permanent=true")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	_, appErr = th.App.GetTeam(teamForDeletion.Id)
	require.NotNil(t, appErr)

	noEmailUser := &model.User{Username: "roles-no-email-" + model.NewId(), Password: "Test-password1"}
	noEmailUser, createdResponse, err := th.SystemAdminClient.CreateUser(ctx, noEmailUser)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createdResponse.StatusCode)
	validRolesBody := `{"roles":"system_user system_admin"}`
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"roles":"system_user system_admin","unexpected":true}`, http.StatusBadRequest},
		{validRolesBody + ` true`, http.StatusBadRequest},
		{validRolesBody + strings.Repeat(" ", 4096), http.StatusRequestEntityTooLarge},
	} {
		response, requestErr := th.SystemAdminClient.DoAPIPut(ctx, "/users/"+noEmailUser.Id+"/roles", tc.body)
		require.Error(t, requestErr)
		require.Equal(t, tc.status, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
	requireStoredUserState(t, th, noEmailUser.Id, "", model.SystemUserRoleId, 0)

	response, err = th.SystemAdminClient.DoAPIPut(ctx, "/users/"+noEmailUser.Id+"/roles", `{"roles":"system_user system_admin"}`)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	requireStoredUserState(t, th, noEmailUser.Id, "", "system_user system_admin", 0)

	noEmailClient := th.CreateClient()
	_, _, err = noEmailClient.Login(ctx, noEmailUser.Username, "Test-password1")
	require.NoError(t, err)
	newNickname := "no-email-profile"
	patchedUser, patchResponse, err := noEmailClient.PatchUser(ctx, model.Me, &model.UserPatch{Nickname: &newNickname})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, patchResponse.StatusCode)
	require.Equal(t, newNickname, patchedUser.Nickname)
	require.Empty(t, patchedUser.Email)
	requireStoredUserState(t, th, noEmailUser.Id, "", "system_user system_admin", 0)

	fullUpdate := patchedUser.DeepCopy()
	fullUpdate.FirstName = "Full Update"
	updatedUser, updateResponse, err := noEmailClient.UpdateUser(ctx, fullUpdate)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, updateResponse.StatusCode)
	require.Equal(t, "Full Update", updatedUser.FirstName)
	require.Empty(t, updatedUser.Email)
	requireStoredUserState(t, th, noEmailUser.Id, "", "system_user system_admin", 0)

	emailUser := &model.User{
		Username: "clear-email-" + model.NewId(),
		Email:    "clear-email-" + model.NewId() + "@example.com",
		Password: "Test-password1",
	}
	emailUser, createdResponse, err = th.SystemAdminClient.CreateUser(ctx, emailUser)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createdResponse.StatusCode)

	emailClient := th.CreateClient()
	_, _, err = emailClient.Login(ctx, emailUser.Username, "Test-password1")
	require.NoError(t, err)
	emptyEmail := ""
	patchedUser, patchResponse, err = emailClient.PatchUser(ctx, model.Me, &model.UserPatch{Email: &emptyEmail})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, patchResponse.StatusCode)
	require.Empty(t, patchedUser.Email)
	requireStoredUserState(t, th, emailUser.Id, "", model.SystemUserRoleId, 0)

	openIDAuthData := "openid-admin-edit-" + model.NewId()
	_, authStoreErr := th.Store.User().UpdateAuthData(emailUser.Id, model.ServiceOpenid, &openIDAuthData, "", false)
	require.NoError(t, authStoreErr)
	th.Store.User().ClearCaches()
	updatedFirstName := "OpenID Admin Edit"
	patchedUser, patchResponse, err = th.SystemAdminClient.PatchUser(ctx, emailUser.Id, &model.UserPatch{FirstName: &updatedFirstName})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, patchResponse.StatusCode)
	require.Equal(t, updatedFirstName, patchedUser.FirstName)
	require.Empty(t, patchedUser.Email)
	requireStoredUserState(t, th, emailUser.Id, "", model.SystemUserRoleId, 0)

	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"active":false} true`, http.StatusBadRequest},
		{`{"active":false}` + strings.Repeat(" ", 4096), http.StatusRequestEntityTooLarge},
	} {
		response, requestErr := th.SystemAdminClient.DoAPIPut(ctx, "/users/"+noEmailUser.Id+"/active", tc.body)
		require.Error(t, requestErr)
		require.Equal(t, tc.status, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
	requireStoredUserState(t, th, noEmailUser.Id, "", "system_user system_admin", 0)

	response, err = th.SystemAdminClient.DoAPIPut(ctx, "/users/"+noEmailUser.Id+"/active", `{"active":false}`)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	requireStoredUserState(t, th, noEmailUser.Id, "", "system_user system_admin", 1)

	response, err = th.SystemAdminClient.DoAPIPut(ctx, "/users/"+noEmailUser.Id+"/active", `{"active":true}`)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	requireStoredUserState(t, th, noEmailUser.Id, "", "system_user system_admin", 0)

	deletedNoEmailUser := &model.User{Username: "delete-no-email-" + model.NewId(), Password: "Test-password1"}
	deletedNoEmailUser, createdResponse, err = th.SystemAdminClient.CreateUser(ctx, deletedNoEmailUser)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createdResponse.StatusCode)

	response, err = th.SystemAdminClient.DoAPIDelete(ctx, "/users/"+deletedNoEmailUser.Id)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var softDeletedUserCount int
	var softDeletedAt int64
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(
		`SELECT COUNT(*), MAX(deleteat) FROM users WHERE id = $1`, deletedNoEmailUser.Id,
	).Scan(&softDeletedUserCount, &softDeletedAt))
	require.Equal(t, 1, softDeletedUserCount)
	require.Positive(t, softDeletedAt)

	response, err = th.SystemAdminClient.DoAPIPut(ctx, "/users/"+th.SystemAdminUser.Id+"/preferences", string(legacyPreferenceJSON))
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	storedPreference, storeErr := th.Store.Preference().Get(
		th.SystemAdminUser.Id,
		model.PreferenceCategorySidebarSettings,
		model.PreferenceLimitVisibleDmsGms,
	)
	require.NoError(t, storeErr)
	require.Equal(t, "0", storedPreference.Value)

	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE users SET locale = 'ar' WHERE id = $1`, th.SystemAdminUser.Id)
	require.NoError(t, err)
	th.Store.User().ClearCaches()
	notificationMessage, appErr := th.App.Notification().GetNotificationMessage(th.Context.WithAcceptLanguage("ar"), &model.PushNotificationAck{
		Id:               model.NewId(),
		NotificationType: model.PushTypeMessage,
		PostId:           th.BasicPost.Id,
		IsIdLoaded:       true,
	}, th.SystemAdminUser.Id)
	require.Nil(t, appErr)
	require.Equal(t, "لقد تلقيت رسالة جديدة.", notificationMessage.Message)
	require.Equal(t, th.BasicPost.Id, notificationMessage.PostId)
	require.True(t, notificationMessage.IsIdLoaded)

	onlineOnlyUser := th.CreateUser(t)
	require.NoError(t, th.Store.Status().SaveOrUpdate(&model.Status{
		UserId:         onlineOnlyUser.Id,
		Status:         model.StatusOnline,
		LastActivityAt: model.GetMillis(),
	}))
	offlineReportUser := th.CreateUser(t)
	require.NoError(t, th.Store.Status().SaveOrUpdate(&model.Status{
		UserId:         offlineReportUser.Id,
		Status:         model.StatusOffline,
		LastActivityAt: 1,
	}))
	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/reports/users?show_online_only=true&page_size=100", "")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var onlineReports []*model.UserReport
	require.NoError(t, json.NewDecoder(response.Body).Decode(&onlineReports))
	onlineReportIDs := make(map[string]bool, len(onlineReports))
	for _, report := range onlineReports {
		onlineReportIDs[report.Id] = true
	}
	require.True(t, onlineReportIDs[onlineOnlyUser.Id])
	require.False(t, onlineReportIDs[offlineReportUser.Id])

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/reports/users/count?show_online_only=true", "")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var onlineReportCount int64
	require.NoError(t, json.NewDecoder(response.Body).Decode(&onlineReportCount))
	require.EqualValues(t, len(onlineReports), onlineReportCount)

	inactiveUser := th.CreateUser(t)
	require.NoError(t, th.Store.Status().SaveOrUpdate(&model.Status{
		UserId:         inactiveUser.Id,
		Status:         model.StatusOffline,
		LastActivityAt: 1,
	}))
	response, err = th.SystemAdminClient.DoAPIPost(ctx, "/users/deactivate/inactive", `{"days":30}`)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var inactiveResult struct {
		Count int64 `json:"count"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&inactiveResult))
	require.Positive(t, inactiveResult.Count)
	requireStoredUserDeletedState(t, th, inactiveUser.Id, true)
	requireStoredUserDeletedState(t, th, th.SystemAdminUser.Id, false)
	requireStoredUserDeletedState(t, th, systemBotUser.Id, false)
}

func TestCustomSessionRoutesEnforceMFA(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense("mfa"))
	th.App.UpdateConfig(func(config *model.Config) {
		config.ServiceSettings.EnableMultifactorAuthentication = model.NewPointer(true)
		config.ServiceSettings.EnforceMultifactorAuthentication = model.NewPointer(true)
	})

	response, err := th.SystemAdminClient.DoAPIGet(context.Background(), "/audits/file/status", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
}

func TestDeactivateInactiveUsersRevokesExistingSessions(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	ctx := context.Background()

	client := th.CreateClient()
	th.LoginBasic2WithClient(t, client)
	require.NoError(t, th.Store.Status().SaveOrUpdate(&model.Status{
		UserId: th.BasicUser2.Id, Status: model.StatusOffline, LastActivityAt: 1,
	}))

	response, err := th.SystemAdminClient.DoAPIPost(ctx, "/users/deactivate/inactive", `{"days":30}`)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	var deleteAt int64
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRowContext(ctx,
		`SELECT deleteat FROM users WHERE id = $1`, th.BasicUser2.Id,
	).Scan(&deleteAt))
	require.Positive(t, deleteAt)
	var pendingCleanup int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRowContext(ctx,
		`SELECT count(*) FROM WorkspaceInactiveUserSessionCleanup WHERE UserId=$1`, th.BasicUser2.Id,
	).Scan(&pendingCleanup))
	require.Zero(t, pendingCleanup, "successful revocation must clear the durable cleanup intent")

	_, getMeResponse, err := client.GetMe(ctx, "")
	require.Error(t, err)
	require.Equal(t, http.StatusUnauthorized, getMeResponse.StatusCode)
}

func TestDeletionRequiresExplicitPermanentAndEnabledConfig(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	ctx := context.Background()

	t.Run("ordinary user DELETE remains a soft deactivation", func(t *testing.T) {
		user, _, err := th.SystemAdminClient.CreateUser(ctx, &model.User{
			Username: "soft-delete-" + model.NewId(),
			Password: "Test-password1",
		})
		require.NoError(t, err)

		response, err := th.SystemAdminClient.DoAPIDelete(ctx, "/users/"+user.Id)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())

		var count int
		var deleteAt int64
		require.NoError(t, th.Store.GetInternalMasterDB().QueryRowContext(ctx,
			`SELECT COUNT(*), MAX(deleteat) FROM users WHERE id = $1`, user.Id,
		).Scan(&count, &deleteAt))
		require.Equal(t, 1, count)
		require.Positive(t, deleteAt)
	})

	t.Run("permanent user deletion obeys EnableAPIUserDeletion", func(t *testing.T) {
		user, _, err := th.SystemAdminClient.CreateUser(ctx, &model.User{
			Username: "blocked-user-delete-" + model.NewId(),
			Password: "Test-password1",
		})
		require.NoError(t, err)
		th.App.UpdateConfig(func(config *model.Config) {
			config.ServiceSettings.EnableAPIUserDeletion = model.NewPointer(false)
		})

		response, err := th.SystemAdminClient.DoAPIDelete(ctx, "/users/"+user.Id+"?permanent=true")
		require.Error(t, err)
		require.Equal(t, http.StatusUnauthorized, response.StatusCode)

		var count int
		require.NoError(t, th.Store.GetInternalMasterDB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM users WHERE id = $1 AND deleteat = 0`, user.Id,
		).Scan(&count))
		require.Equal(t, 1, count)
	})

	t.Run("permanent team deletion obeys EnableAPITeamDeletion", func(t *testing.T) {
		team := th.CreateTeam(t)
		th.App.UpdateConfig(func(config *model.Config) {
			config.ServiceSettings.EnableAPITeamDeletion = model.NewPointer(false)
		})

		response, err := th.SystemAdminClient.DoAPIDelete(ctx, "/teams/"+team.Id+"?permanent=true")
		require.Error(t, err)
		require.Equal(t, http.StatusUnauthorized, response.StatusCode)

		var count int
		require.NoError(t, th.Store.GetInternalMasterDB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM teams WHERE id = $1 AND deleteat = 0`, team.Id,
		).Scan(&count))
		require.Equal(t, 1, count)
	})
}

func TestChannelResponsesIncludeLatestVisibleUserPost(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	ctx := context.Background()

	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypeOpen)
	firstPost := th.CreatePostWithClient(t, th.Client, channel)
	_, err := th.Store.GetInternalMasterDB().Exec(`INSERT INTO postspriority(postid,channelid,priority,requestedack,persistentnotifications)
		VALUES ($1,$2,'urgent',true,false)`, firstPost.Id, channel.Id)
	require.NoError(t, err)
	_, err = th.Store.GetInternalMasterDB().Exec(`INSERT INTO postacknowledgements(postid,userid,channelid,acknowledgedat)
		VALUES ($1,$2,$3,123)`, firstPost.Id, th.BasicUser.Id, channel.Id)
	require.NoError(t, err)

	// A newer system post must not replace the user-authored sidebar preview.
	time.Sleep(2 * time.Millisecond)
	_, err = th.Store.Post().Save(th.Context, &model.Post{
		UserId:    th.BasicUser.Id,
		ChannelId: channel.Id,
		Message:   "system message",
		Type:      model.PostTypeJoinChannel,
	})
	require.NoError(t, err)

	assertLastPost := func(expected *model.Post) {
		t.Helper()
		response, err := th.Client.DoAPIGet(ctx, "/users/me/teams/"+th.BasicTeam.Id+"/channels", "")
		require.NoError(t, err)
		defer response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)

		var channels []struct {
			ID         string      `json:"id"`
			LastPostID string      `json:"last_post_id"`
			LastPost   *model.Post `json:"last_post"`
		}
		require.NoError(t, json.NewDecoder(response.Body).Decode(&channels))
		for _, candidate := range channels {
			if candidate.ID != channel.Id {
				continue
			}
			require.Equal(t, expected.Id, candidate.LastPostID)
			require.NotNil(t, candidate.LastPost)
			require.Equal(t, expected.Id, candidate.LastPost.Id)
			require.Equal(t, expected.Message, candidate.LastPost.Message)
			if expected.Id == firstPost.Id {
				require.NotNil(t, candidate.LastPost.Metadata)
				require.NotNil(t, candidate.LastPost.Metadata.Priority)
				require.Equal(t, "urgent", *candidate.LastPost.Metadata.Priority.Priority)
				require.Len(t, candidate.LastPost.Metadata.Acknowledgements, 1)
				require.Equal(t, th.BasicUser.Id, candidate.LastPost.Metadata.Acknowledgements[0].UserId)
			}
			return
		}
		t.Fatalf("channel %s was missing from response", channel.Id)
	}

	assertLastPost(firstPost)
	updatedMessage := "edited channel preview"
	updatedPost, _, err := th.Client.PatchPost(ctx, firstPost.Id, &model.PostPatch{Message: &updatedMessage})
	require.NoError(t, err)
	assertLastPost(updatedPost)

	// Soft-deleted posts are omitted, so the preview falls back to the previous
	// non-system post just as it does in the reference implementation.
	time.Sleep(2 * time.Millisecond)
	latestPost := th.CreatePostWithClient(t, th.Client, channel)
	assertLastPost(latestPost)
	_, err = th.Client.DeletePost(ctx, latestPost.Id)
	require.NoError(t, err)
	assertLastPost(updatedPost)

	_, err = th.Client.DeletePost(ctx, firstPost.Id)
	require.NoError(t, err)
	response, err := th.Client.DoAPIGet(ctx, "/channels/"+channel.Id, "")
	require.NoError(t, err)
	defer response.Body.Close()
	var emptyPreview struct {
		LastPostID string      `json:"last_post_id"`
		LastPost   *model.Post `json:"last_post"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&emptyPreview))
	require.Empty(t, emptyPreview.LastPostID)
	require.Nil(t, emptyPreview.LastPost)
}

func requireStoredUserState(t *testing.T, th *api4.TestHelper, userID, email, roles string, deleted int) {
	t.Helper()

	var storedEmail, storedRoles string
	var deleteAt int64
	err := th.Store.GetInternalMasterDB().QueryRow(
		`SELECT email, roles, deleteat FROM users WHERE id = $1`, userID,
	).Scan(&storedEmail, &storedRoles, &deleteAt)
	require.NoError(t, err)
	require.Equal(t, email, storedEmail)
	require.Equal(t, roles, storedRoles)
	if deleted == 0 {
		require.Zero(t, deleteAt)
	} else {
		require.Positive(t, deleteAt)
	}
}

func requireStoredUserDeletedState(t *testing.T, th *api4.TestHelper, userID string, deleted bool) {
	t.Helper()

	var deleteAt int64
	err := th.Store.GetInternalMasterDB().QueryRow(`SELECT deleteat FROM users WHERE id = $1`, userID).Scan(&deleteAt)
	require.NoError(t, err)
	if deleted {
		require.Positive(t, deleteAt)
	} else {
		require.Zero(t, deleteAt)
	}
}

func analyticsCount(rows model.AnalyticsRows, name string) (int64, bool) {
	for _, row := range rows {
		if row.Name == name {
			return int64(row.Value), true
		}
	}
	return 0, false
}
