package compliance_service

import (
	"context"
	"github.com/lib/pq"
	"net"
	"sync"
	"time"
)

// A report owns these connections, including PostgreSQL startup and pq's
// separate cancellation link. Expiry never closes the application's pool.
type complianceDialer struct {
	ctx         context.Context
	cancel      context.CancelFunc
	timeout     time.Duration
	mu          sync.Mutex
	closed      bool
	connections map[*complianceConn]struct{}
}

var _ pq.Dialer = (*complianceDialer)(nil)
var _ pq.DialerContext = (*complianceDialer)(nil)

func (d *complianceDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *complianceDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		return d.Dial(network, address)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.DialContext(ctx, network, address)
}

func (d *complianceDialer) DialContext(parent context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(d.ctx, cancel)
	defer stop()
	defer cancel()
	if d.ctx.Err() != nil {
		return nil, d.ctx.Err()
	}
	conn, err := (&net.Dialer{Timeout: d.timeout}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	wrapped := &complianceConn{Conn: conn, owner: d}
	d.mu.Lock()
	if d.closed || ctx.Err() != nil {
		d.mu.Unlock()
		_ = conn.Close()
		return nil, context.Canceled
	}
	if d.connections == nil {
		d.connections = make(map[*complianceConn]struct{})
	}
	d.connections[wrapped] = struct{}{}
	d.mu.Unlock()
	return wrapped, nil
}

func (d *complianceDialer) close() {
	d.mu.Lock()
	d.closed = true
	d.cancel()
	connections := make([]*complianceConn, 0, len(d.connections))
	for conn := range d.connections {
		connections = append(connections, conn)
	}
	d.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

type complianceConn struct {
	net.Conn
	owner *complianceDialer
}

func (c *complianceConn) Close() error {
	c.owner.mu.Lock()
	delete(c.owner.connections, c)
	c.owner.mu.Unlock()
	return c.Conn.Close()
}
