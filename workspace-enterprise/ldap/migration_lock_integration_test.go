package ldap

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLDAPMigrationLockWaiterKeepsApplicationConnection(t *testing.T) {
	th, _ := setupDirectoryAPI(t)
	service := th.App.Ldap().(*Service)
	owner := newService(th.App)
	releaseOwner, err := owner.acquireSyncLock(th.Context.Context())
	require.NoError(t, err)
	defer releaseOwner()
	observer, err := sql.Open("postgres", *th.App.Config().SqlSettings.DataSource)
	require.NoError(t, err)
	defer observer.Close()
	observer.SetMaxOpenConns(1)
	db := th.Store.GetInternalMasterDB()
	previousLimit := db.Stats().MaxOpenConnections
	db.SetMaxOpenConns(1)
	defer db.SetMaxOpenConns(previousLimit)
	ctx, cancel := context.WithTimeout(th.Context.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- service.MigrateIDAttribute(th.Context.WithContext(ctx), "newIdentity")
	}()
	defer func() {
		cancel()
		releaseOwner()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("canceled migration did not release its lock wait")
		}
	}()
	require.Eventually(t, func() bool {
		var waiting int
		return observer.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks
			WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
			AND classid=1 AND objid=2197294107 AND NOT granted`).Scan(&waiting) == nil && waiting == 1
	}, time.Second, 10*time.Millisecond, "probe only after migration is actually waiting for the sync lock")
	probe, stop := context.WithTimeout(th.Context.Context(), 200*time.Millisecond)
	defer stop()
	var one int
	require.NoError(t, db.QueryRowContext(probe, `SELECT 1`).Scan(&one), "an administrative migration waiting behind a sync must leave its native connection available")
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("migration lock wait ignored cancellation")
	}
}
