//go:build integration

package disappearing_messages

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

// Opt-in browser fixture: all accounts, credentials, files and messages belong
// to the disposable database from TestMain. Never reuse a running site's DB.
// A frontend dev server can proxy to api_url; the browser must use its normal
// login and conversation menu, not an injected application state.
func TestBrowserAcceptance(t *testing.T) {
	path := os.Getenv("WORKSPACE_EXPIRY_BROWSER_FIXTURE")
	if path == "" {
		t.Skip("set WORKSPACE_EXPIRY_BROWSER_FIXTURE to a private temporary JSON path")
	}
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	th.AddPermissionToRole(t, permissionManageDisappearingMessages.Id, model.SystemUserRoleId)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	channel, _, err := th.Client.CreateDirectChannel(context.Background(), th.BasicUser.Id, th.BasicUser2.Id)
	require.NoError(t, err)
	_, _, err = th.Client.CreatePost(context.Background(), &model.Post{ChannelId: channel.Id, Message: "هذه محادثة اختبار معزولة لإعداد الرسائل المختفية."})
	require.NoError(t, err)
	fixture, err := json.Marshal(map[string]string{
		"api_url": th.Client.URL, "email": th.BasicUser.Email, "password": th.BasicUser.Password,
		"recipient_email": th.BasicUser2.Email, "recipient_password": th.BasicUser2.Password,
		"channel_id": channel.Id, "team_name": th.BasicTeam.Name,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, fixture, 0600))
	t.Cleanup(func() { os.Remove(path); os.Remove(path + ".done") })
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(15 * time.Minute)
	for {
		select {
		case <-ticker.C:
			if _, err := os.Stat(path + ".done"); err == nil {
				return
			}
		case <-deadline:
			t.Fatal("browser fixture timed out")
		}
	}
}
