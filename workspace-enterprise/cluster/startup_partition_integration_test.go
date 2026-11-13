package cluster

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestSeparateProcessesStartupDuringPartitionRecoversCachesAfterSourceCrash(t *testing.T) {
	runStartupPartitionCacheRecovery(t, "registered")
}

func TestSeparateProcessesStartupWithoutDiscoveryRecoversCachesAfterSourceCrash(t *testing.T) {
	for _, discovery := range []string{"missing writer record", "discovery read denied"} {
		t.Run(discovery, func(t *testing.T) {
			runStartupPartitionCacheRecovery(t, discovery)
		})
	}
}

func TestSeparateProcessesFailedTransportRecoversCachesAfterSourceCrash(t *testing.T) {
	for _, failure := range []string{"startup schema read denied", "startup registration denied"} {
		t.Run(failure, func(t *testing.T) {
			runStartupPartitionCacheRecovery(t, failure)
		})
	}
}

func runStartupPartitionCacheRecovery(t *testing.T, discovery string) {
	t.Helper()
	failedTransport := strings.HasPrefix(discovery, "startup ")
	survivorPeers := 1
	if failedTransport {
		survivorPeers = 0
	}
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	}).InitBasic(t)
	cfg := th.Server.Config().Clone()
	cfg.ClusterSettings.Enable = new(true)
	cfg.ClusterSettings.EnableGossipEncryption = new(true)
	cfg.ClusterSettings.ClusterName = new(model.NewId())
	cfg.ClusterSettings.ReadOnlyConfig = new(false)
	cfg.ServiceSettings.ListenAddress = new("127.0.0.1:0")
	cfg.ServiceSettings.EnableLocalMode = new(false)
	cfg.PluginSettings.Enable = new(false)
	cfg.LogSettings.EnableFile = new(false)
	firstProxy := newGossipProxy(t, "127.0.0.18", "127.0.0.8")
	secondProxy := newGossipProxy(t, "127.0.0.19", "127.0.0.9")
	configure := func(bind, advertised string, proxy *gossipProxy) *model.Config {
		result := cfg.Clone()
		result.ClusterSettings.BindAddress = new(bind)
		result.ClusterSettings.AdvertiseAddress = new(advertised)
		result.ClusterSettings.OverrideHostname = new(advertised)
		result.ClusterSettings.GossipPort = new(proxy.port)
		return result
	}
	// The proxies block both transports before either node starts. No remote
	// member can ever join or leave the newcomer during this scenario.
	firstProxy.partition(true)
	secondProxy.partition(true)
	source := startClusterTestProcess(t, configure("127.0.0.8", "127.0.0.18", firstProxy))
	var coordinationDSN string
	switch discovery {
	case "missing writer record":
		result, err := th.Store.GetInternalMasterDB().Exec(`DELETE FROM ClusterDiscovery WHERE Id=$1`, source.id)
		require.NoError(t, err)
		affected, err := result.RowsAffected()
		require.NoError(t, err)
		require.EqualValues(t, 1, affected)
	case "discovery read denied":
		coordinationDSN = clusterDiscoveryDeniedDataSource(t, th.Store.GetInternalMasterDB(), *cfg.SqlSettings.DataSource)
	case "startup schema read denied", "startup registration denied":
		coordinationDSN = clusterStartupDeniedDataSource(t, th.Store.GetInternalMasterDB(), *cfg.SqlSettings.DataSource, discovery)
	}
	survivor := startClusterTestProcessWithStartupExpectation(t, configure("127.0.0.9", "127.0.0.19", secondProxy), coordinationDSN, failedTransport)
	t.Cleanup(func() { firstProxy.partition(false); secondProxy.partition(false) })
	require.Eventually(t, func() bool {
		a, ea := source.status(http.MethodGet)
		b, eb := survivor.status(http.MethodGet)
		return ea == nil && eb == nil && a.Peers == 1 && b.Peers == survivorPeers && a.Leader != b.Leader
	}, 20*time.Second, 100*time.Millisecond)
	if discovery == "registered" {
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(survivor.logPath)
			return err == nil && strings.Contains(string(data), "Cluster peer discovery retry")
		}, 10*time.Second, 50*time.Millisecond)
	} else if failedTransport {
		data, err := os.ReadFile(survivor.logPath)
		require.NoError(t, err)
		require.Contains(t, string(data), "Unable to start Enterprise cluster")
		_, table := clusterStartupDeniedStatement(discovery)
		require.Contains(t, string(data), "permission denied for table "+table)
		status, err := survivor.status(http.MethodGet)
		require.NoError(t, err)
		require.False(t, status.TransportRunning)
		require.False(t, status.Leader)
		require.False(t, status.NotifiedLeader)
		require.True(t, status.PrimaryUnchanged)
		t.Logf("startup failure fixture: %s; no transport, membership or leadership; native app SQL unchanged", discovery)
	} else {
		// Wait across discovery/heartbeat attempts before warming caches. The
		// fallback must work without ever learning this writer's endpoint.
		require.Never(t, func() bool {
			status, err := survivor.status(http.MethodGet)
			return err != nil || status.Peers != 1 || status.CachePeersMissing
		}, 3*time.Second, 100*time.Millisecond)
		var registered int
		require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM ClusterDiscovery WHERE Id=$1`, source.id).Scan(&registered))
		if discovery == "missing writer record" {
			require.Zero(t, registered, "heartbeats must not recreate the deliberately absent writer record")
		} else {
			require.Equal(t, 1, registered, "the writer is registered but the newcomer cannot read discovery")
		}
		t.Logf("discovery fixture: mode=%s writer_rows=%d; native SQL and leadership remain available", discovery, registered)
	}
	status, err := survivor.status(http.MethodGet)
	require.NoError(t, err)
	if status.CachePeersMissing {
		// If startup recovery was scheduled, wait for its actual completion
		// before priming. Later writes must require a subsequent recheck.
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(survivor.logPath)
			return err == nil && strings.Contains(string(data), "Local cluster caches refreshed")
		}, 5*time.Second, 50*time.Millisecond)
	}
	require.Zero(t, firstProxy.snapshot().TCPBytes)
	require.Zero(t, secondProxy.snapshot().TCPBytes)
	t.Logf("newcomer started with SQL available but no remote gossip membership; discovery=%s startup_missing_peer=%t", discovery, status.CachePeersMissing)
	clientFor := func(node *clusterTestProcess, token string) *model.Client4 {
		client := model.NewAPIv4Client(node.url)
		client.SetToken(token)
		client.HTTPClient.Timeout = 10 * time.Second
		return client
	}
	sourceClient := clientFor(source, th.Client.AuthToken)
	survivorClient := clientFor(survivor, th.Client.AuthToken)
	revokedClient := clientFor(survivor, "")
	th.LoginBasicWithClient(t, revokedClient)
	_, _, err = revokedClient.GetMe(th.Context.Context(), "")
	require.NoError(t, err)
	before, _, err := survivorClient.GetUser(th.Context.Context(), th.BasicUser.Id, "")
	require.NoError(t, err)
	connect := func(token string) *model.WebSocketClient {
		ws, err := model.NewWebSocketClient4(strings.Replace(survivor.url, "http://", "ws://", 1), token)
		require.NoError(t, err)
		ws.Listen()
		t.Cleanup(ws.Close)
		select {
		case response := <-ws.ResponseChannel:
			require.NotNil(t, response)
			require.Equal(t, model.StatusOk, response.Status)
		case <-time.After(5 * time.Second):
			t.Fatal("survivor websocket did not authenticate")
		}
		return ws
	}
	validWS, revokedWS := connect(th.Client.AuthToken), connect(revokedClient.AuthToken)
	nickname := "startup-partition-" + model.NewId()
	_, _, err = sourceClient.PatchUser(th.Context.Context(), th.BasicUser.Id, &model.UserPatch{Nickname: &nickname})
	require.NoError(t, err)
	logoutClient := clientFor(source, revokedClient.AuthToken)
	_, err = logoutClient.Logout(th.Context.Context())
	require.NoError(t, err)
	var storedNickname string
	var remainingSessions int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT Nickname FROM Users WHERE Id=$1`, th.BasicUser.Id).Scan(&storedNickname))
	require.Equal(t, nickname, storedNickname)
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM Sessions WHERE Token=$1`, revokedClient.AuthToken).Scan(&remainingSessions))
	require.Zero(t, remainingSessions, "logout must be committed before killing its source")
	stale, _, err := survivorClient.GetUser(th.Context.Context(), th.BasicUser.Id, "")
	require.NoError(t, err)
	require.Equal(t, before.Nickname, stale.Nickname)
	_, _, err = revokedClient.GetMe(th.Context.Context(), "")
	require.NoError(t, err, "fixture must reproduce a stale authenticated session before failure detection")
	if !failedTransport {
		require.Eventually(t, func() bool { return secondProxy.snapshot().TCPDropped > 0 }, 3*time.Second, 50*time.Millisecond)
	}
	a, err := source.status(http.MethodGet)
	require.NoError(t, err)
	b, err := survivor.status(http.MethodGet)
	require.NoError(t, err)
	require.Equal(t, 1, a.Peers)
	require.Equal(t, survivorPeers, b.Peers)
	require.Zero(t, secondProxy.snapshot().TCPBytes)
	t.Logf("committed user update and session revocation after newcomer startup with no remote membership; proxy=%+v", secondProxy.snapshot())
	start := time.Now()
	source.killed = true
	require.NoError(t, source.cmd.Process.Kill())
	select {
	case <-source.done:
		require.Error(t, source.err, "source must die without graceful leave or a final state transfer")
	case <-time.After(5 * time.Second):
		t.Fatal("crashed source was not reaped")
	}
	if !failedTransport {
		require.Eventually(t, func() bool {
			status, err := survivor.status(http.MethodGet)
			return err == nil && status.Peers == 1 && status.Leader && status.NotifiedLeader
		}, 25*time.Second, 100*time.Millisecond)
		t.Logf("survivor remains alone and holds SQL leadership after %d ms", time.Since(start).Milliseconds())
	}
	require.Eventually(t, func() bool {
		user, _, userErr := survivorClient.GetUser(th.Context.Context(), th.BasicUser.Id, "")
		_, response, sessionErr := revokedClient.GetMe(th.Context.Context(), "")
		return userErr == nil && user.Nickname == nickname && sessionErr != nil && response != nil && response.StatusCode == http.StatusUnauthorized
	}, 45*time.Second, 100*time.Millisecond, "the newcomer must repair user and session writes despite never observing a remote membership event")
	t.Logf("startup-partition user cache and revoked HTTP session repaired after source crash in %d ms; TCP remains blocked", time.Since(start).Milliseconds())
	if failedTransport {
		status, err := survivor.status(http.MethodGet)
		require.NoError(t, err)
		require.False(t, status.TransportRunning)
		require.Zero(t, status.Peers)
		require.False(t, status.Leader)
		require.False(t, status.NotifiedLeader)
		require.Zero(t, secondProxy.snapshot().TCPBytes)
		t.Log("cache and session recovery did not pretend to start transport or acquire leadership")
	}
	// The pre-existing valid socket must stay usable; the revoked socket must
	// reload its session instead of retaining an authenticated WebConn forever.
	revokedWS.GetStatusesByIds([]string{th.BasicUser.Id})
	select {
	case response := <-revokedWS.ResponseChannel:
		require.NotNil(t, response)
		require.NotEqual(t, model.StatusOk, response.Status)
		require.NotNil(t, response.Error)
		require.Equal(t, http.StatusUnauthorized, response.Error.StatusCode)
	case <-time.After(5 * time.Second):
		t.Fatal("revoked websocket session was not rejected")
	}
	post, _, err := survivorClient.CreatePost(th.Context.Context(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "surviving websocket keeps its valid session"})
	require.NoError(t, err)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-validWS.EventChannel:
			require.NotNil(t, event, "valid websocket closed after cache recovery")
			if event.EventType() != model.WebsocketEventPosted {
				continue
			}
			var received model.Post
			if raw, ok := event.GetData()["post"].(string); ok && json.Unmarshal([]byte(raw), &received) == nil && received.Id == post.Id {
				return
			}
		case <-deadline:
			t.Fatal("valid pre-existing websocket stopped receiving after source crash")
		}
	}
}
