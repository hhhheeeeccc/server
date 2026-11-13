//go:build integration

package audit_file

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/mattermost/enterprise/compliance_service"
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
		return compliance_service.New(a)
	})
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestAuditFilePageRequiresLicenseAndPermissions(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MM_LOG_PATH", root)
	path := filepath.Join(root, "audit.log")
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		*cfg.ExperimentalAuditSettings.FileEnabled = true
		*cfg.ExperimentalAuditSettings.FileName = path
	}).InitBasic(t)
	record, err := json.Marshal(map[string]any{
		"event_name": "testEvent",
		"actor":      map[string]any{"user_id": th.BasicUser.Id},
		"event":      map[string]any{"object_type": "post", "parameters": map[string]any{"post": map[string]any{"channel_id": th.BasicChannel.Id}}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(record, '\n'), 0o600))
	ctx := th.Context.Context()
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))

	response, err := th.Client.DoAPIGet(ctx, "/audits/file/page", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/audits/file/page", "")
	require.NoError(t, err)
	defer response.Body.Close()
	var page Page
	require.NoError(t, json.NewDecoder(response.Body).Decode(&page))
	require.NotEmpty(t, page.Records)
	require.Len(t, page.Contexts, len(page.Records))
	found := false
	for i, raw := range page.Records {
		if bytes.Contains(raw, []byte(`"event_name":"testEvent"`)) {
			require.Equal(t, th.BasicUser.Username, page.Contexts[i].Actor.Username)
			require.Equal(t, th.BasicChannel.DisplayName, page.Contexts[i].Channel.Name)
			found = true
		}
	}
	require.True(t, found)

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/audits/file/page?q=testEvent", "")
	require.NoError(t, err)
	var searched Page
	require.NoError(t, json.NewDecoder(response.Body).Decode(&searched))
	require.NoError(t, response.Body.Close())
	require.NotEmpty(t, searched.Records)
	for _, raw := range searched.Records {
		require.Contains(t, string(raw), `"event_name":"testEvent"`)
	}

	th.Server.SetLicense(nil)
	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/audits/file/page", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())
}
