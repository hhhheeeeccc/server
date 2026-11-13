package plugin_assets

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestClientManifests(t *testing.T) {
	manifest := &model.Manifest{
		Id: "workspace-tickets", Name: "Service Desk", Version: "0.39.2",
		Description:    "Server description",
		Server:         &model.ManifestServer{Executable: "server/plugin"},
		Webapp:         &model.ManifestWebapp{BundlePath: "webapp/dist/main.js", BundleHash: []byte{0xd0, 0xbc}},
		SettingsSchema: &model.PluginSettingsSchema{Header: "Private configuration"},
	}
	clients := clientManifests([]*model.Manifest{nil, {Id: "server-only"}, manifest})
	require.Len(t, clients, 1)
	require.Equal(t, "/static/plugins/workspace-tickets/workspace-tickets_d0bc_bundle.js", clients[0].Webapp.BundlePath)
	require.Equal(t, manifest.Name, clients[0].Name)
	require.Equal(t, manifest.Version, clients[0].Version)
	require.Nil(t, clients[0].Server)
	require.Nil(t, clients[0].SettingsSchema)
	require.Empty(t, clients[0].Description)
	require.Equal(t, "webapp/dist/main.js", manifest.Webapp.BundlePath)
	require.NotNil(t, manifest.Server)
	require.NotNil(t, manifest.SettingsSchema)
}

func TestEmptyClientManifestsIsJSONArray(t *testing.T) {
	data, err := json.Marshal(clientManifests(nil))
	require.NoError(t, err)
	require.JSONEq(t, "[]", string(data))
}
