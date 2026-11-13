package content_extraction

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDatabaseSessionBoundsSilentStartup(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			accepted := make(chan net.Conn, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err == nil {
					accepted <- conn
					_, _ = io.Copy(io.Discard, conn)
					_ = conn.Close()
				}
			}()
			session := newDatabaseSession("postgres://probe@"+listener.Addr().String()+"/probe?sslmode=disable&connect_timeout=0", "workspace_extraction_startup_test", 150*time.Millisecond)
			defer session.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				finished <- session.query(ctx, func(ctx context.Context, db *sql.DB) error {
					var one int
					return db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
				})
			}()
			var conn net.Conn
			select {
			case conn = <-accepted:
			case <-time.After(time.Second):
				t.Fatal("no connection reached the silent startup fixture")
			}
			defer conn.Close()
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-finished:
				want := context.DeadlineExceeded
				if mode == "cancel" {
					want = context.Canceled
				}
				require.ErrorIs(t, err, want)
			case <-time.After(time.Second):
				t.Fatal("the session did not close its silent startup socket")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("a fixture socket remained open after the SQL call returned")
			}
		})
	}
}

func TestDatabaseSessionReusesReadOnlyConnection(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_POSTGRESQL_DSN")
	if dsn == "" {
		t.Skip("requires the isolated PostgreSQL audit runner")
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", u.Hostname())
	password, _ := u.User.Password()
	keyword := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable", u.Hostname(), u.Port(), u.User.Username(), password, u.Path[1:])
	for _, source := range []struct{ name, dsn string }{{"url", dsn}, {"keyword", keyword}} {
		t.Run(source.name, func(t *testing.T) {
			session := newDatabaseSession(source.dsn, "workspace_extraction_reuse_test", 2*time.Second)
			defer session.Close()
			backend := func() int {
				var pid int
				err := session.query(context.Background(), func(ctx context.Context, db *sql.DB) error {
					var readOnly string
					return db.QueryRowContext(ctx, "SELECT pg_backend_pid(), current_setting('default_transaction_read_only')").Scan(&pid, &readOnly)
				})
				require.NoError(t, err)
				return pid
			}
			first := backend()
			require.Equal(t, first, backend(), "healthy polling must reuse its connection")
			err := session.query(context.Background(), func(ctx context.Context, db *sql.DB) error {
				_, err := db.ExecContext(ctx, "CREATE TABLE workspace_extraction_forbidden_write (id text)")
				return err
			})
			require.ErrorContains(t, err, "read-only")
			require.NotEqual(t, first, backend(), "a failed session must be replaced before a later explicit read")
		})
	}
}

func TestDatabaseSessionDoesNotExposeMalformedDSN(t *testing.T) {
	session := newDatabaseSession("postgres://probe:private-test-secret@bad%escape/db", "workspace_extraction_parse_test", time.Second)
	defer session.Close()
	err := session.query(context.Background(), func(context.Context, *sql.DB) error { return errors.New("unreachable") })
	require.EqualError(t, err, "invalid content extraction database configuration")
}
