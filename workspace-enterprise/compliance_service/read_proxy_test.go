package compliance_service

import (
	"github.com/stretchr/testify/require"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type complianceSQLProxy struct {
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

func newComplianceSQLProxy(t *testing.T, dsn string) (*complianceSQLProxy, string) {
	t.Helper()
	address, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, address.Scheme)
	require.Contains(t, []string{"127.0.0.1", "localhost"}, address.Hostname(), "only isolated loopback PostgreSQL may be proxied")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &complianceSQLProxy{listener: listener, target: address.Host, links: make(map[net.Conn]net.Conn), finished: make(chan struct{})}
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

func (p *complianceSQLProxy) relay(client net.Conn) {
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
		_, _ = io.Copy(complianceSQLProxyWriter{p: p, target: upstream}, client)
		_ = upstream.Close()
		_ = client.Close()
		close(done)
	}()
	_, _ = io.Copy(complianceSQLProxyWriter{p: p, target: client}, upstream)
	_ = upstream.Close()
	_ = client.Close()
	<-done
}

func (p *complianceSQLProxy) Close() {
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

type complianceSQLProxyWriter struct {
	p      *complianceSQLProxy
	target io.Writer
}

func (w complianceSQLProxyWriter) Write(data []byte) (int, error) {
	if w.p.blocked.Load() {
		w.p.dropped.Add(int64(len(data)))
		return len(data), nil
	}
	return w.target.Write(data)
}
