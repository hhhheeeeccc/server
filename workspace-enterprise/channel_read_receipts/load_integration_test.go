package channel_read_receipts

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

// Explicit opt-in: this starts an ephemeral HTTP server and creates 150 users
// in the randomly named database owned by the integration test helper.
func TestReadReceiptsMessagingLoad(t *testing.T) {
	if os.Getenv("WORKSPACE_RECEIPTS_LOAD_TEST") != "1" {
		t.Skip("opt-in messaging load test")
	}
	script := os.Getenv("WORKSPACE_RECEIPTS_LOAD_SCRIPT")
	output := os.Getenv("WORKSPACE_RECEIPTS_LOAD_OUTPUT")
	require.NotEmpty(t, script)
	require.NotEmpty(t, output)
	require.True(t, filepath.IsAbs(script))
	require.True(t, filepath.IsAbs(output))
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableChannelViewedMessages = new(true)
		cfg.ServiceSettings.EnableUserAccessTokens = new(false)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.RateLimitSettings.Enable = new(false)
		cfg.TeamSettings.MaxUsersPerTeam = new(1000)
		cfg.LogSettings.EnableConsole = new(false)
	}).InitBasic(t)
	// Bound test traffic below the shared local PostgreSQL connection budget.
	// Concurrent HTTP requests queue in database/sql instead of opening a
	// connection per in-flight request and exhausting max_connections.
	th.Store.GetInternalMasterDB().SetMaxOpenConns(20)
	th.Store.GetInternalMasterDB().SetMaxIdleConns(10)
	var database string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow("SELECT current_database()").Scan(&database))
	require.Regexp(t, regexp.MustCompile(`^db[a-z0-9]{26}$`), database)
	require.NotEqual(t, 8065, th.Server.ListenAddr.Port)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	type identity struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	users := make([]identity, 0, 150)
	for i := range 150 {
		user := th.CreateUser(t)
		require.NotNil(t, user)
		th.LinkUserToTeam(t, user, th.BasicTeam)
		th.AddUserToChannel(t, user, th.BasicChannel)
		users = append(users, identity{user.Id, user.Username, user.Password})
		if (i+1)%25 == 0 {
			t.Logf("Prepared %d isolated test users", i+1)
		}
	}
	fixture := map[string]any{
		"base_url":   fmt.Sprintf("http://127.0.0.1:%d", th.Server.ListenAddr.Port),
		"server_pid": os.Getpid(), "database": database,
		"database_pool_max_open": th.Store.GetInternalMasterDB().Stats().MaxOpenConnections,
		"data_source":            *th.App.Config().SqlSettings.DataSource,
		"team":                   map[string]string{"id": th.BasicTeam.Id},
		"channel":                map[string]string{"id": th.BasicChannel.Id}, "users": users,
	}
	credentials := filepath.Join(t.TempDir(), "fixture.json")
	data, err := json.Marshal(fixture)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(credentials, data, 0600))
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", script)
	cmd.Env = append(os.Environ(), "WORKSPACE_RECEIPTS_LOAD_FIXTURE="+credentials)
	logPath := filepath.Join(output, "generator.log")
	log, err := os.Create(logPath)
	require.NoError(t, err)
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	require.NoError(t, cmd.Run(), "load failed; inspect %s", logPath)
	require.NoFileExists(t, credentials, "generator should remove temporary credentials after reading")
	t.Logf("Messaging load completed against isolated database %s", database)
}
