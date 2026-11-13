package message_export

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestMessageExportReaderSessionSafetyAndExplicitRecovery(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
	source, err := url.Parse(*th.App.Config().SqlSettings.DataSource)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", source.Hostname())
	password, _ := source.User.Password()
	keyword := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable application_name=ignored statement_timeout=1 default_transaction_read_only=off",
		source.Hostname(), source.Port(), source.User.Username(), password, source.Path[1:])
	for _, spec := range []struct{ name, dsn string }{{"url", source.String()}, {"keyword", keyword}} {
		t.Run(spec.name, func(t *testing.T) {
			settings := th.App.Config().SqlSettings
			settings.DataSource = &spec.dsn
			settings.DataSourceReplicas = []string{"invalid replica must never be used"}
			settings.DataSourceSearchReplicas = []string{"invalid search replica must never be used"}
			settings.QueryTimeout = model.NewPointer(7)
			reader := newMessageExportReader(context.Background(), settings)
			defer reader.Close()
			backend := func() int {
				var pid int
				var readOnly, name, timeout string
				var connectionCheck int
				err := reader.query(context.Background(), func(ctx context.Context, db *sql.DB) error {
					return db.QueryRowContext(ctx, `SELECT pg_backend_pid(),current_setting('transaction_read_only'),
					    current_setting('application_name'),current_setting('statement_timeout'),
					    (EXTRACT(EPOCH FROM current_setting('client_connection_check_interval')::interval)*1000)::int`).Scan(&pid, &readOnly, &name, &timeout, &connectionCheck)
				})
				require.NoError(t, err)
				require.Equal(t, "on", readOnly)
				require.Equal(t, "workspace_message_export_reader", name)
				require.Equal(t, "7s", timeout, "read timeout must use configured query budget, not the short owner-write budget")
				require.Equal(t, 100, connectionCheck)
				return pid
			}
			first := backend()
			require.Equal(t, first, backend(), "healthy pages must reuse their read-only backend")
			calls := 0
			err := reader.query(context.Background(), func(ctx context.Context, db *sql.DB) error {
				calls++
				_, err := db.ExecContext(ctx, `CREATE TABLE workspace_export_forbidden_write (id text)`)
				return err
			})
			var pgErr *pq.Error
			require.ErrorAs(t, err, &pgErr)
			require.Equal(t, pq.ErrorCode("25006"), pgErr.Code)
			require.Equal(t, 1, calls, "failed operations must never be replayed")
			require.Nil(t, reader.db)
			require.NotEqual(t, first, backend(), "a separate explicit read may reconnect after an optional count failed")
		})
	}
	t.Run("ssl-preserved", func(t *testing.T) {
		query := source.Query()
		query.Set("sslmode", "require")
		source.RawQuery = query.Encode()
		dsn := source.String()
		reader := newMessageExportReader(context.Background(), model.SqlSettings{DataSource: &dsn})
		defer reader.Close()
		require.ErrorContains(t, reader.query(context.Background(), func(ctx context.Context, db *sql.DB) error { return db.PingContext(ctx) }), "SSL is not enabled")
	})
}

func TestMessageExportReaderCutsSilentQueryAndCancellationLink(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
	for _, mode := range []string{"deadline", "caller-cancel", "job-cancel", "silent-cancel-link"} {
		t.Run(mode, func(t *testing.T) {
			proxy, dsn := newMessageExportSQLProxy(t, *th.App.Config().SqlSettings.DataSource)
			job, cancelJob := context.WithCancel(context.Background())
			defer cancelJob()
			reader := newMessageExportReader(job, model.SqlSettings{DataSource: &dsn})
			defer func() { proxy.Close(); reader.Close() }()
			require.NoError(t, reader.query(context.Background(), func(ctx context.Context, db *sql.DB) error { return db.PingContext(ctx) }))
			reader.timeout = 500 * time.Millisecond
			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			proxy.blocked.Store(true)
			finished := make(chan error, 1)
			go func() {
				queryParent := parent
				if mode == "silent-cancel-link" {
					queryParent = context.Background()
				}
				finished <- reader.query(queryParent, func(ctx context.Context, db *sql.DB) error {
					if mode == "silent-cancel-link" {
						statement, cancel := context.WithCancel(ctx)
						defer cancel()
						stop := context.AfterFunc(parent, cancel)
						defer stop()
						ctx = statement
					}
					_, err := db.ExecContext(ctx, `SELECT 1`)
					return err
				})
			}()
			require.Eventually(t, func() bool { return proxy.dropped.Load() > 0 }, 300*time.Millisecond, time.Millisecond)
			before := proxy.dropped.Load()
			if mode == "caller-cancel" || mode == "silent-cancel-link" {
				cancelParent()
			} else if mode == "job-cancel" {
				cancelJob()
			}
			if mode == "silent-cancel-link" {
				require.Eventually(t, func() bool { return proxy.dropped.Load() > before }, 300*time.Millisecond, time.Millisecond, "real pq cancellation packet must reach the silent proxy")
			}
			select {
			case err := <-finished:
				want := context.DeadlineExceeded
				if mode == "caller-cancel" || mode == "job-cancel" {
					want = context.Canceled
				}
				require.ErrorIs(t, err, want)
			case <-time.After(time.Second):
				t.Fatal("reader left a query or pq cancellation socket blocked")
			}
			require.Nil(t, reader.db)
			cancelJob()
			require.ErrorIs(t, reader.query(context.Background(), func(context.Context, *sql.DB) error { t.Error("read ran after job cancellation"); return nil }), context.Canceled)
			require.NoError(t, th.Store.GetInternalMasterDB().Ping())
		})
	}
}

func TestMessageExportReaderServerTimeoutAndConnectionLimit(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
	dsn := *th.App.Config().SqlSettings.DataSource
	ping := func(ctx context.Context, db *sql.DB) error { return db.PingContext(ctx) }
	reader := newMessageExportReader(context.Background(), model.SqlSettings{DataSource: &dsn})
	reader.timeout = 150 * time.Millisecond
	defer reader.Close()
	require.NoError(t, reader.query(context.Background(), ping))
	_, err := reader.db.ExecContext(context.Background(), `SELECT pg_sleep(2)`)
	var pgErr *pq.Error
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, pq.ErrorCode("57014"), pgErr.Code, "PostgreSQL must enforce the deadline even without a client context")
	reader.Close()
	var active []*messageExportReader
	defer func() {
		for _, r := range active {
			r.Close()
		}
	}()
	for range 4 {
		r := newMessageExportReader(context.Background(), model.SqlSettings{DataSource: &dsn})
		active = append(active, r)
		require.NoError(t, r.query(context.Background(), ping))
	}
	var count int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name='workspace_message_export_reader'`).Scan(&count))
	require.Equal(t, 4, count)
	blocked := newMessageExportReader(context.Background(), model.SqlSettings{DataSource: &dsn})
	defer blocked.Close()
	blocked.timeout = 100 * time.Millisecond
	require.ErrorIs(t, blocked.query(context.Background(), ping), context.DeadlineExceeded)
	require.Nil(t, blocked.db)
	job, cancel := context.WithCancel(context.Background())
	waiting := newMessageExportReader(job, model.SqlSettings{DataSource: &dsn})
	defer waiting.Close()
	done := make(chan error, 1)
	go func() { done <- waiting.query(context.Background(), ping) }()
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("slot wait ignored owning job cancellation")
	}
	require.Nil(t, waiting.db)
	active[0].Close()
	active[0].Close()
	blocked.timeout = time.Second
	require.NoError(t, blocked.query(context.Background(), ping), "closing a reader must release its slot exactly once")
}
