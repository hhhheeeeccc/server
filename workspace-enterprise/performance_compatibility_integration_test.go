package enterprise_test

import (
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestPerformanceWebsocketNativeActions(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	ws := th.CreateConnectedWebSocketClientWithClient(t, th.SystemAdminClient)
	request := func(action string, data map[string]any) *model.WebSocketResponse {
		t.Helper()
		seq := ws.Sequence
		ws.SendMessage(action, data)
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case response, open := <-ws.ResponseChannel:
				require.True(t, open, "websocket closed before action %s completed", action)
				if response.SeqReply == seq {
					return response
				}
			case <-deadline.C:
				t.Fatalf("no websocket response to %s", action)
				return nil
			}
		}
	}
	for _, action := range []struct {
		name string
		data map[string]any
	}{
		{"get_statuses_by_ids", map[string]any{"user_ids": []string{th.BasicUser.Id}}},
		{"presence", map[string]any{"channel_id": th.BasicChannel.Id}},
		{"posted_notify_ack", map[string]any{"post_id": th.BasicPost.Id, "status": "success"}},
	} {
		response := request(action.name, action.data)
		require.Equal(t, model.StatusOk, response.Status, "native %s must be registered", action.name)
		require.Nil(t, response.Error)
		if action.name == "get_statuses_by_ids" {
			require.Contains(t, response.Data, th.BasicUser.Id, "a successful status lookup must carry actual data")
		}
	}

	response := request("subscribe_statuses", map[string]any{"user_ids": []string{th.BasicUser.Id}})
	require.Equal(t, model.StatusFail, response.Status)
	require.NotNil(t, response.Error, "an unsupported subscription must not claim to be active")
}
