package elasticsearch_indexing

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/lib/pq"
)

// Context cancellation alone does not interrupt pq's startup handshake or a
// silent connection used for its separate cancellation request. Every socket
// belongs to this one execution and is closed when its SQL operation expires.
type indexingDialer struct {
	timeout     time.Duration
	mu          sync.Mutex
	closed      bool
	connections map[*indexingConn]struct{}
}

var _ pq.Dialer = (*indexingDialer)(nil)
var _ pq.DialerContext = (*indexingDialer)(nil)

func (d *indexingDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *indexingDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		return d.Dial(network, address)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.DialContext(ctx, network, address)
}

func (d *indexingDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return nil, errIndexingOwnershipLost
	}
	conn, err := (&net.Dialer{Timeout: d.timeout}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	wrapped := &indexingConn{Conn: conn, owner: d}
	d.mu.Lock()
	if d.closed || ctx.Err() != nil {
		d.mu.Unlock()
		_ = conn.Close()
		return nil, errIndexingOwnershipLost
	}
	if d.connections == nil {
		d.connections = make(map[*indexingConn]struct{})
	}
	d.connections[wrapped] = struct{}{}
	d.mu.Unlock()
	return wrapped, nil
}

func (d *indexingDialer) close() {
	d.mu.Lock()
	d.closed = true
	connections := make([]*indexingConn, 0, len(d.connections))
	for conn := range d.connections {
		connections = append(connections, conn)
	}
	d.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

type indexingConn struct {
	net.Conn
	owner *indexingDialer
}

func (c *indexingConn) Close() error {
	c.owner.mu.Lock()
	delete(c.owner.connections, c)
	c.owner.mu.Unlock()
	return c.Conn.Close()
}
