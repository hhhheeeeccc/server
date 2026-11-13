package bulk_reactions_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	_ "github.com/mattermost/enterprise"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestBulkReactionsReturnsEveryRequestedPost(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())

	otherPost, _, appErr := th.App.CreatePost(th.Context, &model.Post{
		UserId: th.BasicUser.Id, ChannelId: th.BasicPost.ChannelId, Message: "No reactions",
	}, th.BasicChannel, model.CreatePostFlags{})
	require.Nil(t, appErr)
	_, appErr = th.App.SaveReactionForPost(th.Context, &model.Reaction{
		UserId: th.BasicUser.Id, PostId: th.BasicPost.Id, EmojiName: "smile",
	})
	require.Nil(t, appErr)

	response, err := th.Client.DoAPIPost(th.Context.Context(), "/posts/ids/reactions", `["`+th.BasicPost.Id+`","`+otherPost.Id+`"]`)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var byPost map[string][]*model.Reaction
	require.NoError(t, json.NewDecoder(response.Body).Decode(&byPost))
	require.Len(t, byPost[th.BasicPost.Id], 1)
	require.Equal(t, "smile", byPost[th.BasicPost.Id][0].EmojiName)
	require.NotNil(t, byPost[otherPost.Id])
	require.Empty(t, byPost[otherPost.Id])
}

func TestBulkReactionsRejectsOversizedAndMalformedLists(t *testing.T) {
	const testMaxPostIDs = 500
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	postID := `"` + th.BasicPost.Id + `"`

	for _, body := range []string{"null", `[] true`, "[" + strings.Repeat(postID+",", testMaxPostIDs) + postID + "]"} {
		response, err := th.Client.DoAPIPost(th.Context.Context(), "/posts/ids/reactions", body)
		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}

	largeBody := "[" + strings.Repeat(postID+",", 2000) + postID + "]"
	response, err := th.Client.DoAPIPost(th.Context.Context(), "/posts/ids/reactions", largeBody)
	require.Error(t, err)
	require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)
	require.NoError(t, response.Body.Close())

	validBody := "[" + strings.TrimSuffix(strings.Repeat(postID+",", testMaxPostIDs), ",") + "]"
	response, err = th.Client.DoAPIPost(th.Context.Context(), "/posts/ids/reactions", validBody)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var byPost map[string][]*model.Reaction
	require.NoError(t, json.NewDecoder(response.Body).Decode(&byPost))
	require.Contains(t, byPost, th.BasicPost.Id)
}

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}
