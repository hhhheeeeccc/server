package channel_read_receipts

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
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
		return nil
	})
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestReadReceiptsRequireLicenseAndChannelAccess(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	path := "/channels/" + th.BasicChannel.Id + "/read_receipts"

	response, requestErr := th.Client.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodGet, path, "", nil)
	require.Error(t, requestErr)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())

	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	response, requestErr = th.Client.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodGet, path, "", nil)
	require.NoError(t, requestErr)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var receipts map[string]int64
	require.NoError(t, json.NewDecoder(response.Body).Decode(&receipts))
	require.NoError(t, response.Body.Close())
	require.Contains(t, receipts, th.BasicUser.Id)
	require.Contains(t, receipts, th.BasicUser2.Id)

	// The endpoint must include members beyond its 200-row database page.
	dummyIDs := make([]string, maxMembers+1)
	tx, err := th.Store.GetInternalMasterDB().Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	for index := range dummyIDs {
		dummyIDs[index] = model.NewId()
		_, err = tx.Exec(`INSERT INTO channelmembers(channelid, userid, lastviewedat) VALUES($1, $2, $3)`, th.BasicChannel.Id, dummyIDs[index], index+1)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	t.Cleanup(func() {
		_, cleanupErr := th.Store.GetInternalMasterDB().Exec(`DELETE FROM channelmembers WHERE channelid=$1 AND userid=ANY($2)`, th.BasicChannel.Id, pq.Array(dummyIDs))
		require.NoError(t, cleanupErr)
	})
	response, requestErr = th.Client.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodGet, path, "", nil)
	require.NoError(t, requestErr)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, json.NewDecoder(response.Body).Decode(&receipts))
	require.NoError(t, response.Body.Close())
	for index, userID := range dummyIDs {
		require.Equal(t, int64(index+1), receipts[userID])
	}

	privatePath := "/channels/" + th.BasicPrivateChannel2.Id + "/read_receipts"
	response, requestErr = th.Client.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodGet, privatePath, "", nil)
	require.Error(t, requestErr)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

func TestPostingDoesNotMarkOtherMembersRead(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	th.App.SetStatusOnline(th.BasicUser2.Id, false)
	require.Nil(t, th.App.SetActiveChannel(th.Context, th.BasicUser2.Id, th.BasicChannel.Id))
	before, appErr := th.App.GetChannelMember(th.Context, th.BasicChannel.Id, th.BasicUser2.Id)
	require.Nil(t, appErr)
	_, _, err := th.Client.CreatePost(th.Context.Context(), &model.Post{
		ChannelId: th.BasicChannel.Id,
		Message:   "Receiving a message does not prove that another user read it",
	})
	require.NoError(t, err)
	after, appErr := th.App.GetChannelMember(th.Context, th.BasicChannel.Id, th.BasicUser2.Id)
	require.Nil(t, appErr)
	require.Equal(t, before.LastViewedAt, after.LastViewedAt)
}

func TestViewRequestsPublishSavedReceipts(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableChannelViewedMessages = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	ws := th.CreateConnectedWebSocketClient(t)
	second := th.CreateClient()
	_, _, err := second.Login(th.Context.Context(), th.BasicUser2.Email, th.BasicUser2.Password)
	require.NoError(t, err)
	for _, client := range []*model.Client4{th.Client, second} {
		_, _, err := client.ViewChannel(th.Context.Context(), model.Me, &model.ChannelView{ChannelId: th.BasicChannel.Id})
		require.NoError(t, err)
	}
	want := make(map[string]int64)
	for _, userID := range []string{th.BasicUser.Id, th.BasicUser2.Id} {
		member, appErr := th.App.GetChannelMember(th.Context, th.BasicChannel.Id, userID)
		require.Nil(t, appErr)
		want[userID] = member.LastViewedAt
	}
	got := make(map[string]int64)
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for len(got) < len(want) {
		select {
		case event := <-ws.EventChannel:
			if event == nil || event.EventType() != channelMemberViewedEvent {
				continue
			}
			require.Equal(t, th.BasicChannel.Id, event.GetBroadcast().ChannelId)
			data := event.GetData()
			if receipts, ok := data["receipts"].(map[string]any); ok {
				for userID, at := range receipts {
					got[userID] = int64(at.(float64))
				}
			} else {
				got[data["user_id"].(string)] = int64(data["last_viewed_at"].(float64))
			}
		case <-timeout.C:
			t.Fatalf("saved read receipts not received: got %v, want %v", got, want)
		}
	}
	require.Equal(t, want, got)
}

func TestReactionOnlyMarksLatestPostRead(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableChannelViewedMessages = new(true)
	}).InitBasic(t)
	latest, _, err := th.Client.CreatePost(th.Context.Context(), &model.Post{
		ChannelId: th.BasicChannel.Id,
		Message:   "Latest message for reaction receipt",
	})
	require.NoError(t, err)
	// Use deterministic fixture timestamps instead of sleeping between posts.
	oldAt, latestAt := int64(1000), int64(2000)
	db := th.Store.GetInternalMasterDB()
	_, err = db.Exec(`UPDATE posts SET createat=$1 WHERE channelid=$2`, oldAt, th.BasicChannel.Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE posts SET createat=$1 WHERE id=$2`, latestAt, latest.Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE channels SET lastpostat=$1 WHERE id=$2`, latestAt, th.BasicChannel.Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE channelmembers SET lastviewedat=0 WHERE channelid=$1 AND userid=$2`, th.BasicChannel.Id, th.BasicUser.Id)
	require.NoError(t, err)
	readAt := func() int64 {
		var at int64
		require.NoError(t, db.QueryRow(`SELECT lastviewedat FROM channelmembers WHERE channelid=$1 AND userid=$2`, th.BasicChannel.Id, th.BasicUser.Id).Scan(&at))
		return at
	}
	// The reaction itself remains permitted without the Enterprise receipt feature.
	_, _, err = th.Client.SaveReaction(th.Context.Context(), &model.Reaction{UserId: th.BasicUser.Id, PostId: latest.Id, EmojiName: "smile"})
	require.NoError(t, err)
	require.Zero(t, readAt())
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	_, _, err = th.Client.SaveReaction(th.Context.Context(), &model.Reaction{UserId: th.BasicUser.Id, PostId: th.BasicPost.Id, EmojiName: "smile"})
	require.NoError(t, err)
	require.Zero(t, readAt(), "an old-post reaction must not clear newer unread messages")
	_, _, err = th.Client.SaveReaction(th.Context.Context(), &model.Reaction{UserId: th.BasicUser.Id, PostId: latest.Id, EmojiName: "thumbsup"})
	require.NoError(t, err)
	// The middleware runs after the HTTP response is written, so wait for its
	// saved result rather than assuming the client observed completion of it.
	require.Eventually(t, func() bool { return readAt() == latestAt }, 5*time.Second, 10*time.Millisecond)
}

func TestReactionDoesNotClearUnreadPostWithSameTimestamp(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableChannelViewedMessages = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	other, _, err := th.Client.CreatePost(th.Context.Context(), &model.Post{
		ChannelId: th.BasicChannel.Id,
		Message:   "Another message in the same millisecond",
	})
	require.NoError(t, err)
	db := th.Store.GetInternalMasterDB()
	at := int64(2000)
	_, err = db.Exec(`UPDATE posts SET createat=1000 WHERE channelid=$1`, th.BasicChannel.Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE posts SET createat=$1 WHERE id IN ($2,$3)`, at, th.BasicPost.Id, other.Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE channels SET lastpostat=$1 WHERE id=$2`, at, th.BasicChannel.Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE channelmembers SET lastviewedat=0, msgcount=0, mentioncount=1 WHERE channelid=$1 AND userid=$2`, th.BasicChannel.Id, th.BasicUser.Id)
	require.NoError(t, err)

	// Serve synchronously so the assertion waits for post-response middleware,
	// including the absence of a receipt, without a sleep or timing assumption.
	r := httptest.NewRequest(http.MethodPost, saveReactionPath, strings.NewReader(fmt.Sprintf(`{"user_id":%q,"post_id":%q,"emoji_name":"smile"}`, th.BasicUser.Id, th.BasicPost.Id)))
	r.Header.Set("Authorization", "Bearer "+th.Client.AuthToken)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	th.Server.RootRouter.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var viewedAt, messageCount, mentionCount int64
	require.NoError(t, db.QueryRow(`SELECT lastviewedat,msgcount,mentioncount FROM channelmembers WHERE channelid=$1 AND userid=$2`, th.BasicChannel.Id, th.BasicUser.Id).Scan(&viewedAt, &messageCount, &mentionCount))
	require.Zero(t, viewedAt, "one reaction does not prove reading the other post with the same timestamp")
	require.Zero(t, messageCount)
	require.EqualValues(t, 1, mentionCount)
}

func TestThreadReceiptsDoNotInheritChannelViews(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableChannelViewedMessages = new(true)
		cfg.ServiceSettings.CollapsedThreads = new(model.CollapsedThreadsAlwaysOn)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	root, _, err := th.Client.CreatePost(th.Context.Context(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "A separate thread receipt"})
	require.NoError(t, err)
	reply, _, err := th.Client.CreatePost(th.Context.Context(), &model.Post{ChannelId: th.BasicChannel.Id, RootId: root.Id, Message: "An unopened reply"})
	require.NoError(t, err)
	db := th.Store.GetInternalMasterDB()
	_, err = db.Exec(`UPDATE threadmemberships SET lastviewed=0 WHERE postid=$1 AND userid=$2`, root.Id, th.BasicUser.Id)
	require.NoError(t, err)
	_, _, err = th.Client.ViewChannel(th.Context.Context(), model.Me, &model.ChannelView{ChannelId: th.BasicChannel.Id, CollapsedThreadsSupported: true})
	require.NoError(t, err)
	ws := th.CreateConnectedWebSocketClient(t)
	path := "/channels/" + th.BasicChannel.Id + "/threads/" + root.Id + "/read_receipts"
	read := func() map[string]int64 {
		response, err := th.Client.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodGet, path, "", nil)
		require.NoError(t, err)
		defer response.Body.Close()
		var receipts map[string]int64
		require.NoError(t, json.NewDecoder(response.Body).Decode(&receipts))
		return receipts
	}
	require.Less(t, read()[th.BasicUser.Id], reply.CreateAt, "a channel view must not mark collapsed replies read")
	_, _, err = th.Client.UpdateThreadReadForUser(th.Context.Context(), th.BasicUser.Id, th.BasicTeam.Id, root.Id, reply.CreateAt+100)
	require.NoError(t, err)
	require.GreaterOrEqual(t, read()[th.BasicUser.Id], reply.CreateAt)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	found := false
	for !found {
		select {
		case event := <-ws.EventChannel:
			if event == nil || event.EventType() != channelMemberViewedEvent || event.GetData()["thread_id"] != root.Id {
				continue
			}
			require.Equal(t, th.BasicChannel.Id, event.GetBroadcast().ChannelId)
			require.Equal(t, th.BasicUser.Id, event.GetData()["user_id"])
			require.GreaterOrEqual(t, int64(event.GetData()["last_viewed_at"].(float64)), reply.CreateAt)
			found = true
		case <-deadline.C:
			t.Fatal("thread receipt was not broadcast after opening the thread")
		}
	}

	wrongChannel := "/channels/" + th.BasicChannel2.Id + "/threads/" + root.Id + "/read_receipts"
	response, err := th.Client.DoAPIRequestWithHeaders(th.Context.Context(), http.MethodGet, wrongChannel, "", nil)
	require.Error(t, err)
	require.NoError(t, response.Body.Close())
}
