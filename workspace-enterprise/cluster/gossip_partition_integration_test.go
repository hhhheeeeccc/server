package cluster

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

type gossipProxyStats struct {
	TCPAccepted int   `json:"tcp_accepted"`
	TCPBytes    int64 `json:"tcp_bytes"`
	TCPDropped  int64 `json:"tcp_dropped_bytes"`
	UDPPackets  int64 `json:"udp_packets"`
	UDPDropped  int64 `json:"udp_dropped_packets"`
}

// The advertised address differs from the node's bind address, on loopback.
// Both transports pass through this proxy; no host firewall or live service is
// involved. Blackholing drops payloads in both directions without decoding the
// encrypted protocol or closing established TCP sockets until recovery.
type gossipProxy struct {
	tcp       net.Listener
	udp       *net.UDPConn
	target    string
	port      int
	acceptEnd chan struct{}
	udpEnd    chan struct{}
	workers   sync.WaitGroup
	mu        sync.Mutex
	closed    bool
	blocked   bool
	tcpOnly   bool
	links     map[net.Conn]net.Conn
	clients   map[string]*net.UDPConn
	stats     gossipProxyStats
}

func newGossipProxy(t *testing.T, advertised, bind string) *gossipProxy {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(advertised, "0"))
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(advertised), Port: port})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	proxy := &gossipProxy{tcp: listener, udp: udp, target: net.JoinHostPort(bind, strconv.Itoa(port)), port: port,
		acceptEnd: make(chan struct{}), udpEnd: make(chan struct{}), links: make(map[net.Conn]net.Conn), clients: make(map[string]*net.UDPConn)}
	go proxy.accept()
	go proxy.datagrams()
	t.Cleanup(func() {
		proxy.mu.Lock()
		proxy.closed = true
		proxy.mu.Unlock()
		_ = listener.Close()
		_ = udp.Close()
		// Only the two listener loops add workers. Join them before Wait.
		<-proxy.acceptEnd
		<-proxy.udpEnd
		proxy.mu.Lock()
		for client, upstream := range proxy.links {
			_ = client.Close()
			_ = upstream.Close()
		}
		for _, upstream := range proxy.clients {
			_ = upstream.Close()
		}
		proxy.mu.Unlock()
		proxy.workers.Wait()
	})
	return proxy
}

func (proxy *gossipProxy) accept() {
	defer close(proxy.acceptEnd)
	for {
		client, err := proxy.tcp.Accept()
		if err != nil {
			return
		}
		proxy.workers.Add(1)
		go proxy.relay(client)
	}
}

func (proxy *gossipProxy) relay(client net.Conn) {
	defer proxy.workers.Done()
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", proxy.target, time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	proxy.mu.Lock()
	if proxy.closed {
		proxy.mu.Unlock()
		return
	}
	proxy.links[client] = upstream
	proxy.stats.TCPAccepted++
	proxy.mu.Unlock()
	defer func() { proxy.mu.Lock(); delete(proxy.links, client); proxy.mu.Unlock() }()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(gossipProxyWriter{proxy, upstream}, client)
		_ = client.Close()
		_ = upstream.Close()
		close(done)
	}()
	_, _ = io.Copy(gossipProxyWriter{proxy, client}, upstream)
	_ = client.Close()
	_ = upstream.Close()
	<-done
}

type gossipProxyWriter struct {
	proxy *gossipProxy
	net.Conn
}

func (writer gossipProxyWriter) Write(data []byte) (int, error) {
	writer.proxy.mu.Lock()
	blocked := writer.proxy.blocked || writer.proxy.tcpOnly
	if blocked {
		writer.proxy.stats.TCPDropped += int64(len(data))
	} else {
		writer.proxy.stats.TCPBytes += int64(len(data))
	}
	writer.proxy.mu.Unlock()
	if blocked {
		return len(data), nil
	}
	return writer.Conn.Write(data)
}

func (proxy *gossipProxy) dropDatagram() bool {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if proxy.blocked {
		proxy.stats.UDPDropped++
	} else {
		proxy.stats.UDPPackets++
	}
	return proxy.blocked
}

func (proxy *gossipProxy) datagrams() {
	defer close(proxy.udpEnd)
	target, _ := net.ResolveUDPAddr("udp", proxy.target)
	buffer := make([]byte, 65536)
	for {
		count, client, err := proxy.udp.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		if proxy.dropDatagram() {
			continue
		}
		key := client.String()
		proxy.mu.Lock()
		upstream := proxy.clients[key]
		if upstream == nil && !proxy.closed {
			upstream, err = net.DialUDP("udp", nil, target)
			if err == nil {
				proxy.clients[key] = upstream
				proxy.workers.Add(1)
				go proxy.udpReplies(upstream, client)
			}
		}
		proxy.mu.Unlock()
		if upstream != nil {
			_, _ = upstream.Write(buffer[:count])
		}
	}
}

func (proxy *gossipProxy) udpReplies(upstream *net.UDPConn, client *net.UDPAddr) {
	defer proxy.workers.Done()
	buffer := make([]byte, 65536)
	for {
		count, err := upstream.Read(buffer)
		if err != nil {
			return
		}
		if !proxy.dropDatagram() {
			_, _ = proxy.udp.WriteToUDP(buffer[:count], client)
		}
	}
}

func (proxy *gossipProxy) partition(blocked bool) {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	wasBlocked := proxy.blocked
	proxy.blocked = blocked
	if wasBlocked && !blocked {
		// Dropped stream bytes cannot be replayed; force a fresh connection.
		for client, upstream := range proxy.links {
			_ = client.Close()
			_ = upstream.Close()
		}
	}
}

func (proxy *gossipProxy) snapshot() gossipProxyStats {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	return proxy.stats
}

func (proxy *gossipProxy) partitionTCP(blocked bool) {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	wasBlocked := proxy.tcpOnly
	proxy.tcpOnly = blocked
	if wasBlocked && !blocked {
		for client, upstream := range proxy.links {
			_ = client.Close()
			_ = upstream.Close()
		}
	}
}

func TestSeparateProcessesLostInvalidationWithoutMembershipChange(t *testing.T) {
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
	firstProxy := newGossipProxy(t, "127.0.0.14", "127.0.0.4")
	secondProxy := newGossipProxy(t, "127.0.0.15", "127.0.0.5")
	configure := func(bind, advertise string, proxy *gossipProxy) *model.Config {
		result := cfg.Clone()
		result.ClusterSettings.BindAddress = new(bind)
		result.ClusterSettings.AdvertiseAddress = new(advertise)
		result.ClusterSettings.OverrideHostname = new(advertise)
		result.ClusterSettings.GossipPort = new(proxy.port)
		return result
	}
	first := startClusterTestProcess(t, configure("127.0.0.4", "127.0.0.14", firstProxy))
	second := startClusterTestProcess(t, configure("127.0.0.5", "127.0.0.15", secondProxy))
	t.Cleanup(func() { firstProxy.partitionTCP(false); secondProxy.partitionTCP(false) })
	statusOK := func() bool {
		a, ea := first.status(http.MethodGet)
		b, eb := second.status(http.MethodGet)
		return ea == nil && eb == nil && a.Peers == 2 && b.Peers == 2 && a.Leader != b.Leader && a.Leader == a.NotifiedLeader && b.Leader == b.NotifiedLeader
	}
	require.Eventually(t, statusOK, 20*time.Second, 100*time.Millisecond)
	// Allow the initial join purge to finish before priming the cache.
	time.Sleep(2 * time.Second)
	clientFor := func(node *clusterTestProcess) *model.Client4 {
		client := model.NewAPIv4Client(node.url)
		client.SetToken(th.Client.AuthToken)
		client.HTTPClient.Timeout = 10 * time.Second
		return client
	}
	firstClient, secondClient := clientFor(first), clientFor(second)
	before, _, err := secondClient.GetUser(th.Context.Context(), th.BasicUser.Id, "")
	require.NoError(t, err)
	firstProxy.partitionTCP(true)
	secondProxy.partitionTCP(true)
	nickname := "lost-invalidation-" + model.NewId()
	_, _, err = firstClient.PatchUser(th.Context.Context(), th.BasicUser.Id, &model.UserPatch{Nickname: &nickname})
	require.NoError(t, err)
	until := time.Now().Add(4 * time.Second)
	for time.Now().Before(until) {
		require.True(t, statusOK(), "UDP membership must remain healthy during TCP loss")
		time.Sleep(100 * time.Millisecond)
	}
	stale, _, err := secondClient.GetUser(th.Context.Context(), th.BasicUser.Id, "")
	require.NoError(t, err)
	require.Equal(t, before.Nickname, stale.Nickname, "fixture must lose a real invalidation")
	stats := secondProxy.snapshot()
	require.Positive(t, stats.TCPDropped)
	require.Zero(t, stats.UDPDropped)
	require.Positive(t, stats.UDPPackets)
	t.Logf("lost invalidation while UDP membership stayed healthy: %+v", stats)
	firstProxy.partitionTCP(false)
	secondProxy.partitionTCP(false)
	id := model.NewId()
	code, body, err := first.probe(second.id, id)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code, body)
	state, err := second.status(http.MethodGet)
	require.NoError(t, err)
	require.Equal(t, id, state.LastProbe)
	start := time.Now()
	require.Eventually(t, func() bool {
		user, _, err := secondClient.GetUser(th.Context.Context(), th.BasicUser.Id, "")
		return statusOK() && err == nil && user.Nickname == nickname
	}, 70*time.Second, 250*time.Millisecond, "a lost invalidation must be recovered without another edit or membership change")
	require.True(t, statusOK(), "cache repair must not depend on leave/rejoin")
	t.Logf("cache repaired without membership change after %d ms", time.Since(start).Milliseconds())
}

func TestSeparateProcessesGossipPartitionAndCacheRecovery(t *testing.T) {
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
	firstProxy := newGossipProxy(t, "127.0.0.12", "127.0.0.2")
	secondProxy := newGossipProxy(t, "127.0.0.13", "127.0.0.3")
	configure := func(bind, advertise string, proxy *gossipProxy) *model.Config {
		result := cfg.Clone()
		result.ClusterSettings.BindAddress = new(bind)
		result.ClusterSettings.AdvertiseAddress = new(advertise)
		result.ClusterSettings.OverrideHostname = new(advertise)
		result.ClusterSettings.GossipPort = new(proxy.port)
		return result
	}
	first := startClusterTestProcess(t, configure("127.0.0.2", "127.0.0.12", firstProxy))
	second := startClusterTestProcess(t, configure("127.0.0.3", "127.0.0.13", secondProxy))
	// Restore first on any assertion failure, before child shutdown cleanups.
	t.Cleanup(func() { firstProxy.partition(false); secondProxy.partition(false) })
	statuses := func(peers int) bool {
		a, ea := first.status(http.MethodGet)
		b, eb := second.status(http.MethodGet)
		return ea == nil && eb == nil && a.Peers == peers && b.Peers == peers && a.Leader != b.Leader && a.Leader == a.NotifiedLeader && b.Leader == b.NotifiedLeader
	}
	require.Eventually(t, func() bool { return statuses(2) }, 20*time.Second, 100*time.Millisecond)
	require.Eventually(t, func() bool { return firstProxy.snapshot().UDPPackets > 0 && secondProxy.snapshot().UDPPackets > 0 }, 10*time.Second, 100*time.Millisecond)
	clientFor := func(node *clusterTestProcess) *model.Client4 {
		client := model.NewAPIv4Client(node.url)
		client.SetToken(th.Client.AuthToken)
		client.HTTPClient.Timeout = 15 * time.Second
		return client
	}
	firstClient, secondClient := clientFor(first), clientFor(second)
	connect := func(node *clusterTestProcess) *model.WebSocketClient {
		ws, err := model.NewWebSocketClient4(strings.Replace(node.url, "http://", "ws://", 1), th.Client.AuthToken)
		require.NoError(t, err)
		ws.Listen()
		t.Cleanup(ws.Close)
		select {
		case response := <-ws.ResponseChannel:
			require.NotNil(t, response)
			require.Equal(t, model.StatusOk, response.Status)
		case <-time.After(5 * time.Second):
			t.Fatal("partition test websocket did not authenticate")
		}
		return ws
	}
	firstWS, secondWS := connect(first), connect(second)
	createAndReceive := func(sender *model.Client4, receiver *model.WebSocketClient, text string) *model.Post {
		t.Helper()
		post, _, err := sender.CreatePost(th.Context.Context(), &model.Post{ChannelId: th.BasicChannel.Id, Message: text})
		require.NoError(t, err)
		deadline := time.After(8 * time.Second)
		for {
			select {
			case event := <-receiver.EventChannel:
				require.NotNil(t, event, "websocket closed during partition test")
				if event.EventType() != model.WebsocketEventPosted {
					continue
				}
				var received model.Post
				if data, ok := event.GetData()["post"].(string); ok && json.Unmarshal([]byte(data), &received) == nil && received.Id == post.Id {
					require.Equal(t, text, received.Message)
					return post
				}
			case <-deadline:
				t.Fatal("post did not reach expected websocket")
			}
		}
	}
	probe := func(from, to *clusterTestProcess) {
		t.Helper()
		id := model.NewId()
		before, err := to.status(http.MethodGet)
		require.NoError(t, err)
		code, body, err := from.probe(to.id, id)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, code, body)
		after, err := to.status(http.MethodGet)
		require.NoError(t, err)
		require.Equal(t, before.ProbeCount+1, after.ProbeCount)
		require.Equal(t, id, after.LastProbe)
	}
	createAndReceive(firstClient, secondWS, "before gossip partition")
	probe(first, second)
	beforeUser, _, err := secondClient.GetUser(th.Context.Context(), th.BasicUser.Id, "")
	require.NoError(t, err)
	initial, err := first.status(http.MethodGet)
	require.NoError(t, err)
	initialOwner := first.id
	if !initial.Leader {
		initialOwner = second.id
	}
	firstProxy.partition(true)
	secondProxy.partition(true)
	partitionStart := time.Now()
	beforeProbe, err := second.status(http.MethodGet)
	require.NoError(t, err)
	code, body, err := first.probe(second.id, model.NewId())
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, code, body)
	require.NotEmpty(t, strings.TrimSpace(body))
	afterProbe, err := second.status(http.MethodGet)
	require.NoError(t, err)
	require.Equal(t, beforeProbe.ProbeCount, afterProbe.ProbeCount, "a failed isolated probe must not execute remotely")
	require.Eventually(t, func() bool { return statuses(1) }, 35*time.Second, 100*time.Millisecond)
	t.Logf("gossip isolation observed after %d ms; reliable failure: %s", time.Since(partitionStart).Milliseconds(), strings.TrimSpace(body))
	// Observe for longer than the SQL lease. Gossip alone must not transfer it.
	until := time.Now().Add(12 * time.Second)
	for time.Now().Before(until) {
		require.True(t, statuses(1), "gossip partition changed leadership or its notification")
		var owner string
		var valid bool
		require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT OwnerId, ExpiresAt > clock_timestamp() FROM WorkspaceClusterLeases WHERE ClusterName=$1`, *cfg.ClusterSettings.ClusterName).Scan(&owner, &valid))
		require.Equal(t, initialOwner, owner)
		require.True(t, valid)
		time.Sleep(250 * time.Millisecond)
	}
	post := createAndReceive(firstClient, firstWS, "first stays available in partition")
	createAndReceive(secondClient, secondWS, "second stays available in partition")
	read, _, err := secondClient.GetPost(th.Context.Context(), post.Id, "")
	require.NoError(t, err)
	require.Equal(t, post.Message, read.Message, "shared SQL retains a post even when its live event is lost")
	nickname := "partition-" + model.NewId()
	_, _, err = firstClient.PatchUser(th.Context.Context(), th.BasicUser.Id, &model.UserPatch{Nickname: &nickname})
	require.NoError(t, err)
	isolatedUser, _, err := secondClient.GetUser(th.Context.Context(), th.BasicUser.Id, "")
	require.NoError(t, err)
	require.Equal(t, beforeUser.Nickname, isolatedUser.Nickname, "the fixture must reproduce a missed cache invalidation")
	for _, proxy := range []*gossipProxy{firstProxy, secondProxy} {
		stats := proxy.snapshot()
		require.Positive(t, stats.TCPDropped)
		require.Positive(t, stats.UDPDropped)
		t.Logf("partition proxy counters: %+v", stats)
	}
	healStart := time.Now()
	firstProxy.partition(false)
	secondProxy.partition(false)
	require.Eventually(t, func() bool { return statuses(2) }, 25*time.Second, 100*time.Millisecond)
	t.Logf("gossip rejoined after %d ms", time.Since(healStart).Milliseconds())
	probe(first, second)
	probe(second, first)
	createAndReceive(firstClient, secondWS, "after healing first to second")
	createAndReceive(secondClient, firstWS, "after healing second to first")
	require.Eventually(t, func() bool {
		user, _, err := secondClient.GetUser(th.Context.Context(), th.BasicUser.Id, "")
		return err == nil && user.Nickname == nickname
	}, 8*time.Second, 100*time.Millisecond, "rejoining must repair a cache invalidation lost during partition without a second edit or process restart")
}
