package plugin_assets

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

// Register runs before core API registration. Without a license, the matcher
// leaves the upstream endpoint in charge.
func Register(a *app.App) {
	a.Srv().Router.Handle(model.APIURLSuffix+"/plugins/webapp", &web.Handler{
		Srv: a.Srv(), HandleFunc: getWebappPlugins,
		HandlerName: web.GetHandlerName(getWebappPlugins),
	}).MatcherFunc(func(_ *http.Request, _ *mux.RouteMatch) bool {
		return a.License() != nil
	}).Methods(http.MethodGet)
}

func getWebappPlugins(c *web.Context, w http.ResponseWriter, _ *http.Request) {
	if !*c.App.Config().PluginSettings.Enable {
		c.Err = model.NewAppError("getWebappPlugins", "app.plugin.disabled.app_error", nil, "", http.StatusNotImplemented)
		return
	}
	manifests, appErr := c.App.GetActivePluginManifests()
	if appErr != nil {
		c.Err = appErr
		return
	}
	if err := json.NewEncoder(w).Encode(clientManifests(manifests)); err != nil {
		c.Logger.Warn("Unable to write webapp plugin manifests", mlog.Err(err))
	}
}

func clientManifests(manifests []*model.Manifest) []*model.Manifest {
	result := make([]*model.Manifest, 0, len(manifests))
	for _, manifest := range manifests {
		if manifest == nil || !manifest.HasClient() {
			continue
		}
		client := manifest.ClientManifest()
		client.SettingsSchema = nil
		// Match the host's /static/plugins/ file handler without changing the
		// upstream model or relying on a client's compatibility workaround.
		client.Webapp.BundlePath = fmt.Sprintf("/static/plugins/%s/%s_%x_bundle.js", manifest.Id, manifest.Id, manifest.Webapp.BundleHash)
		result = append(result, client)
	}
	return result
}
