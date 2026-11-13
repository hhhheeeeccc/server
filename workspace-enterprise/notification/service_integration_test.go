package notification

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	app.RegisterNotificationInterface(func(a *app.App) einterfaces.NotificationInterface { return New(a) })
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestIDLoadedNotificationUsesPostAuthor(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.IDLoadedPushNotifications = model.NewPointer(true)
	require.True(t, th.Server.SetLicense(license))
	ack := &model.PushNotificationAck{Id: model.NewId(), PostId: th.BasicPost.Id, NotificationType: model.PushTypeMessage, IsIdLoaded: true}
	message, appErr := New(th.App).GetNotificationMessage(th.Context, ack, th.BasicUser2.Id)
	require.Nil(t, appErr)
	require.Equal(t, th.BasicPost.UserId, message.SenderId)
	require.NotEqual(t, th.BasicUser2.Id, message.SenderId)
	require.Equal(t, th.BasicPost.ChannelId, message.ChannelId)
	license.Features.IDLoadedPushNotifications = model.NewPointer(false)
	require.True(t, th.Server.SetLicense(license))
	_, appErr = New(th.App).GetNotificationMessage(th.Context, ack, th.BasicUser2.Id)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusForbidden, appErr.StatusCode)
	th.Server.SetLicense(nil)
	_, appErr = New(th.App).GetNotificationMessage(th.Context, ack, th.BasicUser2.Id)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusForbidden, appErr.StatusCode)
}

func TestIDLoadedNotificationAckAPI(t *testing.T) {
	var forwarded atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != model.APIURLSuffixV1+"/ack" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		forwarded.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendPushNotifications = new(true)
		cfg.EmailSettings.PushNotificationServer = &proxy.URL
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.IDLoadedPushNotifications = model.NewPointer(true)
	require.True(t, th.Server.SetLicense(license))

	ack := model.PushNotificationAck{Id: model.NewId(), PostId: th.BasicPost.Id, NotificationType: model.PushTypeMessage, IsIdLoaded: true}
	response, err := th.Client.DoAPIPostJSON(th.Context.Context(), "/notifications/ack", ack)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var message model.PushNotification
	require.NoError(t, json.NewDecoder(response.Body).Decode(&message))
	require.Equal(t, ack.Id, message.AckId)
	require.Equal(t, th.BasicPost.Id, message.PostId)
	require.Equal(t, th.BasicPost.UserId, message.SenderId)
	require.True(t, message.IsIdLoaded)
	require.EqualValues(t, 1, forwarded.Load())

	privateChannel := th.CreateChannelWithClient(t, th.SystemAdminClient, model.ChannelTypePrivate)
	privatePost := th.CreatePostWithClient(t, th.SystemAdminClient, privateChannel)
	ack.PostId = privatePost.Id
	response, err = th.Client.DoAPIPostJSON(th.Context.Context(), "/notifications/ack", ack)
	require.Error(t, err)
	require.NotNil(t, response)
	defer response.Body.Close()
	require.Equal(t, http.StatusForbidden, response.StatusCode)
}
