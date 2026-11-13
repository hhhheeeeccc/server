package advisorylock

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/lib/pq"
)

// A dialer belongs to one nonreconnecting lease, including pq's secondary
// cancellation connections. It never sees a socket from the application pool.
type coordinationDialer struct {
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	closed      bool
	connections map[*coordinationConn]struct{}
}

var _ pq.Dialer = (*coordinationDialer)(nil)
var _ pq.DialerContext = (*coordinationDialer)(nil)

func newCoordinationDialer() *coordinationDialer {
	ctx, cancel := context.WithCancel(context.Background())
	return &coordinationDialer{ctx: ctx, cancel: cancel, connections: make(map[*coordinationConn]struct{})}
}

func (d *coordinationDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *coordinationDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		return d.Dial(network, address)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.DialContext(ctx, network, address)
}

func (d *coordinationDialer) DialContext(parent context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithCancel(parent)
	stop := watchCancellation(d.ctx, cancel)
	defer stop()
	defer cancel()
	if d.ctx.Err() != nil {
		return nil, context.Canceled
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	wrapped := &coordinationConn{Conn: conn, owner: d}
	d.mu.Lock()
	if d.closed || ctx.Err() != nil {
		d.mu.Unlock()
		_ = conn.Close()
		return nil, context.Canceled
	}
	d.connections[wrapped] = struct{}{}
	d.mu.Unlock()
	return wrapped, nil
}

func (d *coordinationDialer) close() {
	d.mu.Lock()
	d.closed = true
	d.cancel()
	connections := make([]*coordinationConn, 0, len(d.connections))
	for conn := range d.connections {
		connections = append(connections, conn)
	}
	d.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

type coordinationConn struct {
	net.Conn
	owner *coordinationDialer
}

func (c *coordinationConn) Close() error {
	c.owner.mu.Lock()
	delete(c.owner.connections, c)
	c.owner.mu.Unlock()
	return c.Conn.Close()
}

// Stop and join callbacks before returning their resources to another caller.
func watchCancellation(ctx context.Context, cancel func()) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); cancel() })
	return func() {
		if !stop() {
			<-done
		}
	}
}
