package message_export

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/lib/pq"
)

// These sockets belong only to one messageExport execution. Closing an expired
// operation must never affect the application's PostgreSQL connection pool.
type messageExportDialer struct {
	ctx         context.Context
	cancel      context.CancelFunc
	timeout     time.Duration
	mu          sync.Mutex
	closed      bool
	connections map[*messageExportConn]struct{}
}

var _ pq.Dialer = (*messageExportDialer)(nil)
var _ pq.DialerContext = (*messageExportDialer)(nil)

func (d *messageExportDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *messageExportDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		return d.Dial(network, address)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.DialContext(ctx, network, address)
}

func (d *messageExportDialer) DialContext(parent context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(d.ctx, cancel)
	defer stop()
	defer cancel()
	if d.ctx.Err() != nil {
		return nil, errMessageExportSessionLost
	}
	conn, err := (&net.Dialer{Timeout: d.timeout}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	wrapped := &messageExportConn{Conn: conn, owner: d}
	d.mu.Lock()
	if d.closed || ctx.Err() != nil {
		d.mu.Unlock()
		_ = conn.Close()
		return nil, errMessageExportSessionLost
	}
	if d.connections == nil {
		d.connections = make(map[*messageExportConn]struct{})
	}
	d.connections[wrapped] = struct{}{}
	d.mu.Unlock()
	return wrapped, nil
}

func (d *messageExportDialer) close() {
	d.mu.Lock()
	d.closed = true
	d.cancel()
	connections := make([]*messageExportConn, 0, len(d.connections))
	for conn := range d.connections {
		connections = append(connections, conn)
	}
	d.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

type messageExportConn struct {
	net.Conn
	owner *messageExportDialer
}

func (c *messageExportConn) Close() error {
	c.owner.mu.Lock()
	delete(c.owner.connections, c)
	c.owner.mu.Unlock()
	return c.Conn.Close()
}
