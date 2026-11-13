package access_control_job

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

// Opt-in browser fixture: a temporary test database, test-only license, local
// file store and loopback listener. It never reads the running site's config.
// Generated test credentials live only in a mode-0600 temporary handoff file.
func TestChannelAttributeBrowserFixture(t *testing.T) {
	dir := os.Getenv("WORKSPACE_ABAC_BROWSER_DIR")
	clientDir := os.Getenv("WORKSPACE_ABAC_BROWSER_CLIENT")
	if dir == "" || clientDir == "" {
		t.Skip("set WORKSPACE_ABAC_BROWSER_DIR and WORKSPACE_ABAC_BROWSER_CLIENT for browser verification")
	}
	require.NoError(t, os.MkdirAll(dir, 0700))
	// MainHelper owns this temporary resource directory, including client.
	require.NoError(t, os.Rename("client", "client-before-browser"))
	// The web server may rewrite root.html for its SiteURL. Use a private
	// copy so a browser test cannot rewrite the project's built assets.
	require.NoError(t, os.CopyFS("client", os.DirFS(clientDir)))
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll("client"))
		require.NoError(t, os.Rename("client-before-browser", "client"))
	})
	th, worker := setupChannelAttributeHTTP(t)
	th.App.UpdateConfig(func(cfg *model.Config) {
		*cfg.ServiceSettings.SiteURL = th.Client.URL
		*cfg.LocalizationSettings.DefaultClientLocale = "ar"
		*cfg.ServiceSettings.EnableOnboardingFlow = false
		*cfg.PluginSettings.Enable = false
	})
	channel := th.CreateChannelWithClient(t, th.SystemAdminClient, model.ChannelTypePrivate)
	_, _, err := th.SystemAdminClient.AddChannelMember(th.Context.Context(), channel.Id, th.BasicUser2.Id)
	require.NoError(t, err)
	data, err := json.Marshal(map[string]any{
		"base":   th.Client.URL,
		"admin":  map[string]string{"username": th.SystemAdminUser.Username, "password": th.SystemAdminUser.Password, "id": th.SystemAdminUser.Id},
		"member": map[string]string{"username": th.BasicUser2.Username, "password": th.BasicUser2.Password, "id": th.BasicUser2.Id},
		"team":   th.BasicTeam, "channel": channel,
	})
	require.NoError(t, err)
	handoff := filepath.Join(dir, "fixture.json")
	require.NoError(t, os.WriteFile(handoff, data, 0600))
	t.Cleanup(func() { _ = os.Remove(handoff) })
	t.Logf("BROWSER_FIXTURE_READY %s", th.Client.URL)
	deadline := time.After(45 * time.Minute)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("browser verification did not finish within 45 minutes")
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(dir, "finished")); err == nil {
				return
			}
			if _, err := os.Stat(filepath.Join(dir, "sync")); err == nil {
				require.NoError(t, os.Remove(filepath.Join(dir, "sync")))
				job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeAccessControlSync, map[string]string{"channel_ids": channel.Id})
				require.Nil(t, appErr)
				require.False(t, worker.doJob(job))
				stored, err := th.Store.Job().Get(th.Context, job.Id)
				require.NoError(t, err)
				result, err := json.Marshal(stored)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "sync-result.json"), result, 0600))
			}
		}
	}
}
