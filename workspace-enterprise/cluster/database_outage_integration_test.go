package cluster

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

type databaseFaultMode uint8

const (
	databaseForward databaseFaultMode = iota
	databaseDisconnect
	databaseBlackhole
)

// A private TCP proxy for each test process: disconnect closes established
// sockets and rejects new ones; blackhole keeps sockets open but discards all
// bytes in both directions, including PostgreSQL cancellation requests. It
// records byte counts only, never database payloads or credentials.
type clusterDatabaseProxy struct {
	listener net.Listener
	target   string
	done     chan struct{}
	workers  sync.WaitGroup
	mu       sync.Mutex
	mode     databaseFaultMode
	closed   bool
	links    map[net.Conn]net.Conn
	accepted int
	rejected int
	dropped  int64
}

func newClusterDatabaseProxy(t *testing.T, dsn string) (*clusterDatabaseProxy, string) {
	t.Helper()
	address, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, address.Scheme, "outage tests require a PostgreSQL URI")
	target := address.Host
	if address.Port() == "" {
		target = net.JoinHostPort(address.Hostname(), "5432")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	proxy := &clusterDatabaseProxy{listener: listener, target: target, done: make(chan struct{}), links: make(map[net.Conn]net.Conn)}
	go proxy.accept()
	t.Cleanup(func() {
		proxy.mu.Lock()
		proxy.closed = true
		proxy.mu.Unlock()
		_ = listener.Close()
		<-proxy.done
		proxy.setMode(databaseDisconnect)
		proxy.workers.Wait()
	})
	address.Host = listener.Addr().String()
	return proxy, address.String()
}

func (proxy *clusterDatabaseProxy) accept() {
	defer close(proxy.done)
	for {
		client, err := proxy.listener.Accept()
		if err != nil {
			return
		}
		proxy.mu.Lock()
		reject := proxy.closed || proxy.mode == databaseDisconnect
		if reject {
			proxy.rejected++
		} else {
			proxy.accepted++
		}
		proxy.mu.Unlock()
		if reject {
			_ = client.Close()
			continue
		}
		proxy.workers.Add(1)
		go proxy.relay(client)
	}
}

func (proxy *clusterDatabaseProxy) relay(client net.Conn) {
	defer proxy.workers.Done()
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", proxy.target, 2*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	proxy.mu.Lock()
	if proxy.closed || proxy.mode == databaseDisconnect {
		proxy.mu.Unlock()
		return
	}
	proxy.links[client] = upstream
	proxy.mu.Unlock()
	defer func() { proxy.mu.Lock(); delete(proxy.links, client); proxy.mu.Unlock() }()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(clusterProxyWriter{proxy, upstream}, client)
		_ = upstream.Close()
		_ = client.Close()
		close(done)
	}()
	_, _ = io.Copy(clusterProxyWriter{proxy, client}, upstream)
	_ = upstream.Close()
	_ = client.Close()
	<-done
}

type clusterProxyWriter struct {
	proxy *clusterDatabaseProxy
	net.Conn
}

func (writer clusterProxyWriter) Write(data []byte) (int, error) {
	writer.proxy.mu.Lock()
	drop := writer.proxy.mode == databaseBlackhole
	if drop {
		writer.proxy.dropped += int64(len(data))
	}
	writer.proxy.mu.Unlock()
	if drop {
		return len(data), nil
	}
	return writer.Conn.Write(data)
}

func (proxy *clusterDatabaseProxy) setMode(mode databaseFaultMode) {
	proxy.mu.Lock()
	previous := proxy.mode
	proxy.mode = mode
	// Restoring a silent connection requires a reconnect: discarded protocol
	// bytes cannot be replayed. Closing also unblocks pending driver calls.
	if mode == databaseDisconnect || (mode == databaseForward && previous != databaseForward) {
		for client, upstream := range proxy.links {
			_ = client.Close()
			_ = upstream.Close()
		}
	}
	proxy.mu.Unlock()
}

func TestSeparateProcessesDatabaseOutage(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	}).InitBasic(t)
	cfg := th.Server.Config().Clone()
	cfg.ClusterSettings.Enable = new(true)
	cfg.ClusterSettings.ClusterName = new(model.NewId())
	cfg.ClusterSettings.BindAddress = new("127.0.0.1")
	cfg.ClusterSettings.AdvertiseAddress = new("127.0.0.1")
	cfg.ClusterSettings.ReadOnlyConfig = new(false)
	cfg.ServiceSettings.ListenAddress = new("127.0.0.1:0")
	cfg.ServiceSettings.EnableLocalMode = new(false)
	cfg.PluginSettings.Enable = new(false)
	cfg.LogSettings.EnableFile = new(false)
	cfg.SqlSettings.DataSourceReplicas = nil
	cfg.SqlSettings.MaxOpenConns = new(15)
	firstProxy, firstDSN := newClusterDatabaseProxy(t, *cfg.SqlSettings.DataSource)
	secondProxy, secondDSN := newClusterDatabaseProxy(t, *cfg.SqlSettings.DataSource)
	firstCfg, secondCfg := cfg.Clone(), cfg.Clone()
	firstCfg.SqlSettings.DataSource = &firstDSN
	secondCfg.SqlSettings.DataSource = &secondDSN
	firstCfg.ClusterSettings.GossipPort = new(freePort(t))
	secondCfg.ClusterSettings.GossipPort = new(freePort(t))
	first, second := startClusterTestProcess(t, firstCfg), startClusterTestProcess(t, secondCfg)
	// Restore before process cleanup even when a fatal assertion interrupts an
	// outage. Otherwise a deliberately stuck driver could delay shutdown.
	t.Cleanup(func() { firstProxy.setMode(databaseForward); secondProxy.setMode(databaseForward) })
	require.Eventually(t, func() bool {
		a, ea := first.status(http.MethodGet)
		b, eb := second.status(http.MethodGet)
		return ea == nil && eb == nil && a.Peers == 2 && b.Peers == 2 && a.Leader && a.NotifiedLeader && !b.Leader && !b.NotifiedLeader
	}, 15*time.Second, 100*time.Millisecond)
	proxies := map[*clusterTestProcess]*clusterDatabaseProxy{first: firstProxy, second: secondProxy}
	assertOwner := func(t *testing.T, node *clusterTestProcess) {
		t.Helper()
		var owner string
		require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT OwnerId FROM WorkspaceClusterLeases WHERE ClusterName=$1`, *cfg.ClusterSettings.ClusterName).Scan(&owner))
		require.Equal(t, node.id, owner)
	}
	assertHTTPAndWS := func(t *testing.T, node *clusterTestProcess) {
		t.Helper()
		client := model.NewAPIv4Client(node.url)
		client.SetToken(th.Client.AuthToken)
		client.HTTPClient.Timeout = 10 * time.Second
		ws, err := model.NewWebSocketClient4(strings.Replace(node.url, "http://", "ws://", 1), th.Client.AuthToken)
		require.NoError(t, err)
		ws.Listen()
		defer ws.Close()
		select {
		case response := <-ws.ResponseChannel:
			require.Equal(t, model.StatusOk, response.Status)
		case <-time.After(5 * time.Second):
			t.Fatal("healthy peer websocket did not authenticate")
		}
		post, _, err := client.CreatePost(th.Context.Context(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "Healthy peer during SQL outage"})
		require.NoError(t, err)
		deadline := time.After(5 * time.Second)
		for {
			select {
			case event := <-ws.EventChannel:
				require.NotNil(t, event)
				if event.EventType() != model.WebsocketEventPosted {
					continue
				}
				var received model.Post
				data, ok := event.GetData()["post"].(string)
				if ok && json.Unmarshal([]byte(data), &received) == nil && received.Id == post.Id {
					return
				}
			case <-deadline:
				t.Fatal("healthy peer websocket stalled during SQL outage")
			}
		}
	}
	for _, test := range []struct {
		name    string
		mode    databaseFaultMode
		offline *clusterTestProcess
		healthy *clusterTestProcess
	}{
		{"silent database connection", databaseBlackhole, first, second},
		{"disconnected database", databaseDisconnect, second, first},
	} {
		if !t.Run(test.name, func(t *testing.T) {
			start := time.Now()
			t.Cleanup(func() {
				status, err := test.offline.status(http.MethodGet)
				healthyStatus, healthyErr := test.healthy.status(http.MethodGet)
				proxy := proxies[test.offline]
				proxy.mu.Lock()
				accepted, rejected, dropped := proxy.accepted, proxy.rejected, proxy.dropped
				proxy.mu.Unlock()
				t.Logf("outage observation: mode=%d leader=%t notified_leader=%t status_error=%v accepted=%d rejected=%d dropped_bytes=%d", test.mode, status.Leader, status.NotifiedLeader, err, accepted, rejected, dropped)
				t.Logf("database configuration observation: offline_primary_unchanged=%t healthy_primary_unchanged=%t healthy_leader=%t healthy_notified_leader=%t healthy_status_error=%v", status.PrimaryUnchanged, healthyStatus.PrimaryUnchanged, healthyStatus.Leader, healthyStatus.NotifiedLeader, healthyErr)
				proxy.setMode(databaseForward)
			})
			proxies[test.offline].setMode(test.mode)
			require.Eventually(t, func() bool {
				status, err := test.offline.status(http.MethodGet)
				return err == nil && !status.Leader && !status.NotifiedLeader
			}, 9*time.Second, 100*time.Millisecond, "lease expiry must notify listeners even if the driver never returns")
			t.Logf("database outage leadership revoked: mode=%d pid=%d elapsed_ms=%d", test.mode, test.offline.cmd.Process.Pid, time.Since(start).Milliseconds())
			require.Eventually(t, func() bool {
				status, err := test.healthy.status(http.MethodGet)
				return err == nil && status.Leader && status.NotifiedLeader
			}, 15*time.Second, 100*time.Millisecond)
			assertOwner(t, test.healthy)
			assertHTTPAndWS(t, test.healthy)
			proxy := proxies[test.offline]
			proxy.mu.Lock()
			dropped, rejected := proxy.dropped, proxy.rejected
			proxy.mu.Unlock()
			if test.mode == databaseBlackhole {
				require.Positive(t, dropped)
			} else {
				require.Positive(t, rejected)
			}
			proxies[test.offline].setMode(databaseForward)
			require.Eventually(t, func() bool {
				a, ea := test.offline.status(http.MethodGet)
				b, eb := test.healthy.status(http.MethodGet)
				return ea == nil && eb == nil && a.Peers == 2 && b.Peers == 2 && !a.Leader && !a.NotifiedLeader && b.Leader && b.NotifiedLeader
			}, 10*time.Second, 100*time.Millisecond)
			assertHTTPAndWS(t, test.offline)
		}) {
			return
		}
	}
	t.Run("both database connections lost", func(t *testing.T) {
		firstProxy.setMode(databaseDisconnect)
		secondProxy.setMode(databaseDisconnect)
		require.Eventually(t, func() bool {
			a, ea := first.status(http.MethodGet)
			b, eb := second.status(http.MethodGet)
			return ea == nil && eb == nil && !a.Leader && !b.Leader && !a.NotifiedLeader && !b.NotifiedLeader
		}, 9*time.Second, 100*time.Millisecond)
		firstProxy.setMode(databaseForward)
		require.Eventually(t, func() bool {
			a, ea := first.status(http.MethodGet)
			b, eb := second.status(http.MethodGet)
			return ea == nil && eb == nil && a.Leader && a.NotifiedLeader && !b.Leader && !b.NotifiedLeader
		}, 15*time.Second, 100*time.Millisecond)
		assertOwner(t, first)
		assertHTTPAndWS(t, first)
	})
}

func TestSeparateProcessShutdownDuringSilentDatabase(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	}).InitBasic(t)
	cfg := th.Server.Config().Clone()
	cfg.ClusterSettings.Enable = new(true)
	cfg.ClusterSettings.ClusterName = new(model.NewId())
	cfg.ClusterSettings.BindAddress = new("127.0.0.1")
	cfg.ClusterSettings.AdvertiseAddress = new("127.0.0.1")
	cfg.ClusterSettings.ReadOnlyConfig = new(false)
	cfg.ClusterSettings.GossipPort = new(freePort(t))
	cfg.ServiceSettings.ListenAddress = new("127.0.0.1:0")
	cfg.ServiceSettings.EnableLocalMode = new(false)
	cfg.PluginSettings.Enable = new(false)
	cfg.LogSettings.EnableFile = new(false)
	cfg.SqlSettings.DataSourceReplicas = nil
	proxy, dsn := newClusterDatabaseProxy(t, *cfg.SqlSettings.DataSource)
	cfg.SqlSettings.DataSource = &dsn
	node := startClusterTestProcess(t, cfg)
	t.Cleanup(func() { proxy.setMode(databaseForward) })
	require.Eventually(t, func() bool {
		status, err := node.status(http.MethodGet)
		return err == nil && status.Leader && status.NotifiedLeader
	}, 5*time.Second, 100*time.Millisecond)
	proxy.setMode(databaseBlackhole)
	require.Eventually(t, func() bool {
		status, err := node.status(http.MethodGet)
		return err == nil && !status.Leader && !status.NotifiedLeader
	}, 9*time.Second, 100*time.Millisecond)
	proxy.mu.Lock()
	dropped := proxy.dropped
	proxy.mu.Unlock()
	require.Positive(t, dropped, "the outage must intercept real database traffic")
	start := time.Now()
	require.NoError(t, node.stdin.Close())
	select {
	case <-node.done:
		require.NoError(t, node.err)
		t.Logf("silent database process shutdown: pid=%d elapsed_ms=%d dropped_bytes=%d restored_before_exit=false", node.cmd.Process.Pid, time.Since(start).Milliseconds(), dropped)
	case <-time.After(10 * time.Second):
		t.Fatal("process shutdown waited for the silent database connection to recover")
	}
}
