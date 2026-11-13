package content_extraction

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// Each session has one synchronous owner. The worker's batch reader and its
// cancellation monitor use independent sessions, never the application's pool.
// Expiry closes every socket, including pq's startup and separate cancel link.
type databaseSession struct {
	dsn, name string
	timeout   time.Duration
	db        *sql.DB
	dialer    *extractionDialer
	readOnly  bool
	singleUse bool
	opened    bool
}

func newDatabaseSession(dsn, name string, timeout time.Duration) *databaseSession {
	return &databaseSession{dsn: dsn, name: name, timeout: timeout, readOnly: true}
}

func (s *Service) database(role string) *databaseSession {
	timeout := time.Duration(*s.app.Config().SqlSettings.QueryTimeout) * time.Second
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	if role != "reader" {
		timeout = min(timeout, 3*time.Second)
	}
	return newDatabaseSession(*s.app.Config().SqlSettings.DataSource,
		"workspace_content_extraction_"+role, timeout)
}

func sessionDSN(dsn, name string, timeout time.Duration, readOnly bool) (string, error) {
	mode := "off"
	if readOnly {
		mode = "on"
	}
	settings := map[string]string{
		"application_name": name, "default_transaction_read_only": mode,
		"statement_timeout":                strconv.FormatInt(max(1, timeout.Milliseconds()), 10),
		"client_connection_check_interval": "100",
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", errors.New("invalid content extraction database configuration")
		}
		query := u.Query()
		for key, value := range settings {
			query.Set(key, value)
		}
		u.RawQuery = query.Encode()
		return u.String(), nil
	}
	for key, value := range settings {
		dsn += " " + key + "=" + value
	}
	return dsn, nil
}

func (s *databaseSession) open() error {
	if s.db != nil {
		return nil
	}
	if s.singleUse && s.opened {
		return errOwnershipLost
	}
	dsn, err := sessionDSN(s.dsn, s.name, s.timeout, s.readOnly)
	if err != nil {
		return err
	}
	connector, err := pq.NewConnector(dsn)
	if err != nil {
		// Connector parse errors may include the original connection string.
		return errors.New("invalid content extraction database configuration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.dialer = &extractionDialer{ctx: ctx, cancel: cancel, timeout: min(s.timeout, 5*time.Second)}
	connector.Dialer(s.dialer)
	var source driver.Connector = connector
	if s.singleUse {
		source = &extractionConnector{Connector: source}
	}
	s.db = sql.OpenDB(source)
	s.opened = true
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	return nil
}

// An advisory-lock owner must never transparently reconnect after ErrBadConn.
// A replacement backend would have no lock despite the same Go pool object.
type extractionConnector struct {
	driver.Connector
	mu        sync.Mutex
	connected bool
}

func (c *extractionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connected {
		return nil, errOwnershipLost
	}
	c.connected = true
	return c.Connector.Connect(ctx)
}

func (s *databaseSession) Close() {
	if s.dialer != nil {
		s.dialer.close()
		s.dialer = nil
	}
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

func (s *databaseSession) query(parent context.Context, run func(context.Context, *sql.DB) error) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if err := s.open(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, s.timeout)
	dialer := s.dialer
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { dialer.close(); close(finished) })
	err := run(ctx, s.db)
	if !stop() {
		<-finished
	}
	if ctx.Err() != nil {
		err = errors.Join(ctx.Err(), err)
	}
	cancel()
	if err != nil {
		s.Close()
	}
	return err
}

type extractionDialer struct {
	ctx         context.Context
	cancel      context.CancelFunc
	timeout     time.Duration
	mu          sync.Mutex
	closed      bool
	connections map[*extractionConn]struct{}
}

var _ pq.Dialer = (*extractionDialer)(nil)
var _ pq.DialerContext = (*extractionDialer)(nil)

func (d *extractionDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *extractionDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		return d.Dial(network, address)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.DialContext(ctx, network, address)
}

func (d *extractionDialer) DialContext(parent context.Context, network, address string) (net.Conn, error) {
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
	wrapped := &extractionConn{Conn: conn, owner: d}
	d.mu.Lock()
	if d.closed || ctx.Err() != nil {
		d.mu.Unlock()
		_ = conn.Close()
		return nil, context.Canceled
	}
	if d.connections == nil {
		d.connections = make(map[*extractionConn]struct{})
	}
	d.connections[wrapped] = struct{}{}
	d.mu.Unlock()
	return wrapped, nil
}

func (d *extractionDialer) close() {
	d.mu.Lock()
	d.closed = true
	d.cancel()
	connections := make([]*extractionConn, 0, len(d.connections))
	for conn := range d.connections {
		connections = append(connections, conn)
	}
	d.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

type extractionConn struct {
	net.Conn
	owner *extractionDialer
}

func (c *extractionConn) Close() error {
	c.owner.mu.Lock()
	delete(c.owner.connections, c)
	c.owner.mu.Unlock()
	return c.Conn.Close()
}
