package account_migration

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/mattermost/enterprise/internal/advisorylock"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestAuthMigrationLockWaiterKeepsApplicationConnection(t *testing.T) {
	th, service := setupMigration(t)
	owner := advisorylock.New(*th.App.Config().SqlSettings.DataSource)
	releaseOwner, err := owner.Acquire(th.Context.Context(), 6492261403)
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
		done <- service.MigrateToSaml(th.Context.WithContext(ctx), "email", nil, true, true)
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

func TestAuthMigrationReleasesCoordinationBeforeSessionCleanup(t *testing.T) {
	th, service := setupMigration(t)
	db := th.Store.GetInternalMasterDB()
	ctx, cancel := context.WithTimeout(th.Context.Context(), 10*time.Second)
	blocker, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	done := make(chan *model.AppError, 1)
	started := false
	t.Cleanup(func() {
		cancel()
		_ = blocker.Rollback()
		if started {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("migration cleanup did not finish after its blocker was released")
			}
		}
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_, err := db.ExecContext(cleanup, `DROP TRIGGER IF EXISTS workspace_migration_gate_probe ON Sessions`)
		require.NoError(t, err)
		_, err = db.ExecContext(cleanup, `DROP FUNCTION IF EXISTS workspace_migration_gate_probe()`)
		require.NoError(t, err)
	})
	_, err = blocker.ExecContext(ctx, `SELECT pg_advisory_xact_lock(6492261,412)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION workspace_migration_gate_probe() RETURNS trigger AS $$
		BEGIN IF OLD.UserId='%s' THEN PERFORM pg_advisory_xact_lock(6492261,412);
		END IF; RETURN OLD; END; $$ LANGUAGE plpgsql`, th.BasicUser.Id))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE TRIGGER workspace_migration_gate_probe BEFORE DELETE ON Sessions
		FOR EACH ROW EXECUTE FUNCTION workspace_migration_gate_probe()`)
	require.NoError(t, err)
	started = true
	go func() {
		defer close(done)
		done <- service.MigrateToSaml(th.Context.WithContext(ctx), "email", nil, true, false)
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		return db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory'
			AND classid=6492261 AND objid=412 AND NOT granted)`).Scan(&waiting) == nil && waiting
	}, 3*time.Second, 10*time.Millisecond, "the real migration must commit and reach native session deletion before probing its coordination lock")
	probe, stop := context.WithTimeout(ctx, 250*time.Millisecond)
	defer stop()
	release, err := advisorylock.New(*th.App.Config().SqlSettings.DataSource).Acquire(probe, 6492261403)
	if release != nil {
		defer release()
	}
	require.NoError(t, err, "postcommit session revocation must not extend the LDAP synchronization lock lifetime")
	require.NoError(t, blocker.Rollback())
	select {
	case appErr := <-done:
		require.Nil(t, appErr)
	case <-ctx.Done():
		t.Fatal("migration failed to finish after its cleanup was released")
	}
}
