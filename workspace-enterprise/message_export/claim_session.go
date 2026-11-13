// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See the sibling Mattermost LICENSE.txt for license information.

package message_export

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

var errMessageExportSessionLost = errors.New("message export execution cannot reconnect after losing its database session")

// database/sql normally replaces a bad connection and retries the statement.
// That would lose a session advisory lock. Each execution gets one attempt to
// connect; a subsequent execution must acquire a fresh lock on its own backend.
type messageExportConnector struct {
	connector driver.Connector
	mu        sync.Mutex
	attempted bool
}

func (c *messageExportConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.attempted {
		return nil, errMessageExportSessionLost
	}
	c.attempted = true
	return c.connector.Connect(ctx)
}

func (c *messageExportConnector) Driver() driver.Driver { return c.connector.Driver() }

type messageExportExecution struct {
	db      *sql.DB
	ctx     context.Context
	timeout time.Duration
	dialer  *messageExportDialer
}

func newMessageExportExecution(ctx context.Context, dsn string, timeout time.Duration) (*messageExportExecution, error) {
	if timeout <= 0 || timeout > 3*time.Second {
		timeout = 3 * time.Second
	}
	dsn, err := messageExportSessionDSN(dsn, timeout)
	if err != nil {
		return nil, err
	}
	connector, err := pq.NewConnector(dsn)
	if err != nil {
		// A driver parse error can contain the password from the DSN.
		return nil, errors.New("invalid messageExport database configuration")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	dialer := &messageExportDialer{ctx: lifetime, cancel: cancel, timeout: min(timeout, 5*time.Second)}
	connector.Dialer(dialer)
	db := sql.OpenDB(&messageExportConnector{connector: connector})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return &messageExportExecution{db: db, ctx: ctx, timeout: timeout, dialer: dialer}, nil
}

func messageExportSessionDSN(dsn string, timeout time.Duration) (string, error) {
	settings := map[string]string{
		"application_name":                 "workspace_message_export_owner",
		"statement_timeout":                strconv.FormatInt(max(1, timeout.Milliseconds()), 10),
		"client_connection_check_interval": "100",
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", errors.New("invalid messageExport database configuration")
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

func (s *messageExportExecution) Close() {
	s.dialer.close()
	_ = s.db.Close()
}

func (s *messageExportExecution) queryContext() (context.Context, context.CancelFunc) {
	return s.queryContextFor(s.ctx)
}

// A query may reuse this backend only after completing before its deadline.
// Once a query expires, close every session-owned socket, including a silent
// startup or pq cancellation request. Never reconnect without a new lock.
func (s *messageExportExecution) queryContextFor(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, s.timeout)
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { s.dialer.close(); close(finished) })
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			if !stop() {
				<-finished
			}
			cancel()
		})
	}
}

func (s *messageExportExecution) exec(query string, args ...any) (sql.Result, error) {
	ctx, cancel := s.queryContext()
	defer cancel()
	return s.db.ExecContext(ctx, query, args...)
}

func (s *messageExportExecution) checkSession() error {
	ctx, cancel := s.queryContext()
	defer cancel()
	return s.db.PingContext(ctx)
}
