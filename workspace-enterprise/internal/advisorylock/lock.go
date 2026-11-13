package advisorylock

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// Locker bounds coordination connections for this service, including callers
// waiting for a lock owned on another node. New defaults to a single connection.
type Locker struct {
	dsn  string
	gate chan struct{}
}

func New(dsn string) *Locker { return NewWithCapacity(dsn, 1) }

func NewWithCapacity(dsn string, capacity int) *Locker {
	return &Locker{dsn: dsn, gate: make(chan struct{}, max(1, capacity))}
}

// Lease keeps all locks for one operation on the same nonreconnecting
// transaction. Additional keys do not occupy another gate slot or connection.
type Lease struct {
	tx      *sql.Tx
	dialer  *coordinationDialer
	release func()
}

func (l *Lease) Lock(ctx context.Context, key int64) error {
	stop := watchCancellation(ctx, l.dialer.close)
	defer stop()
	_, err := l.tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", key)
	return errors.Join(err, ctx.Err())
}

// Query consumes identity rows under the same transaction's row/advisory
// locks. Closing rows and joining cancellation remain owned by the lease.
func (l *Lease) Query(ctx context.Context, query string, args []any, read func(*sql.Rows) error) error {
	stop := watchCancellation(ctx, l.dialer.close)
	defer stop()
	rows, err := l.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return errors.Join(err, ctx.Err())
	}
	defer rows.Close()
	err = read(rows)
	return errors.Join(err, rows.Err(), rows.Close(), ctx.Err())
}

func (l *Lease) Release() { l.release() }

// A transaction cannot reconnect and silently lose its advisory lock. Its
// private connection never consumes the application pool needed by native
// user/group calls. A per-service gate limits owners plus database waiters to
// the configured number of private connections; other callers wait in Go and
// honor cancellation.
func (l *Locker) Acquire(ctx context.Context, key int64) (func(), error) {
	lease, err := l.AcquireLease(ctx, key)
	if err != nil {
		return nil, err
	}
	return lease.Release, nil
}

func (l *Locker) AcquireLease(ctx context.Context, key int64) (*Lease, error) {
	select {
	case l.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	releaseGate := func() { <-l.gate }
	connector, err := pq.NewConnector(connectionDSN(l.dsn))
	if err != nil {
		releaseGate()
		// PostgreSQL parser errors may contain the configured DSN/password.
		return nil, errors.New("invalid coordination database configuration")
	}
	dialer := newCoordinationDialer()
	connector.Dialer(dialer)
	// pq installs transaction cancellation only after BEGIN has returned.
	// Closing our private sockets also interrupts a silent BEGIN or cancel
	// handshake, without imposing a deadline on a legitimate lock waiter.
	stop := watchCancellation(ctx, dialer.close)
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		dialer.close()
		stop()
		_ = db.Close()
		releaseGate()
		return nil, errors.Join(err, ctx.Err())
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			// Rollback normally confirms release at PostgreSQL. A silent peer
			// must not retain a local permit indefinitely during cleanup.
			timer := time.AfterFunc(time.Second, dialer.close)
			_ = tx.Rollback()
			timer.Stop()
			dialer.close()
			stop()
			_ = db.Close()
			releaseGate()
		})
	}
	lease := &Lease{tx: tx, dialer: dialer, release: release}
	if err := lease.Lock(ctx, key); err != nil {
		release()
		return nil, err
	}
	return lease, nil
}

func connectionDSN(dsn string) string {
	// pq bounds startup as well as TCP with connect_timeout. Retain a shorter
	// configured URL timeout; never create an unbounded new startup connection.
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if parsed, err := url.Parse(dsn); err == nil {
			query := parsed.Query()
			seconds, _ := strconv.Atoi(query.Get("connect_timeout"))
			if seconds <= 0 || seconds > 5 {
				query.Set("connect_timeout", "5")
			}
			parsed.RawQuery = query.Encode()
			return parsed.String()
		}
	}
	return dsn + " connect_timeout=5"
}
