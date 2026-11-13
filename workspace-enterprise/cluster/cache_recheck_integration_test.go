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

func TestSeparateProcessesPostDepartureWritesRecoverAfterSourceCrash(t *testing.T) {
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
	firstProxy := newGossipProxy(t, "127.0.0.16", "127.0.0.6")
	secondProxy := newGossipProxy(t, "127.0.0.17", "127.0.0.7")
	configure := func(bind, advertised string, proxy *gossipProxy) *model.Config {
		result := cfg.Clone()
		result.ClusterSettings.BindAddress = new(bind)
		result.ClusterSettings.AdvertiseAddress = new(advertised)
		result.ClusterSettings.OverrideHostname = new(advertised)
		result.ClusterSettings.GossipPort = new(proxy.port)
		return result
	}
	source := startClusterTestProcess(t, configure("127.0.0.6", "127.0.0.16", firstProxy))
	survivor := startClusterTestProcess(t, configure("127.0.0.7", "127.0.0.17", secondProxy))
	t.Cleanup(func() { firstProxy.partition(false); secondProxy.partition(false) })
	require.Eventually(t, func() bool {
		a, ea := source.status(http.MethodGet)
		b, eb := survivor.status(http.MethodGet)
		return ea == nil && eb == nil && a.Peers == 2 && b.Peers == 2 && a.Leader != b.Leader
	}, 20*time.Second, 100*time.Millisecond)
	purges := func(node *clusterTestProcess) int {
		data, err := os.ReadFile(node.logPath)
		require.NoError(t, err)
		return strings.Count(string(data), "Local cluster caches refreshed")
	}
	require.Eventually(t, func() bool { return purges(source) > 0 && purges(survivor) > 0 }, 5*time.Second, 50*time.Millisecond)
	sourcePurges, survivorPurges := purges(source), purges(survivor)
	firstProxy.partition(true)
	secondProxy.partition(true)
	require.Eventually(t, func() bool {
		a, ea := source.status(http.MethodGet)
		b, eb := survivor.status(http.MethodGet)
		return ea == nil && eb == nil && a.Peers == 1 && b.Peers == 1
	}, 25*time.Second, 100*time.Millisecond)
	// Observe completion of the actual native leave purge before priming or
	// writing. A delay alone would not establish the required ordering.
	require.Eventually(t, func() bool {
		return purges(source) > sourcePurges && purges(survivor) > survivorPurges
	}, 5*time.Second, 50*time.Millisecond)
	t.Logf("both nodes completed departure purges; subsequent writes happen while TCP and UDP remain isolated")
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
	_, _, err := revokedClient.GetMe(th.Context.Context(), "")
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
	nickname := "post-departure-" + model.NewId()
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
	require.Eventually(t, func() bool { return secondProxy.snapshot().TCPDropped > 0 }, 3*time.Second, 50*time.Millisecond)
	a, err := source.status(http.MethodGet)
	require.NoError(t, err)
	b, err := survivor.status(http.MethodGet)
	require.NoError(t, err)
	require.Equal(t, 1, a.Peers)
	require.Equal(t, 1, b.Peers)
	require.Positive(t, secondProxy.snapshot().UDPDropped)
	t.Logf("committed user update and session revocation after completed departure purges; proxy=%+v", secondProxy.snapshot())
	start := time.Now()
	source.killed = true
	require.NoError(t, source.cmd.Process.Kill())
	select {
	case <-source.done:
		require.Error(t, source.err, "source must die without graceful leave or a final state transfer")
	case <-time.After(5 * time.Second):
		t.Fatal("crashed source was not reaped")
	}
	require.Eventually(t, func() bool {
		status, err := survivor.status(http.MethodGet)
		return err == nil && status.Peers == 1 && status.Leader && status.NotifiedLeader
	}, 25*time.Second, 100*time.Millisecond)
	t.Logf("survivor remains alone and holds SQL leadership after %d ms", time.Since(start).Milliseconds())
	require.Eventually(t, func() bool {
		user, _, userErr := survivorClient.GetUser(th.Context.Context(), th.BasicUser.Id, "")
		_, response, sessionErr := revokedClient.GetMe(th.Context.Context(), "")
		return userErr == nil && user.Nickname == nickname && sessionErr != nil && response != nil && response.StatusCode == http.StatusUnauthorized
	}, 45*time.Second, 100*time.Millisecond, "the survivor must repair post-departure user and session writes despite no new leave or rejoin event")
	t.Logf("post-departure user cache and revoked HTTP session repaired after source crash in %d ms; TCP remains blocked", time.Since(start).Milliseconds())
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
