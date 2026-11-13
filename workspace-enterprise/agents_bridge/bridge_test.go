package agents_bridge

import (
	"net/http"
	"testing"

	agentclient "github.com/mattermost/mattermost-plugin-ai/public/bridgeclient"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/stretchr/testify/require"
)

type recordingAppAPI struct {
	userID, sourceID, destinationID, path string
}

func (a *recordingAppAPI) ServeInternalPluginRequest(userID string, w http.ResponseWriter, r *http.Request, sourceID, destinationID string) {
	a.userID, a.sourceID, a.destinationID, a.path = userID, sourceID, destinationID, r.URL.Path
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"agents":[],"completion":"ok"}`))
}

func TestOldBridgeClientRoutesToWorkspaceAI(t *testing.T) {
	api := &recordingAppAPI{}
	client := agentclient.NewClientFromApp(routedAppAPI{api}, "session-user")

	_, err := client.GetAgents("")
	require.NoError(t, err)
	require.Equal(t, "session-user", api.userID)
	require.Equal(t, "mattermost-server", api.sourceID)
	require.Equal(t, "workspace-ai", api.destinationID)
	require.Equal(t, "/bridge/v1/agents", api.path)

	completion, err := client.ServiceCompletion("service", agentclient.CompletionRequest{})
	require.NoError(t, err)
	require.Equal(t, "ok", completion)
	require.Equal(t, "workspace-ai", api.destinationID)
	require.Equal(t, "/bridge/v1/completion/service/service/nostream", api.path)
}

func TestBridgeRequiresLicense(t *testing.T) {
	b := &bridge{}
	available, _ := b.Status(nil)
	require.False(t, available)
	_, err := b.ServiceCompletion("", "service", app.BridgeCompletionRequest{})
	require.Error(t, err)
}
