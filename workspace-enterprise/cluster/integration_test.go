package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/app/platform"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/channels/wsapi"
	"github.com/mattermost/mattermost/server/v8/config"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	platform.RegisterClusterInterface(New)
	if path := os.Getenv(clusterProcessConfigEnv); path != "" {
		if err := runClusterProcess(path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	h := testlib.NewMainHelper()
	defer h.Close()
	api4.SetMainHelper(h)
	h.Main(m)
}
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

func TestTwoNodesTransportRPCAndLeaderFailover(t *testing.T) {
	th := api4.SetupWithServerOptionsAndConfig(t, []app.Option{app.JoinCluster}, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	}).InitBasic(t)
	c1 := th.Server.Platform().Cluster().(*Service)
	require.Nil(t, c1.GetMyClusterInfo(), "disabled nodes must not start a listener")
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Id = model.NewId()
	fixture, err := json.Marshal(license)
	require.NoError(t, err)
	validatorBefore := utils.LicenseValidator
	t.Cleanup(func() { utils.LicenseValidator = validatorBefore })
	t.Setenv("MM_LICENSE", string(fixture))
	utils.LicenseValidator = clusterProcessLicenseValidator{LicenseValidatorIface: validatorBefore, fixture: fixture}
	require.True(t, th.Server.SetLicense(license))
	cfg := th.App.Config().Clone()
	cfg.ClusterSettings.Enable = new(true)
	cfg.ClusterSettings.ClusterName = new(model.NewId())
	cfg.ClusterSettings.BindAddress = new("127.0.0.1")
	cfg.ClusterSettings.AdvertiseAddress = new("127.0.0.1")
	cfg.ClusterSettings.GossipPort = new(freePort(t))
	cfg.ClusterSettings.ReadOnlyConfig = new(false)
	_, _, configSaveErr := th.Server.Platform().SaveConfig(cfg, true)
	require.Nil(t, configSaveErr)
	c1.StartInterNodeCommunication()
	t.Cleanup(c1.StopInterNodeCommunication)
	require.NotNil(t, c1.GetMyClusterInfo())
	require.True(t, c1.IsLeader())
	cfg2 := cfg.Clone()
	cfg2.ClusterSettings.GossipPort = new(freePort(t))
	cfg2.ServiceSettings.ListenAddress = new("localhost:0")
	mem, err := config.NewMemoryStoreWithOptions(&config.MemoryStoreOptions{IgnoreEnvironmentOverrides: true})
	require.NoError(t, err)
	require.NoError(t, mem.Set(cfg2))
	cs, err := config.NewStoreFromBacking(mem, nil, false)
	require.NoError(t, err)
	s2, err := app.NewServer(app.ConfigStore(cs), app.JoinCluster)
	require.NoError(t, err)
	t.Cleanup(func() { s2.Shutdown(); _ = cs.Close() })
	_, err = api4.Init(s2)
	require.NoError(t, err)
	wsapi.Init(s2)
	require.True(t, s2.SetLicense(license))
	require.NoError(t, s2.Start())
	c2 := s2.Platform().Cluster().(*Service)
	require.True(t, c2.enabled(), "second cluster settings: %+v", s2.Config().ClusterSettings)
	require.NotNil(t, c2.GetMyClusterInfo())
	require.Eventually(t, func() bool {
		a, _ := c1.GetClusterInfos()
		b, _ := c2.GetClusterInfos()
		return len(a) == 2 && len(b) == 2
	}, 10*time.Second, 50*time.Millisecond)
	require.NotEqual(t, c1.GetClusterId(), c2.GetClusterId())
	require.True(t, c1.IsLeader())
	require.False(t, c2.IsLeader())
	// Both app instances must read the same database record and file bytes.
	// This remains a same-host check; the physical two-machine test is separate.
	peerApp := app.New(app.ServerConnector(s2.Channels()))
	peerPost, appErr := peerApp.GetSinglePost(th.Context, th.BasicPost.Id, false)
	require.Nil(t, appErr)
	require.Equal(t, th.BasicPost.Message, peerPost.Message)
	fileBytes := []byte("shared cluster file")
	file, appErr := th.App.DoUploadFile(th.Context, time.Now(), th.BasicTeam.Id, th.BasicChannel.Id, th.BasicUser.Id, "shared.txt", fileBytes, false)
	require.Nil(t, appErr)
	peerFile, appErr := peerApp.GetFileInfo(th.Context, file.Id)
	require.Nil(t, appErr)
	require.Equal(t, file.Path, peerFile.Path)
	readBytes, appErr := peerApp.ReadFile(peerFile.Path)
	require.Nil(t, appErr)
	require.Equal(t, fileBytes, readBytes)
	// Possessing an address alone does not permit joining an encrypted cluster.
	var encodedKey string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT Value FROM Systems WHERE Name='ClusterEncryptionKey'`).Scan(&encodedKey))
	sharedKey, err := base64.StdEncoding.DecodeString(encodedKey)
	require.NoError(t, err)
	labelHash := sha256.Sum256([]byte(*cfg.ClusterSettings.ClusterName))
	label := "workspace-" + hex.EncodeToString(labelHash[:8])
	for _, mismatch := range []struct {
		name, label string
		key         []byte
	}{
		{"wrong encryption key", label, bytes.Repeat([]byte{42}, 32)},
		{"other cluster", "workspace-other", sharedKey},
	} {
		t.Run(mismatch.name, func(t *testing.T) {
			peerConfig := memberlist.DefaultLANConfig()
			peerConfig.Name = model.NewId()
			peerConfig.BindAddr, peerConfig.AdvertiseAddr = "127.0.0.1", "127.0.0.1"
			peerConfig.BindPort = freePort(t)
			peerConfig.AdvertisePort = peerConfig.BindPort
			peerConfig.Label, peerConfig.SecretKey = mismatch.label, mismatch.key
			peerConfig.TCPTimeout = time.Second
			peerConfig.LogOutput = io.Discard
			peer, createErr := memberlist.Create(peerConfig)
			require.NoError(t, createErr)
			defer peer.Shutdown()
			_, joinErr := peer.Join([]string{fmt.Sprintf("127.0.0.1:%d", *cfg.ClusterSettings.GossipPort)})
			require.Error(t, joinErr)
		})
	}
	// Config RPC updates the peer while preserving its own transport endpoint.
	updatedConfig := th.App.Config().Clone()
	updatedConfig.TeamSettings.SiteName = new("HA " + model.NewId())
	_, _, configErr := th.Server.Platform().SaveConfig(updatedConfig, true)
	require.Nil(t, configErr, "%v", configErr)
	require.Equal(t, *updatedConfig.TeamSettings.SiteName, *s2.Config().TeamSettings.SiteName)
	require.Equal(t, *cfg2.ClusterSettings.GossipPort, *s2.Config().ClusterSettings.GossipPort)
	require.Equal(t, s2.Platform().ClientConfigHash(), c2.GetMyClusterInfo().ConfigHash)
	// Exercise the public interface on actual encrypted loopback TCP connections.
	received := make(chan *model.ClusterMessage, 2)
	eventType := model.ClusterEvent("workspace_test_delivery")
	c2.RegisterClusterMessageHandler(eventType, func(m *model.ClusterMessage) { received <- m })
	c1.SendClusterMessage(&model.ClusterMessage{Event: eventType, Data: []byte(th.BasicUser.Id), SendType: model.ClusterSendReliable, WaitForAllToSend: true})
	select {
	case m := <-received:
		require.Equal(t, th.BasicUser.Id, string(m.Data))
	case <-time.After(4 * time.Second):
		t.Fatal("peer did not receive cache invalidation")
	}
	stats, appErr := c1.GetClusterStats(th.Context)
	require.Nil(t, appErr)
	require.Len(t, stats, 1)
	require.Equal(t, c2.id, stats[0].Id)
	count, appErr := c1.WebConnCountForUser(th.BasicUser.Id)
	require.Nil(t, appErr)
	require.Zero(t, count)
	queues, err := c1.GetWSQueues(th.BasicUser.Id, model.NewId(), 0)
	require.NoError(t, err)
	require.Len(t, queues, 1)
	// A websocket attached to the second server receives a user-targeted event
	// published on the first server through the real core cluster handler.
	ws, err := model.NewWebSocketClient4(fmt.Sprintf("ws://localhost:%d", s2.ListenAddr.Port), th.Client.AuthToken)
	require.NoError(t, err)
	ws.Listen()
	t.Cleanup(ws.Close)
	select {
	case response := <-ws.ResponseChannel:
		require.Equal(t, model.StatusOk, response.Status)
	case <-time.After(5 * time.Second):
		t.Fatal("websocket authentication timed out")
	}
	require.Eventually(t, func() bool {
		n, e := c1.WebConnCountForUser(th.BasicUser.Id)
		return e == nil && n == 1
	}, 5*time.Second, 50*time.Millisecond)
	event := model.NewWebSocketEvent(model.WebsocketEventType("workspace_ha_test"), "", "", th.BasicUser.Id, nil, "")
	event.Add("value", "across-nodes")
	event.GetBroadcast().ReliableClusterSend = true
	th.App.Publish(event)
	deadline := time.After(5 * time.Second)
receivedEvent:
	for {
		select {
		case e := <-ws.EventChannel:
			if e.EventType() == event.EventType() {
				require.Equal(t, "across-nodes", e.GetData()["value"])
				break receivedEvent
			}
		case <-deadline:
			t.Fatal("event did not reach the other node's websocket")
		}
	}
	ws.Close()
	// Reliable delivery fails visibly if the peer cannot handle the event.
	require.ErrorContains(t, c1.SendClusterMessageToNode(c2.id, &model.ClusterMessage{Event: "unregistered_test_event", SendType: model.ClusterSendReliable}), "unregistered")
	// Graceful stop drains the workers and supports a subsequent start.
	c2.StopInterNodeCommunication()
	require.Nil(t, c2.GetMyClusterInfo())
	c2.StartInterNodeCommunication()
	require.NotNil(t, c2.GetMyClusterInfo())
	require.Eventually(t, func() bool {
		a, _ := c1.GetClusterInfos()
		b, _ := c2.GetClusterInfos()
		return len(a) == 2 && len(b) == 2
	}, 10*time.Second, 50*time.Millisecond)
	// Registered core status handler must update the peer's cache.
	status := &model.Status{UserId: th.BasicUser.Id, Status: model.StatusAway, Manual: true, LastActivityAt: model.GetMillis()}
	data, err := json.Marshal(status)
	require.NoError(t, err)
	require.NoError(t, c1.SendClusterMessageToNode(c2.id, &model.ClusterMessage{Event: model.ClusterEventUpdateStatus, Data: data}))
	require.Eventually(t, func() bool {
		v, e := peerApp.GetStatus(th.BasicUser.Id)
		return e == nil && v.Status == model.StatusAway
	}, 3*time.Second, 20*time.Millisecond)
	// Reject arbitrary and stopped destinations, never return fake success.
	require.Error(t, c1.SendClusterMessageToNode(model.NewId(), &model.ClusterMessage{}))
	// A database timeout must revoke local leadership even if the old lease has
	// not expired. A dedicated connection blocks renewal on both live nodes.
	lock, err := th.Store.GetInternalMasterDB().BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer lock.Rollback()
	_, err = lock.Exec(`SELECT OwnerId FROM WorkspaceClusterLeases WHERE ClusterName=$1 FOR UPDATE`, *cfg.ClusterSettings.ClusterName)
	require.NoError(t, err)
	c1.refresh()
	require.False(t, c1.IsLeader(), "a failed renewal must fail closed")
	require.NoError(t, lock.Rollback())
	c1.refresh()
	require.True(t, c1.IsLeader())
	// A lost process does not release its SQL lease. Simulate that failure rather
	// than just exercising the graceful-leave fast path.
	c1.mu.RLock()
	ml := c1.list
	stop, done := c1.stop, c1.done
	cancel := c1.cancelRun
	c1.mu.RUnlock()
	close(stop)
	cancel()
	<-done
	require.NoError(t, ml.Shutdown())
	c1.workers.Wait()
	c1.rpcWorkers.Wait()
	c1.mu.Lock()
	c1.list = nil
	c1.mu.Unlock()
	require.Eventually(t, func() bool { return !c1.IsLeader() && c2.IsLeader() }, 15*time.Second, 100*time.Millisecond)
	var owner string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRowContext(context.Background(), `SELECT OwnerId FROM WorkspaceClusterLeases WHERE ClusterName=$1`, *cfg.ClusterSettings.ClusterName).Scan(&owner))
	require.Equal(t, c2.id, owner)
	s2.SetLicense(nil)
	require.False(t, c2.IsLeader())
	require.Error(t, c2.SendClusterMessageToNode(c1.id, &model.ClusterMessage{}))
}
