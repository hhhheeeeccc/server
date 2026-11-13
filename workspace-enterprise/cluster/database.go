package cluster

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
)

// Coordination queries are small and must not keep workers alive indefinitely
// when a database (including pq's separate cancellation connection) goes silent.
// This is an idle I/O timeout, not an absolute duration limit for streaming SQL.
type coordinationConn struct {
	net.Conn
	timeout       time.Duration
	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
}

func (c *coordinationConn) deadline(caller time.Time) time.Time {
	idle := time.Now().Add(c.timeout)
	if !caller.IsZero() && caller.Before(idle) {
		return caller
	}
	return idle
}

func (c *coordinationConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	err := c.Conn.SetReadDeadline(c.deadline(c.readDeadline))
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c *coordinationConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	err := c.Conn.SetWriteDeadline(c.deadline(c.writeDeadline))
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

func (c *coordinationConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Conn.SetDeadline(c.deadline(t)); err != nil {
		return err
	}
	c.readDeadline, c.writeDeadline = t, t
	return nil
}

func (c *coordinationConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Conn.SetReadDeadline(c.deadline(t)); err != nil {
		return err
	}
	c.readDeadline = t
	return nil
}

func (c *coordinationConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Conn.SetWriteDeadline(c.deadline(t)); err != nil {
		return err
	}
	c.writeDeadline = t
	return nil
}

type coordinationDialer struct{ timeout time.Duration }

var _ pq.Dialer = coordinationDialer{}
var _ pq.DialerContext = coordinationDialer{}

func (d coordinationDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d coordinationDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		return d.Dial(network, address)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.DialContext(ctx, network, address)
}

func (d coordinationDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: d.timeout}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &coordinationConn{Conn: conn, timeout: d.timeout}, nil
}

func newCoordinationDatabase(dsn string) (*sql.DB, error) {
	connector, err := pq.NewConnector(dsn)
	if err != nil {
		// DSN parse errors may contain passwords or certificate key paths.
		return nil, errors.New("invalid cluster database configuration")
	}
	connector.Dialer(coordinationDialer{timeout: heartbeatInterval})
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	return db, nil
}

func (s *Service) coordinationDatabase() (*sql.DB, error) {
	// Native SqlStore captures SqlSettings at server initialization and does not
	// reconnect its primary on SaveConfig. Match that same initialized primary:
	// config synchronization must not move leases away from the active store.
	cfg := s.databaseConfig
	s.databaseMu.Lock()
	defer s.databaseMu.Unlock()
	if s.database != nil {
		return s.database, nil
	}
	dsn := model.SafeDereference(cfg.DataSource)
	if dsn == "" || model.SafeDereference(cfg.DriverName) != model.DatabaseDriverPostgres {
		return nil, errors.New("cluster requires a PostgreSQL primary database")
	}
	db, err := newCoordinationDatabase(dsn)
	if err != nil {
		return nil, err
	}
	db.SetConnMaxLifetime(time.Duration(model.SafeDereference(cfg.ConnMaxLifetimeMilliseconds)) * time.Millisecond)
	db.SetConnMaxIdleTime(time.Duration(model.SafeDereference(cfg.ConnMaxIdleTimeMilliseconds)) * time.Millisecond)
	s.database = db
	return db, nil
}

func (s *Service) currentCoordinationDatabase(db *sql.DB) bool {
	s.databaseMu.Lock()
	defer s.databaseMu.Unlock()
	return s.database == db
}

func (s *Service) closeCoordinationDatabase() {
	s.databaseMu.Lock()
	db := s.database
	s.database = nil
	s.databaseMu.Unlock()
	if db != nil {
		_ = db.Close()
	}
}
