package message_export

import (
	"context"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

type messageExportSQLProxy struct {
	listener  net.Listener
	target    string
	blocked   atomic.Bool
	dropped   atomic.Int64
	mu        sync.Mutex
	links     map[net.Conn]net.Conn
	closed    bool
	workers   sync.WaitGroup
	finished  chan struct{}
	closeOnce sync.Once
}

func newMessageExportSQLProxy(t *testing.T, dsn string) (*messageExportSQLProxy, string) {
	t.Helper()
	address, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, address.Scheme)
	require.Contains(t, []string{"127.0.0.1", "localhost"}, address.Hostname(), "only isolated loopback PostgreSQL may be proxied")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &messageExportSQLProxy{listener: listener, target: address.Host, links: make(map[net.Conn]net.Conn), finished: make(chan struct{})}
	go func() {
		defer close(p.finished)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			p.workers.Add(1)
			go p.relay(client)
		}
	}()
	t.Cleanup(p.Close)
	address.Host = listener.Addr().String()
	query := address.Query()
	query.Set("connect_timeout", "0")
	address.RawQuery = query.Encode()
	return p, address.String()
}

func (p *messageExportSQLProxy) relay(client net.Conn) {
	defer p.workers.Done()
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.links[client] = upstream
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.links, client); p.mu.Unlock() }()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(messageExportSQLProxyWriter{p: p, target: upstream}, client)
		_ = upstream.Close()
		_ = client.Close()
		close(done)
	}()
	_, _ = io.Copy(messageExportSQLProxyWriter{p: p, target: client}, upstream)
	_ = upstream.Close()
	_ = client.Close()
	<-done
}

func (p *messageExportSQLProxy) Close() {
	p.closeOnce.Do(func() {
		_ = p.listener.Close()
		<-p.finished
		p.mu.Lock()
		p.closed = true
		for client, upstream := range p.links {
			_ = client.Close()
			_ = upstream.Close()
		}
		p.mu.Unlock()
		p.workers.Wait()
	})
}

type messageExportSQLProxyWriter struct {
	p      *messageExportSQLProxy
	target io.Writer
}

func (w messageExportSQLProxyWriter) Write(data []byte) (int, error) {
	if w.p.blocked.Load() {
		w.p.dropped.Add(int64(len(data)))
		return len(data), nil
	}
	return w.target.Write(data)
}

func TestMessageExportSessionBoundsSilentQueryAndReleasesLock(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	for _, mode := range []string{"deadline", "cancel", "cancel-socket"} {
		t.Run(mode, func(t *testing.T) {
			proxy, dsn := newMessageExportSQLProxy(t, *th.App.Config().SqlSettings.DataSource)
			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			session, err := newMessageExportExecution(parent, dsn, 2*time.Second)
			require.NoError(t, err)
			defer func() { proxy.Close(); session.Close() }()
			const key = int64(71279391113)
			_, err = session.exec(`SELECT pg_advisory_lock($1::bigint)`, key)
			require.NoError(t, err)
			session.timeout = 150 * time.Millisecond
			if mode == "cancel-socket" {
				session.timeout = time.Second
			}
			query, stop := session.queryContext()
			defer stop()
			statement, cancel := context.WithCancel(query)
			defer cancel()
			proxy.blocked.Store(true)
			finished := make(chan error, 1)
			go func() { _, err := session.db.ExecContext(statement, `SELECT 1`); finished <- err }()
			require.Eventually(t, func() bool { return proxy.dropped.Load() > 0 }, 100*time.Millisecond, time.Millisecond)
			if mode == "cancel" {
				cancelParent()
			} else if mode == "cancel-socket" {
				before := proxy.dropped.Load()
				cancel()
				require.Eventually(t, func() bool { return proxy.dropped.Load() > before }, 300*time.Millisecond, time.Millisecond,
					"the real pq cancellation socket must reach the silent proxy")
			}
			select {
			case err := <-finished:
				require.Error(t, err)
			case <-time.After(1500 * time.Millisecond):
				t.Fatal("SQL or its cancellation socket survived the enclosing session budget")
			}
			stop()
			_, err = session.exec(`SELECT 1`)
			require.Error(t, err, "an expired owner must not reconnect")
			conn, err := th.Store.GetInternalMasterDB().Conn(context.Background())
			require.NoError(t, err)
			defer conn.Close()
			var acquired bool
			require.Eventually(t, func() bool {
				err := conn.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock($1::bigint)`, key).Scan(&acquired)
				return err == nil && acquired
			}, time.Second, 10*time.Millisecond, "deadline must release the old backend's real advisory lock")
			_, err = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1::bigint)`, key)
			require.NoError(t, err)
			require.NoError(t, th.Store.GetInternalMasterDB().Ping(), "the application's pool must remain healthy")
		})
	}
}

func TestMessageExportHealthySessionReusesBackendAndCapsTimeout(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	session, err := newMessageExportExecution(context.Background(), *th.App.Config().SqlSettings.DataSource, time.Minute)
	require.NoError(t, err)
	defer session.Close()
	first := 0
	for range 3 {
		query, stop := session.queryContext()
		var pid int
		var timeout string
		err := session.db.QueryRowContext(query, `SELECT pg_backend_pid(),current_setting('statement_timeout')`).Scan(&pid, &timeout)
		stop()
		stop()
		require.NoError(t, err)
		require.Equal(t, "3s", timeout)
		if first == 0 {
			first = pid
		}
		require.Equal(t, first, pid)
	}
}
