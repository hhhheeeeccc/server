package compliance_service

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestComplianceReaderBoundsSilentStartup(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			accepted, finished := make(chan net.Conn, 1), make(chan struct{})
			go func() {
				defer close(finished)
				conn, err := listener.Accept()
				if err == nil {
					accepted <- conn
					_, _ = io.Copy(io.Discard, conn)
					_ = conn.Close()
				}
			}()
			reader := &complianceReader{sources: []string{"postgres://probe@" + listener.Addr().String() + "/probe?sslmode=disable&connect_timeout=0"}, timeout: 250 * time.Millisecond}
			defer reader.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- reader.query(ctx, func(context.Context, *sql.DB) error { return nil }) }()
			select {
			case conn := <-accepted:
				defer conn.Close()
			case <-time.After(time.Second):
				t.Fatal("startup did not reach the silent TCP fixture")
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				want := context.DeadlineExceeded
				if mode == "cancel" {
					want = context.Canceled
				}
				require.ErrorIs(t, err, want)
			case <-time.After(time.Second):
				t.Fatal("silent PostgreSQL startup exceeded its context budget")
			}
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("startup socket remained open after the read returned")
			}
		})
	}
}

func TestComplianceReaderSessionSafety(t *testing.T) {
	th, _ := dailyScheduleFixture(t)
	source := *th.App.Config().SqlSettings.DataSource
	u, err := url.Parse(source)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", u.Hostname())
	password, _ := u.User.Password()
	query := u.Query()
	query.Set("application_name", "workspace_compliance_explicit_name")
	u.RawQuery = query.Encode()
	keyword := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable application_name=workspace_compliance_explicit_name", u.Hostname(), u.Port(), u.User.Username(), password, u.Path[1:])
	for _, spec := range []struct{ name, dsn string }{{"url", u.String()}, {"keyword", keyword}} {
		t.Run(spec.name, func(t *testing.T) {
			reader := &complianceReader{sources: []string{spec.dsn}, timeout: 2 * time.Second}
			defer reader.Close()
			backend := func() int {
				var pid int
				err := reader.query(context.Background(), func(ctx context.Context, db *sql.DB) error {
					var readOnly, name, timeout string
					var connectionCheck int
					err := db.QueryRowContext(ctx, `SELECT pg_backend_pid(), current_setting('transaction_read_only'),
						current_setting('application_name'), current_setting('statement_timeout'),
						(EXTRACT(EPOCH FROM current_setting('client_connection_check_interval')::interval)*1000)::int`).Scan(&pid, &readOnly, &name, &timeout, &connectionCheck)
					require.Equal(t, "on", readOnly)
					require.Equal(t, "workspace_compliance_explicit_name", name)
					require.Equal(t, "2s", timeout)
					require.Equal(t, 100, connectionCheck)
					return err
				})
				require.NoError(t, err)
				return pid
			}
			first := backend()
			require.Equal(t, first, backend(), "healthy pages must reuse their backend")
			err := reader.query(context.Background(), func(ctx context.Context, db *sql.DB) error {
				_, err := db.ExecContext(ctx, `CREATE TABLE workspace_compliance_forbidden_write (id text)`)
				return err
			})
			var pgErr *pq.Error
			require.ErrorAs(t, err, &pgErr)
			require.Equal(t, pq.ErrorCode("25006"), pgErr.Code)
			require.Nil(t, reader.db)
			reader.Close()
		})
	}
	t.Run("replica-and-primary-fallback", func(t *testing.T) {
		// A distinct application_name proves which configured source was used.
		settings := th.App.Config().SqlSettings
		settings.DataSourceReplicas = []string{u.String()}
		settings.DataSourceSearchReplicas = []string{"invalid search source must never be used"}
		settings.DataSource = model.NewPointer("invalid primary must never be used")
		reader := newComplianceReader(settings)
		readName := func() string {
			var name string
			require.NoError(t, reader.query(context.Background(), func(ctx context.Context, db *sql.DB) error {
				return db.QueryRowContext(ctx, `SELECT current_setting('application_name')`).Scan(&name)
			}))
			return name
		}
		require.Equal(t, "workspace_compliance_explicit_name", readName())
		reader.Close()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		dead := listener.Addr().String()
		require.NoError(t, listener.Close())
		settings.DataSourceReplicas = []string{"postgres://probe@" + dead + "/probe?sslmode=disable"}
		settings.DataSource = model.NewPointer(u.String())
		reader = newComplianceReader(settings)
		defer reader.Close()
		require.Equal(t, "workspace_compliance_explicit_name", readName(), "an unavailable replica must fall back before the first read")
	})
	t.Run("ssl-preserved", func(t *testing.T) {
		query.Set("sslmode", "require")
		u.RawQuery = query.Encode()
		reader := &complianceReader{sources: []string{u.String()}, timeout: time.Second}
		defer reader.Close()
		require.ErrorContains(t, reader.query(context.Background(), func(context.Context, *sql.DB) error { return nil }), "SSL is not enabled")
	})
	t.Run("invalid-configuration-redacted", func(t *testing.T) {
		for _, source := range []string{"postgres://probe:private-test-secret@bad%escape/db", "password='private-test-secret"} {
			reader := &complianceReader{sources: []string{source}, timeout: time.Second}
			defer reader.Close()
			err := reader.query(context.Background(), func(context.Context, *sql.DB) error { return nil })
			require.EqualError(t, err, "invalid compliance read database configuration")
		}
	})
}

func TestComplianceReaderCutsSilentQueryAndCancellationLink(t *testing.T) {
	th, _ := dailyScheduleFixture(t)
	for _, mode := range []string{"deadline", "cancel", "silent-cancel-link"} {
		t.Run(mode, func(t *testing.T) {
			proxy, source := newComplianceSQLProxy(t, *th.App.Config().SqlSettings.DataSource)
			reader := &complianceReader{sources: []string{source}, timeout: 5 * time.Second}
			defer func() { proxy.Close(); reader.Close() }()
			require.NoError(t, reader.query(context.Background(), func(context.Context, *sql.DB) error { return nil }))
			reader.timeout = 500 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			proxy.blocked.Store(true)
			done := make(chan error, 1)
			if mode == "silent-cancel-link" {
				// Cancel the driver before the enclosing SQL budget. Its distinct
				// cancel connection must actually reach the silent TCP proxy.
				go func() {
					done <- reader.query(context.Background(), func(queryCtx context.Context, db *sql.DB) error {
						statementCtx, stop := context.WithCancel(queryCtx)
						defer stop()
						unhook := context.AfterFunc(ctx, stop)
						defer unhook()
						_, err := db.ExecContext(statementCtx, `SELECT 1`)
						return err
					})
				}()
			} else {
				go func() {
					_, _, err := reader.batch(ctx, &model.Compliance{StartAt: 1, EndAt: model.GetMillis()}, model.ComplianceExportCursor{}, 20)
					done <- err
				}()
			}
			require.Eventually(t, func() bool { return proxy.dropped.Load() > 0 }, 300*time.Millisecond, time.Millisecond)
			beforeCancel := proxy.dropped.Load()
			if mode != "deadline" {
				cancel()
			}
			if mode == "silent-cancel-link" {
				require.Eventually(t, func() bool { return proxy.dropped.Load() > beforeCancel }, 300*time.Millisecond, time.Millisecond,
					"pq's cancellation packet must reach the silent proxy")
			}
			select {
			case err := <-done:
				want := context.DeadlineExceeded
				if mode == "cancel" {
					want = context.Canceled
				}
				require.ErrorIs(t, err, want)
			case <-time.After(time.Second):
				t.Fatal("private reader left a silent query or cancellation socket waiting")
			}
			require.Nil(t, reader.db)
			require.NoError(t, th.Store.GetInternalMasterDB().Ping())
		})
	}
}

func TestComplianceReaderServerEnforcesStatementTimeout(t *testing.T) {
	th, _ := dailyScheduleFixture(t)
	reader := newComplianceReader(th.App.Config().SqlSettings)
	reader.timeout = 150 * time.Millisecond
	defer reader.Close()
	require.NoError(t, reader.query(context.Background(), func(context.Context, *sql.DB) error { return nil }))
	// Bypass the client context to prove the session's server-side timeout.
	_, err := reader.db.ExecContext(context.Background(), `SELECT pg_sleep(2)`)
	var pgErr *pq.Error
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, pq.ErrorCode("57014"), pgErr.Code)
}

func TestComplianceReaderLimitsPrivateConnections(t *testing.T) {
	th, _ := dailyScheduleFixture(t)
	source, err := url.Parse(*th.App.Config().SqlSettings.DataSource)
	require.NoError(t, err)
	q := source.Query()
	q.Set("application_name", "workspace_compliance_limit_test")
	source.RawQuery = q.Encode()
	query := func(ctx context.Context, db *sql.DB) error { return db.PingContext(ctx) }
	var active []*complianceReader
	defer func() {
		for _, reader := range active {
			reader.Close()
		}
	}()
	for range 4 {
		reader := &complianceReader{sources: []string{source.String()}, timeout: 2 * time.Second}
		active = append(active, reader)
		require.NoError(t, reader.query(context.Background(), query))
	}
	var count int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE application_name='workspace_compliance_limit_test'`).Scan(&count))
	require.Equal(t, 4, count)
	blocked := &complianceReader{sources: []string{source.String()}, timeout: 100 * time.Millisecond}
	defer blocked.Close()
	require.ErrorIs(t, blocked.query(context.Background(), query), context.DeadlineExceeded)
	require.Nil(t, blocked.db, "a waiting report must not open a fifth private connection")
	active[0].Close()
	active[0].Close() // release must be idempotent
	blocked.timeout = 2 * time.Second
	require.NoError(t, blocked.query(context.Background(), query), "closing a report must let another report proceed")
}
