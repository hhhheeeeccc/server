package compliance_service

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

// Bound the extra connections independently of the application's pool. Keep
// each permit until its report closes, including time spent writing CSV.
var complianceReadSlots = make(chan struct{}, 4)

// A synchronous report reuses one private read-only backend across pages.
// Connection failures may fall back before the first read, within the same
// query budget. Failed page reads abort the archive rather than replaying it.
type complianceReader struct {
	sources []string
	timeout time.Duration
	db      *sql.DB
	dialer  *complianceDialer
	hasSlot bool
}

func newComplianceReader(settings model.SqlSettings) *complianceReader {
	timeout := time.Duration(model.SafeDereference(settings.QueryTimeout)) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	var sources []string
	if replicas := settings.DataSourceReplicas; len(replicas) > 0 {
		start := rand.IntN(len(replicas))
		for i := range replicas {
			if source := replicas[(start+i)%len(replicas)]; source != "" {
				sources = append(sources, source)
			}
		}
	}
	if primary := model.SafeDereference(settings.DataSource); primary != "" {
		sources = append(sources, primary)
	}
	return &complianceReader{sources: sources, timeout: timeout}
}

func complianceReadDSN(dsn string, timeout time.Duration) (string, error) {
	return complianceSessionDSN(dsn, "workspace_compliance_reader", timeout, true)
}

func complianceSessionDSN(dsn, name string, timeout time.Duration, readOnly bool) (string, error) {
	mode := "off"
	if readOnly {
		mode = "on"
	}
	settings := map[string]string{
		"fallback_application_name":        name,
		"default_transaction_read_only":    mode,
		"statement_timeout":                strconv.FormatInt(max(1, timeout.Milliseconds()), 10),
		"client_connection_check_interval": "100",
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", errors.New("invalid compliance read database configuration")
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

func (r *complianceReader) Close() {
	r.discard()
	if r.hasSlot {
		r.hasSlot = false
		<-complianceReadSlots
	}
}

func (r *complianceReader) discard() {
	if r.dialer != nil {
		r.dialer.close()
		r.dialer = nil
	}
	if r.db != nil {
		_ = r.db.Close()
		r.db = nil
	}
}

func (r *complianceReader) connect(source string) error {
	dsn, err := complianceReadDSN(source, r.timeout)
	if err != nil {
		return err
	}
	connector, err := pq.NewConnector(dsn)
	if err != nil {
		// pq parse errors can contain credentials from the original DSN.
		return errors.New("invalid compliance read database configuration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.dialer = &complianceDialer{ctx: ctx, cancel: cancel, timeout: min(r.timeout, 5*time.Second)}
	connector.Dialer(r.dialer)
	r.db = sql.OpenDB(connector)
	r.db.SetMaxOpenConns(1)
	r.db.SetMaxIdleConns(1)
	return nil
}

func (r *complianceReader) operation(ctx context.Context, run func(context.Context, *sql.DB) error) error {
	dialer := r.dialer
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { dialer.close(); close(finished) })
	err := run(ctx, r.db)
	if !stop() {
		<-finished
	}
	if ctx.Err() != nil {
		err = errors.Join(ctx.Err(), err)
	}
	if err != nil {
		r.discard()
	}
	return err
}

func (r *complianceReader) query(parent context.Context, run func(context.Context, *sql.DB) error) (resultErr error) {
	if err := parent.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	if !r.hasSlot {
		select {
		case complianceReadSlots <- struct{}{}:
			r.hasSlot = true
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() {
		if resultErr != nil {
			r.Close()
		}
	}()
	if r.db == nil {
		err := errors.New("compliance reader has no database source")
		for _, source := range r.sources {
			if err = r.connect(source); err == nil {
				err = r.operation(ctx, func(ctx context.Context, db *sql.DB) error { return db.PingContext(ctx) })
			}
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return errors.Join(ctx.Err(), err)
			}
		}
		if err != nil {
			return err
		}
	}
	return r.operation(ctx, run)
}

func (r *complianceReader) selectPosts(ctx context.Context, dest *[]*model.CompliancePost, query string, args ...any) error {
	return r.query(ctx, func(ctx context.Context, db *sql.DB) error {
		return sqlx.NewDb(db, "postgres").SelectContext(ctx, dest, sqlx.Rebind(sqlx.DOLLAR, query), args...)
	})
}
