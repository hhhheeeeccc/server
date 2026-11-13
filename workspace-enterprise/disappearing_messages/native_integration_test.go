//go:build integration

package disappearing_messages

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	u, err := url.Parse(os.Getenv("TEST_DATABASE_POSTGRESQL_DSN"))
	if err != nil || u.Host != "127.0.0.1:55439" {
		panic("integration tests require disposable PostgreSQL on 127.0.0.1:55439")
	}
	RegisterCommands()
	RegisterPermissions()
	app.RegisterComplianceInterface(func(a *app.App) einterfaces.ComplianceInterface { Register(a); return nil })
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestNativeVisibleMessagesExpireFromSend(t *testing.T) {
	ctx := context.Background()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.FileSettings.EnablePublicLink = new(true)
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	th.AddPermissionToRole(t, permissionManageDisappearingMessages.Id, model.SystemUserRoleId)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	channel, _, err := th.Client.CreateDirectChannel(ctx, th.BasicUser.Id, th.BasicUser2.Id)
	require.NoError(t, err)
	recipient := th.CreateClient()
	_, _, err = recipient.Login(ctx, th.BasicUser2.Email, th.BasicUser2.Password)
	require.NoError(t, err)
	secondDevice := th.CreateClient()
	_, _, err = secondDevice.Login(ctx, th.BasicUser2.Email, th.BasicUser2.Password)
	require.NoError(t, err)
	path := "/workspace/channels/" + channel.Id + "/disappearing"
	// Old messages are outside the policy, even if their conversation opts in.
	old, _, err := th.Client.CreatePost(ctx, &model.Post{ChannelId: channel.Id, Message: "رسالة سابقة"})
	require.NoError(t, err)
	resp, err := th.Client.DoAPIRequestWithHeaders(ctx, http.MethodPut, path, `{"duration_seconds":86400}`, nil)
	require.NoError(t, err)
	resp.Body.Close()
	resp, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodGet, path, "", nil)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp, err = th.Client.DoAPIRequestWithHeaders(ctx, http.MethodPut, path, `{"duration_seconds":60}`, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	for _, body := range []string{`{}`, `{"duration_seconds":null}`, `{"duration_seconds":86400,"enabled":true}`, `{"duration_seconds":86400} {}`} {
		resp, err = th.Client.DoAPIRequestWithHeaders(ctx, http.MethodPut, path, body, nil)
		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	}
	upload, _, err := th.Client.UploadFile(ctx, []byte("expiry integration bytes"), channel.Id, "expiry.txt")
	require.NoError(t, err)
	fileID := upload.FileInfos[0].Id
	post, _, err := th.Client.CreatePost(ctx, &model.Post{ChannelId: channel.Id, Message: "رسالة عادية تظهر مباشرة", FileIds: model.StringArray{fileID}})
	require.NoError(t, err)
	require.Equal(t, model.PostTypeDefault, post.Type)
	visible, _, err := recipient.GetPost(ctx, post.Id, "")
	require.NoError(t, err)
	require.Equal(t, post.Message, visible.Message, "recipient does not need a reveal/read-receipt request")
	resp, err = recipient.DoAPIRequestWithHeaders(ctx, http.MethodGet, "/files/"+fileID, "", nil)
	require.NoError(t, err)
	require.Contains(t, resp.Header.Get("Cache-Control"), "no-store")
	resp.Body.Close()
	for _, suffix := range []string{"/link", "/public?h=anything"} {
		resp, err = recipient.DoAPIRequestWithHeaders(ctx, http.MethodGet, "/files/"+fileID+suffix, "", nil)
		require.Error(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	}
	reply, _, err := th.Client.ExecuteCommandWithTeam(ctx, channel.Id, th.BasicTeam.Id, "/اختفاء حالة")
	require.NoError(t, err)
	require.Contains(t, reply.Text, "24 ساعة")
	reply, _, err = th.Client.ExecuteCommandWithTeam(ctx, channel.Id, th.BasicTeam.Id, "/اختفاء 60")
	require.NoError(t, err)
	require.Contains(t, reply.Text, "ليست عدد ثوانٍ")
	reply, _, err = th.Client.ExecuteCommandWithTeam(ctx, channel.Id, th.BasicTeam.Id, "/اختفاء إيقاف")
	require.NoError(t, err)
	keep, _, err := th.Client.CreatePost(ctx, &model.Post{ChannelId: channel.Id, Message: "تبقى بعد إيقاف الوضع"})
	require.NoError(t, err)
	value, ok := services.Load(th.Server)
	require.True(t, ok)
	s := value.(*Service)
	var frozen int64
	require.NoError(t, s.repo.db.QueryRow(`SELECT expires_at-sent_at FROM workspace_message_expiry.messages WHERE post_id=$1`, post.Id).Scan(&frozen))
	require.Equal(t, int64(86400000), frozen)
	ws := th.CreateConnectedWebSocketClientWithClient(t, secondDevice)
	// Advance the stored test fixture deadline, never the production duration
	// allow-list. No real chat data or 24-hour wait is needed.
	_, err = s.repo.db.Exec(`UPDATE workspace_message_expiry.messages SET expires_at=1,next_attempt_at=1 WHERE post_id=$1`, post.Id)
	require.NoError(t, err)
	for _, suffix := range []string{"", "/preview", "/thumbnail", "/info"} {
		resp, err = recipient.DoAPIRequestWithHeaders(ctx, http.MethodGet, "/files/"+fileID+suffix, "", nil)
		require.Error(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	}
	require.Eventually(t, func() bool {
		var completed int64
		return s.repo.db.QueryRow(`SELECT completed_at FROM workspace_message_expiry.messages WHERE post_id=$1`, post.Id).Scan(&completed) == nil && completed > 0
	}, 10*time.Second, 50*time.Millisecond)
	for _, client := range []*model.Client4{th.Client, recipient, secondDevice} {
		deleted, _, getErr := client.GetPost(ctx, post.Id, "")
		// The native API may return a tombstone or 404, but no usable content.
		if getErr == nil {
			require.NotZero(t, deleted.DeleteAt)
		}
		for _, p := range []*model.Post{old, keep} {
			current, _, getErr := client.GetPost(ctx, p.Id, "")
			require.NoError(t, getErr)
			require.Zero(t, current.DeleteAt)
		}
	}
	select {
	case event := <-ws.EventChannel:
		for event != nil && event.EventType() != model.WebsocketEventPostDeleted {
			select {
			case event = <-ws.EventChannel:
			case <-time.After(5 * time.Second):
				t.Fatal("missing native post_deleted event")
			}
		}
		require.NotNil(t, event)
		data, err := json.Marshal(event.GetData())
		require.NoError(t, err)
		require.True(t, strings.Contains(string(data), post.Id))
	case <-time.After(5 * time.Second):
		t.Fatal("missing native post_deleted event")
	}
	// A missing runtime service still recognizes the Arabic command, instead
	// of offering to send it as a normal message on a second click.
	services.Delete(th.Server)
	t.Cleanup(func() { services.Store(th.Server, s) })
	reply, _, err = th.Client.ExecuteCommandWithTeam(ctx, channel.Id, th.BasicTeam.Id, "/اختفاء حالة")
	require.NoError(t, err)
	require.Contains(t, reply.Text, "لم تُجهّز")
}
