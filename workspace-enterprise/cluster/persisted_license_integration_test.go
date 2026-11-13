package cluster

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func persistedLicenseProcessConfig(t *testing.T, original *model.Config) *model.Config {
	t.Helper()
	cfg := original.Clone()
	cfg.ClusterSettings.Enable = new(true)
	cfg.ClusterSettings.ClusterName = new(model.NewId())
	cfg.ClusterSettings.BindAddress = new("127.0.0.1")
	cfg.ClusterSettings.AdvertiseAddress = new("127.0.0.1")
	cfg.ClusterSettings.GossipPort = new(freePort(t))
	cfg.ClusterSettings.ReadOnlyConfig = new(false)
	cfg.ServiceSettings.ListenAddress = new("127.0.0.1:0")
	cfg.ServiceSettings.EnableLocalMode = new(false)
	cfg.PluginSettings.Enable = new(false)
	cfg.LogSettings.EnableFile = new(false)
	cfg.SqlSettings.DataSourceReplicas = nil
	return cfg
}

func requestPersistedLicenseCacheRecovery(t *testing.T, node *clusterTestProcess) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, node.url+"/_workspace_cluster_test", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+node.token)
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusAccepted, response.StatusCode)
}

func TestSeparateProcessPersistedLicenseCacheRecovery(t *testing.T) {
	for _, failedStartup := range []bool{false, true} {
		name := "transport receiver"
		if failedStartup {
			name = "failed startup receiver"
		}
		t.Run(name, func(t *testing.T) {
			th := api4.SetupConfig(t, func(cfg *model.Config) {
				cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
				cfg.EmailSettings.SendEmailNotifications = new(false)
			}).InitBasic(t)
			cfg := persistedLicenseProcessConfig(t, th.Server.Config())
			coordinationDSN := *cfg.SqlSettings.DataSource
			if failedStartup {
				coordinationDSN = clusterStartupDeniedDataSource(t, th.Store.GetInternalMasterDB(), coordinationDSN, "startup schema read denied")
			}
			node := startClusterTestProcessWithConfig(t, clusterProcessConfig{
				Config: cfg, CoordinationDataSource: coordinationDSN,
				ExpectTransportFailure: failedStartup, PersistedLicense: true,
			})
			before, err := node.status(http.MethodGet)
			require.NoError(t, err)
			require.True(t, model.IsValidId(before.LicenseID))
			require.Equal(t, !failedStartup, before.TransportRunning)
			var activeID, record string
			require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT Value FROM Systems WHERE Name=$1`, model.SystemActiveLicenseId).Scan(&activeID))
			require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT Bytes FROM Licenses WHERE Id=$1`, activeID).Scan(&record))
			require.Equal(t, before.LicenseID, activeID)
			require.Contains(t, record, activeID)
			requestPersistedLicenseCacheRecovery(t, node)
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(node.logPath)
				return err == nil && strings.Contains(string(data), "Local cluster caches refreshed")
			}, 5*time.Second, 25*time.Millisecond)
			after, err := node.status(http.MethodGet)
			require.NoError(t, err)
			require.Equal(t, activeID, after.LicenseID, "the native full purge must reload the SQL fixture without MM_LICENSE")
			start := time.Now()
			require.NoError(t, node.stdin.Close())
			select {
			case <-node.done:
				require.NoError(t, node.err)
				t.Logf("persisted license purge and healthy shutdown: receiver=%s elapsed_ms=%d", name, time.Since(start).Milliseconds())
			case <-time.After(10 * time.Second):
				t.Fatal("healthy SQL-persisted license process did not stop within 10 seconds")
			}
		})
	}
}
