package enterprise_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

// Channel metadata must enforce the same content boundary as the posts API.
func TestPerformancePreviewRespectsContentPermission(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ComplianceSettings.Enable = model.NewPointer(true)
	})
	ctx := context.Background()
	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypeOpen)
	post := th.CreatePostWithClient(t, th.Client, channel)
	reader := th.CreateClient()
	th.LoginBasic2WithClient(t, reader)

	// Compliance mode requires membership to read posts, while public channel
	// metadata remains discoverable to this same team member.
	response, err := reader.DoAPIGet(ctx, "/channels/"+channel.Id+"/posts", "")
	require.Error(t, err)
	require.NotNil(t, response)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	if response.Body != nil {
		_ = response.Body.Close()
	}
	response, err = reader.DoAPIGet(ctx, "/channels/"+channel.Id, "")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var result struct {
		ID       string      `json:"id"`
		LastPost *model.Post `json:"last_post"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
	require.Equal(t, channel.Id, result.ID)
	if result.LastPost != nil {
		t.Logf("native posts endpoint denied; channel metadata exposed post=%s text_matches=%t", result.LastPost.Id, result.LastPost.Message == post.Message)
	}
	require.Nil(t, result.LastPost, "channel metadata must not expose content denied by the posts endpoint")
}

func TestPerformanceReactionDoesNotReadNewerUnseenPosts(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	ctx := context.Background()
	oldPost := th.CreatePostWithClient(t, th.Client, th.BasicChannel)
	time.Sleep(5 * time.Millisecond)
	newPost := th.CreatePostWithClient(t, th.Client, th.BasicChannel)
	reader := th.CreateClient()
	th.LoginBasic2WithClient(t, reader)
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE channelmembers SET lastviewedat=$3
		WHERE channelid=$1 AND userid=$2`, th.BasicChannel.Id, th.BasicUser2.Id, oldPost.CreateAt)
	require.NoError(t, err)
	// A reaction can be sent from search results or a permalink, without opening
	// the channel or seeing any posts newer than the reacted-to message.
	_, _, err = reader.SaveReaction(ctx, &model.Reaction{
		UserId: th.BasicUser2.Id, PostId: oldPost.Id, EmojiName: "+1",
	})
	require.NoError(t, err)
	var viewedAt int64
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT lastviewedat FROM channelmembers
		WHERE channelid=$1 AND userid=$2`, th.BasicChannel.Id, th.BasicUser2.Id).Scan(&viewedAt))
	t.Logf("reaction to old post: read_cursor_advanced_past_newer_post=%t", viewedAt >= newPost.CreateAt)
	require.Less(t, viewedAt, newPost.CreateAt, "reacting to an old post must not mark newer unseen messages read")
}

func TestPerformanceBuzzCooldownSurvivesBusyChannel(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	ctx := context.Background()
	_, firstResponse, err := th.Client.CreatePost(ctx, &model.Post{
		ChannelId: th.BasicChannel.Id, Message: "BUZZMESSAGE",
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, firstResponse.StatusCode)
	// Sanity check: the immediate retry is correctly blocked.
	_, response, err := th.Client.CreatePost(ctx, &model.Post{
		ChannelId: th.BasicChannel.Id, Message: "BUZZMESSAGE",
	})
	require.Error(t, err)
	require.Equal(t, http.StatusTooManyRequests, response.StatusCode)
	time.Sleep(5 * time.Millisecond)
	// Fifty newer messages can arrive well within three minutes in an active
	// channel. Insert directly to avoid notification work unrelated to the check.
	for range 50 {
		_, saveErr := th.Store.Post().Save(th.Context, &model.Post{
			ChannelId: th.BasicChannel.Id, UserId: th.BasicUser2.Id, Message: "ordinary newer message",
		})
		require.NoError(t, saveErr)
	}
	_, response, err = th.Client.CreatePost(ctx, &model.Post{
		ChannelId: th.BasicChannel.Id, Message: "BUZZMESSAGE",
	})
	require.NotNil(t, response)
	t.Logf("buzz retry with 50 newer posts: http_status=%d", response.StatusCode)
	require.Equal(t, http.StatusTooManyRequests, response.StatusCode, "the cooldown is a time window, not a last-50-post window")
	require.Error(t, err)
}

// Native validation failure must release only its own reservation, and parallel
// requests (including requests on different servers) share the database guard.
func TestPerformanceBuzzConcurrentAndFailureRetry(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	ctx := context.Background()
	_, response, err := th.Client.CreatePost(ctx, &model.Post{
		ChannelId: th.BasicChannel.Id, RootId: model.NewId(), Message: "BUZZMESSAGE",
	})
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	_, response, err = th.Client.CreatePost(ctx, &model.Post{ChannelId: th.BasicChannel.Id, Message: "BUZZMESSAGE"})
	require.NoError(t, err, "failed native validation must not start the cooldown")
	require.Equal(t, http.StatusCreated, response.StatusCode)

	const attempts = 8
	statuses := make(chan int, attempts)
	start := make(chan struct{})
	for range attempts {
		go func() {
			<-start
			_, response, err := th.Client.CreatePost(ctx, &model.Post{ChannelId: th.BasicChannel2.Id, Message: "BUZZMESSAGE"})
			if response == nil || (err != nil && response.StatusCode != http.StatusTooManyRequests) {
				statuses <- 0
				return
			}
			statuses <- response.StatusCode
		}()
	}
	close(start)
	created, blocked := 0, 0
	for range attempts {
		switch status := <-statuses; status {
		case http.StatusCreated:
			created++
		case http.StatusTooManyRequests:
			blocked++
		default:
			t.Errorf("unexpected concurrent Buzz response: %d", status)
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, attempts-1, blocked)
	var stored int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT COUNT(*) FROM posts WHERE channelid=$1 AND userid=$2 AND message='BUZZMESSAGE'`, th.BasicChannel2.Id, th.BasicUser.Id).Scan(&stored))
	require.Equal(t, 1, stored)

	// The posts lookup protects upgrades and missing reservation rows too.
	_, err = th.Store.GetInternalMasterDB().Exec(`DELETE FROM workspace_buzz_cooldowns WHERE channelid=$1 AND userid=$2`, th.BasicChannel2.Id, th.BasicUser.Id)
	require.NoError(t, err)
	_, response, err = th.Client.CreatePost(ctx, &model.Post{ChannelId: th.BasicChannel2.Id, Message: "BUZZMESSAGE"})
	require.Error(t, err)
	require.Equal(t, http.StatusTooManyRequests, response.StatusCode)

	// Once the successful post is outside the time window, sending works again.
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET createat=createat-181000 WHERE channelid=$1 AND message='BUZZMESSAGE'`, th.BasicChannel2.Id)
	require.NoError(t, err)
	_, response, err = th.Client.CreatePost(ctx, &model.Post{ChannelId: th.BasicChannel2.Id, Message: "BUZZMESSAGE"})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
}
