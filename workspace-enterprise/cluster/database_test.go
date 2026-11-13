package cluster

import (
	"context"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestCoordinationConnectionSilentIO(t *testing.T) {
	for _, write := range []bool{false, true} {
		name := "read"
		if write {
			name = "write"
		}
		t.Run(name, func(t *testing.T) {
			local, peer := net.Pipe()
			defer peer.Close()
			conn := &coordinationConn{Conn: local, timeout: 80 * time.Millisecond}
			defer conn.Close()
			started := time.Now()
			var err error
			if write {
				_, err = conn.Write([]byte("query"))
			} else {
				_, err = conn.Read(make([]byte, 1))
			}
			require.Error(t, err)
			var timeout net.Error
			require.ErrorAs(t, err, &timeout)
			require.True(t, timeout.Timeout())
			require.Less(t, time.Since(started), time.Second)
		})
	}
}

func TestCoordinationConnectionPreservesEarlierDeadline(t *testing.T) {
	for _, operation := range []string{"both", "read", "write"} {
		t.Run(operation, func(t *testing.T) {
			local, peer := net.Pipe()
			defer peer.Close()
			conn := &coordinationConn{Conn: local, timeout: 5 * time.Second}
			defer conn.Close()
			deadline := time.Now().Add(60 * time.Millisecond)
			switch operation {
			case "both":
				require.NoError(t, conn.SetDeadline(deadline))
			case "read":
				require.NoError(t, conn.SetReadDeadline(deadline))
			case "write":
				require.NoError(t, conn.SetWriteDeadline(deadline))
			}
			started := time.Now()
			var err error
			if operation == "write" {
				_, err = conn.Write([]byte("query"))
			} else {
				_, err = conn.Read(make([]byte, 1))
			}
			require.Error(t, err)
			require.Less(t, time.Since(started), time.Second, "idle timeout must not extend the driver's deadline")
		})
	}
}

func TestCoordinationConnectionClearsDeadlineAndRefreshesIdleTimeout(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	conn := &coordinationConn{Conn: local, timeout: 200 * time.Millisecond}
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(-time.Second)))
	require.NoError(t, conn.SetDeadline(time.Time{}))
	// Two reads span longer than the idle timeout, but neither read is silent
	// for that long. Clearing the driver's startup deadline must also work.
	completed := make(chan error, 1)
	go func() {
		for range 2 {
			time.Sleep(130 * time.Millisecond)
			if _, err := peer.Write([]byte("x")); err != nil {
				completed <- err
				return
			}
		}
		completed <- nil
	}()
	for range 2 {
		buffer := make([]byte, 1)
		_, err := conn.Read(buffer)
		require.NoError(t, err)
		require.Equal(t, byte('x'), buffer[0])
	}
	require.NoError(t, <-completed)
}

func TestCoordinationDatabaseRejectsInvalidDSNWithoutSecrets(t *testing.T) {
	secret := "private-password-must-not-be-logged"
	db, err := newCoordinationDatabase("postgres://user:" + secret + "@localhost/db?sslmode=invalid")
	require.Error(t, err)
	require.Nil(t, db)
	require.False(t, strings.Contains(err.Error(), secret))
}

func TestCoordinationDatabaseTLSVerifyFull(t *testing.T) {
	dsn := os.Getenv("WORKSPACE_CLUSTER_TEST_TLS_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL TLS fixture is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := newCoordinationDatabase(dsn)
	require.NoError(t, err)
	defer db.Close()
	var encrypted bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()`).Scan(&encrypted))
	require.True(t, encrypted)
	address, err := url.Parse(dsn)
	require.NoError(t, err)
	t.Run("hostname mismatch", func(t *testing.T) {
		invalid := *address
		invalid.Host = net.JoinHostPort("localhost", address.Port())
		db, err := newCoordinationDatabase(invalid.String())
		require.NoError(t, err)
		defer db.Close()
		err = db.PingContext(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "certificate")
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		untrusted := os.Getenv("WORKSPACE_CLUSTER_TEST_UNTRUSTED_CA")
		require.NotEmpty(t, untrusted)
		invalid := *address
		query := invalid.Query()
		query.Set("sslrootcert", untrusted)
		invalid.RawQuery = query.Encode()
		db, err := newCoordinationDatabase(invalid.String())
		require.NoError(t, err)
		defer db.Close()
		err = db.PingContext(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "certificate")
	})
}

func TestCoordinationDatabaseKeepsInitializedPrimaryAndOwnsPool(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	})
	s := New(th.Server.Platform()).(*Service)
	t.Cleanup(s.StopInterNodeCommunication)
	db, err := s.coordinationDatabase()
	require.NoError(t, err)
	require.NotSame(t, th.Store.GetInternalMasterDB(), db)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx))
	original := th.App.Config().Clone().SqlSettings
	t.Cleanup(func() { th.App.UpdateConfig(func(cfg *model.Config) { cfg.SqlSettings = original }) })
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.SqlSettings.DataSource = new("postgres://127.0.0.1:1/not-the-initialized-primary?sslmode=disable")
	})
	// Core keeps using its initialized primary until a server restart. A config
	// save must not move coordination to a different database or peer's proxy.
	afterSave, err := s.coordinationDatabase()
	require.NoError(t, err)
	require.Same(t, db, afterSave)
	require.NoError(t, afterSave.PingContext(ctx))
	s.StopInterNodeCommunication() // Also close a pool when transport never started.
	require.ErrorContains(t, db.PingContext(ctx), "database is closed")
	require.NoError(t, th.Store.GetInternalMasterDB().PingContext(ctx))
}
