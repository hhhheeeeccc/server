package message_export

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
)

var messageExportReadSlots = make(chan struct{}, 4)

// A job reuses one private read-only primary session. No failed operation is
// replayed. The next explicit operation may reconnect (e.g. after the optional
// count failed), but cancellation of the job forbids opening any new socket.
type messageExportReader struct {
	ctx     context.Context
	dsn     string
	timeout time.Duration
	db      *sql.DB
	dialer  *messageExportDialer
	hasSlot bool
}

func newMessageExportReader(ctx context.Context, cfg model.SqlSettings) *messageExportReader {
	timeout := time.Duration(model.SafeDereference(cfg.QueryTimeout)) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &messageExportReader{ctx: ctx, dsn: model.SafeDereference(cfg.DataSource), timeout: timeout}
}

func messageExportReadDSN(dsn string, timeout time.Duration) (string, error) {
	dsn, err := messageExportSessionDSN(dsn, timeout)
	if err != nil {
		return "", errors.New("invalid message export read database configuration")
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", errors.New("invalid message export read database configuration")
		}
		q := u.Query()
		q.Set("application_name", "workspace_message_export_reader")
		q.Set("default_transaction_read_only", "on")
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	return dsn + " application_name=workspace_message_export_reader default_transaction_read_only=on", nil
}

func (r *messageExportReader) discard() {
	if r.dialer != nil {
		r.dialer.close()
		r.dialer = nil
	}
	if r.db != nil {
		_ = r.db.Close()
		r.db = nil
	}
}

func (r *messageExportReader) Close() {
	r.discard()
	if r.hasSlot {
		r.hasSlot = false
		<-messageExportReadSlots
	}
}

func (r *messageExportReader) query(parent context.Context, run func(context.Context, *sql.DB) error) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if err := parent.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	stopJob := context.AfterFunc(r.ctx, cancel)
	defer stopJob()
	defer cancel()
	if !r.hasSlot {
		select {
		case messageExportReadSlots <- struct{}{}:
			r.hasSlot = true
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.db == nil {
		dsn, err := messageExportReadDSN(r.dsn, r.timeout)
		if err != nil {
			return err
		}
		connector, err := pq.NewConnector(dsn)
		if err != nil {
			return errors.New("invalid message export read database configuration")
		}
		lifetime, stop := context.WithCancel(context.Background())
		r.dialer = &messageExportDialer{ctx: lifetime, cancel: stop, timeout: min(r.timeout, 5*time.Second)}
		connector.Dialer(r.dialer)
		r.db = sql.OpenDB(&messageExportConnector{connector: connector})
		r.db.SetMaxOpenConns(1)
		r.db.SetMaxIdleConns(1)
	}
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
