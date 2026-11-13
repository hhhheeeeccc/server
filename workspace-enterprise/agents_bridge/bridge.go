package agents_bridge

import (
	"fmt"
	"net/http"

	"github.com/blang/semver/v4"
	agentclient "github.com/mattermost/mattermost-plugin-ai/public/bridgeclient"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"

	"github.com/mattermost/enterprise/plugin_ids"
)

const minimumBridgeVersion = "1.5.0"

// Register replaces the upstream bridge, which targets the upstream plugin ID,
// with a bridge targeting the Workspace AI plugin ID.
func Register(a *app.App) {
	a.Channels().SetAgentsBridge(&bridge{app: a})
}

type bridge struct {
	app *app.App
}

var _ app.AgentsBridge = (*bridge)(nil)

func (b *bridge) Status(_ request.CTX) (bool, string) {
	if b.app == nil || b.app.License() == nil {
		return false, "app.agents.bridge.not_available.plugin_not_active"
	}
	env := b.app.GetPluginsEnvironment()
	if env == nil {
		return false, "app.agents.bridge.not_available.plugin_env_not_initialized"
	}
	if !env.IsActive(plugin_ids.AI) {
		return false, "app.agents.bridge.not_available.plugin_not_active"
	}
	for _, active := range env.Active() {
		if active.Manifest == nil || active.Manifest.Id != plugin_ids.AI {
			continue
		}
		version, err := semver.Parse(active.Manifest.Version)
		if err != nil {
			return false, "app.agents.bridge.not_available.plugin_version_parse_failed"
		}
		minimum, _ := semver.Parse(minimumBridgeVersion)
		if version.LT(minimum) {
			return false, "app.agents.bridge.not_available.plugin_version_too_old"
		}
		return true, ""
	}
	return false, "app.agents.bridge.not_available.plugin_not_registered"
}

func (b *bridge) client(sessionUserID string) *agentclient.Client {
	return agentclient.NewClientFromApp(routedAppAPI{AppAPI: b.app}, sessionUserID)
}

func (b *bridge) available() error {
	if ok, reason := b.Status(nil); !ok {
		return fmt.Errorf("Workspace AI bridge unavailable: %s", reason)
	}
	return nil
}

func (b *bridge) GetAgents(sessionUserID, userID string) ([]model.BridgeAgentInfo, error) {
	if err := b.available(); err != nil {
		return []model.BridgeAgentInfo{}, nil
	}
	agents, err := b.client(sessionUserID).GetAgents(userID)
	if err != nil {
		return nil, err
	}
	result := make([]model.BridgeAgentInfo, 0, len(agents))
	for _, agent := range agents {
		result = append(result, model.BridgeAgentInfo{
			ID: agent.ID, DisplayName: agent.DisplayName, Username: agent.Username,
			ServiceID: agent.ServiceID, ServiceType: agent.ServiceType, IsDefault: agent.IsDefault,
		})
	}
	return result, nil
}

func (b *bridge) GetServices(sessionUserID, userID string) ([]model.BridgeServiceInfo, error) {
	if err := b.available(); err != nil {
		return []model.BridgeServiceInfo{}, nil
	}
	services, err := b.client(sessionUserID).GetServices(userID)
	if err != nil {
		return nil, err
	}
	result := make([]model.BridgeServiceInfo, 0, len(services))
	for _, service := range services {
		result = append(result, model.BridgeServiceInfo{ID: service.ID, Name: service.Name, Type: service.Type})
	}
	return result, nil
}

func (b *bridge) AgentCompletion(sessionUserID, agentID string, req app.BridgeCompletionRequest) (string, error) {
	if err := b.available(); err != nil {
		return "", err
	}
	return b.client(sessionUserID).AgentCompletion(agentID, toClientRequest(req))
}

func (b *bridge) ServiceCompletion(sessionUserID, serviceID string, req app.BridgeCompletionRequest) (string, error) {
	if err := b.available(); err != nil {
		return "", err
	}
	return b.client(sessionUserID).ServiceCompletion(serviceID, toClientRequest(req))
}

func toClientRequest(req app.BridgeCompletionRequest) agentclient.CompletionRequest {
	posts := make([]agentclient.Post, 0, len(req.Messages))
	for _, message := range req.Messages {
		posts = append(posts, agentclient.Post{Role: message.Role, Message: message.Message, FileIDs: append([]string(nil), message.FileIDs...)})
	}
	return agentclient.CompletionRequest{
		Posts: posts, JSONOutputFormat: req.JSONOutputFormat,
		UserID: req.UserID, ChannelID: req.ChannelID,
		Operation: req.ClientOperation, OperationSubType: req.OperationSubType,
	}
}

// The upstream bridge client hardcodes mattermost-ai for internal requests.
// Its transport removes the first URL path segment before calling this method.
type routedAppAPI struct {
	agentclient.AppAPI
}

func (a routedAppAPI) ServeInternalPluginRequest(userID string, w http.ResponseWriter, r *http.Request, sourcePluginID, _ string) {
	a.AppAPI.ServeInternalPluginRequest(userID, w, r, sourcePluginID, plugin_ids.AI)
}
