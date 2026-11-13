package audit_file

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestExtractRefs(t *testing.T) {
	post := json.RawMessage(`{"actor":{"user_id":"author"},"event_name":"createPost","event":{"object_type":"post","parameters":{"post":{"channel_id":"channel","user_id":"author"}},"resulting_state":{"id":"post"}}}`)
	refs := extractRefs(post)
	require.Equal(t, "author", refs.actorID)
	require.Equal(t, "channel", refs.channelID)
	require.Equal(t, "post", refs.postID)
	require.Empty(t, refs.targetID, "a post author is not a recipient")

	user := json.RawMessage(`{"actor":{"user_id":"admin"},"event":{"object_type":"user","parameters":{"user_id":"member"}}}`)
	require.Equal(t, "member", extractRefs(user).targetID)
}

func TestIdentityFallback(t *testing.T) {
	users := map[string]*model.User{
		"full":     {FirstName: "مراد", LastName: "السعيد", Username: "murad"},
		"partial":  {FirstName: "مراد", Username: "murad"},
		"username": {Username: "writer"},
	}
	require.Equal(t, "مراد السعيد", identity("full", users).Name)
	require.Equal(t, "murad", identity("partial", users).Name)
	require.Equal(t, "writer", identity("username", users).Name)
	require.Equal(t, "missing", identity("missing", users).Name)
}

func TestRecipientIDs(t *testing.T) {
	actor := "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbb"
	channel := &model.Channel{Type: model.ChannelTypeDirect, Name: actor + "__" + other}
	require.Equal(t, []string{other}, recipientIDs(channel, actor))
	channel.Type = model.ChannelTypeOpen
	require.Empty(t, recipientIDs(channel, actor))
}
