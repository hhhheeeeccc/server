package elasticsearch_indexing

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
)

// A reader is used synchronously by one job. Its sockets are independent of
// both the application pool and the advisory-lock session. Canceling a read
// therefore leaves the owner able to flush acknowledged work and save pending.
type indexingReader struct {
	ctx     context.Context
	sources []string
	timeout time.Duration
	next    int
	db      *sql.DB
	dialer  *indexingDialer
	closed  bool
}

func indexingReadSources(settings model.SqlSettings) []string {
	var sources []string
	appendGroup := func(group []string) {
		if len(group) == 0 {
			return
		}
		start := rand.IntN(len(group))
		for i := range group {
			dsn := group[(start+i)%len(group)]
			found := false
			for _, previous := range sources {
				found = found || previous == dsn
			}
			if dsn != "" && !found {
				sources = append(sources, dsn)
			}
		}
	}
	appendGroup(settings.DataSourceSearchReplicas)
	appendGroup(settings.DataSourceReplicas)
	appendGroup([]string{model.SafeDereference(settings.DataSource)})
	return sources
}

func newIndexingReader(ctx context.Context, sources []string, timeout time.Duration) *indexingReader {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &indexingReader{ctx: ctx, sources: sources, timeout: timeout}
}

func indexingSessionDSN(dsn, name string, readOnly bool, timeout time.Duration) (string, error) {
	settings := map[string]string{
		"application_name": name, "statement_timeout": strconv.FormatInt(max(1, timeout.Milliseconds()), 10),
		// Closing TCP alone may leave a server blocked on a relation lock.
		// Supported PostgreSQL versions (15+) check the lost client during work.
		"client_connection_check_interval": "100",
	}
	if readOnly {
		settings["default_transaction_read_only"] = "on"
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", errors.New("invalid indexing read database configuration")
		}
		q := u.Query()
		for key, value := range settings {
			q.Set(key, value)
		}
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	for key, value := range settings {
		dsn += " " + key + "=" + value
	}
	return dsn, nil
}

func (r *indexingReader) open() error {
	if r.closed {
		return errors.New("indexing reader is closed")
	}
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if r.db != nil {
		return nil
	}
	if len(r.sources) == 0 {
		return errors.New("indexing reader has no database source")
	}
	dsn, err := indexingSessionDSN(r.sources[r.next%len(r.sources)], "workspace_indexing_reader", true, r.timeout)
	if err != nil {
		r.next++
		return err
	}
	connector, err := pq.NewConnector(dsn)
	if err != nil {
		r.next++
		return errors.New("invalid indexing read database configuration")
	}
	r.dialer = &indexingDialer{timeout: min(r.timeout, 5*time.Second)}
	connector.Dialer(r.dialer)
	r.db = sql.OpenDB(&indexingConnector{connector: connector})
	r.db.SetMaxOpenConns(1)
	r.db.SetMaxIdleConns(1)
	return nil
}

func (r *indexingReader) discard() {
	if r.dialer != nil {
		r.dialer.close()
		r.dialer = nil
	}
	if r.db != nil {
		_ = r.db.Close()
		r.db = nil
	}
}

func (r *indexingReader) Close() { r.closed = true; r.discard() }

func (r *indexingReader) query(run func(context.Context, *sqlx.DB) error) error {
	if err := r.open(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
	dialer := r.dialer
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { dialer.close(); close(finished) })
	err := run(ctx, sqlx.NewDb(r.db, "postgres"))
	// Disarm before normal cancellation, and join an expiry callback before
	// replacing a failed connection. A late callback must not close a new reader.
	if !stop() {
		<-finished
	}
	if err == nil {
		err = ctx.Err()
	}
	cancel()
	if err != nil {
		r.discard()
		r.next++
	}
	return err
}

func (r *indexingReader) selectRows(dest any, query string, args ...any) error {
	return r.query(func(ctx context.Context, db *sqlx.DB) error { return db.SelectContext(ctx, dest, query, args...) })
}

func (r *indexingReader) get(dest any, query string, args ...any) error {
	return r.query(func(ctx context.Context, db *sqlx.DB) error { return db.GetContext(ctx, dest, query, args...) })
}
