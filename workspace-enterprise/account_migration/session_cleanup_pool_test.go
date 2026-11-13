package account_migration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestSessionCleanupCompletesWithOneDatabaseConnection(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	service := &Service{app: th.App}
	require.NoError(t, service.initSessionCleanup())
	db := th.Store.GetInternalMasterDB()
	cutoff := model.GetMillis()
	_, err := db.Exec(`INSERT INTO WorkspaceMigrationSessionCleanup (UserId,CutoffAt,RetryAt) VALUES ($1,$2,0)`, th.BasicUser.Id, cutoff)
	require.NoError(t, err)
	newer, err := th.Store.Session().Save(th.Context, &model.Session{UserId: th.BasicUser.Id, ExpiresAt: cutoff + 3600000})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE Sessions SET CreateAt=$1 WHERE Id=$2`, cutoff+1, newer.Id)
	require.NoError(t, err)

	previousLimit := db.Stats().MaxOpenConnections
	db.SetMaxOpenConns(1)
	defer db.SetMaxOpenConns(previousLimit)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, service.retryPendingSessionCleanup(ctx), "the worker must release its database connection before native session reads and revocation")

	sessions, err := th.Store.Session().GetSessions(th.Context, th.BasicUser.Id)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, newer.Id, sessions[0].Id, "the cleanup cutoff must continue to protect newer sessions")
	var pending int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM WorkspaceMigrationSessionCleanup WHERE UserId=$1`, th.BasicUser.Id).Scan(&pending))
	require.Zero(t, pending)
}

func TestSessionCleanupFencesConcurrentReplacement(t *testing.T) {
	for _, tc := range []struct {
		name       string
		newCutoff  bool
		failRevoke bool
	}{
		{name: "new_cutoff", newCutoff: true},
		{name: "new_claim"},
		{name: "failed_old_claim", failRevoke: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			th := api4.SetupConfig(t, func(cfg *model.Config) {
				cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
			}).InitBasic(t)
			service := &Service{app: th.App}
			require.NoError(t, service.initSessionCleanup())
			db := th.Store.GetInternalMasterDB()
			userID := th.BasicUser.Id
			cutoff := model.GetMillis()
			_, err := db.Exec(`INSERT INTO WorkspaceMigrationSessionCleanup (UserId,CutoffAt,RetryAt) VALUES ($1,$2,0)`, userID, cutoff)
			require.NoError(t, err)
			newer, err := th.Store.Session().Save(th.Context, &model.Session{UserId: userID, ExpiresAt: cutoff + 3600000})
			require.NoError(t, err)
			_, err = db.Exec(`UPDATE Sessions SET CreateAt=$1 WHERE Id=$2`, cutoff+1, newer.Id)
			require.NoError(t, err)

			// Hold a different lock from the cleanup row. The worker must
			// release its claim connection before reaching native revocation.
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			blocker, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			done := make(chan struct{})
			started := false
			var workerErr error
			t.Cleanup(func() {
				cancel()
				_ = blocker.Rollback()
				if started {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("session cleanup worker did not finish after releasing its blocker")
					}
				}
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cleanupCancel()
				_, err := db.ExecContext(cleanupCtx, `DROP TRIGGER IF EXISTS workspace_cleanup_pool_block ON Sessions`)
				require.NoError(t, err)
				_, err = db.ExecContext(cleanupCtx, `DROP FUNCTION IF EXISTS workspace_cleanup_pool_block()`)
				require.NoError(t, err)
			})
			_, err = blocker.ExecContext(ctx, `SELECT pg_advisory_xact_lock(6492261, 411)`)
			require.NoError(t, err)
			failure := ""
			if tc.failRevoke {
				failure = "RAISE EXCEPTION 'fixture revocation failure';"
			}
			_, err = db.ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION workspace_cleanup_pool_block() RETURNS trigger AS $$
				BEGIN IF OLD.UserId='%s' THEN
					PERFORM pg_advisory_xact_lock(6492261, 411); %s
				END IF; RETURN OLD; END; $$ LANGUAGE plpgsql`, userID, failure))
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `CREATE TRIGGER workspace_cleanup_pool_block BEFORE DELETE ON Sessions
				FOR EACH ROW EXECUTE FUNCTION workspace_cleanup_pool_block()`)
			require.NoError(t, err)
			started = true
			go func() {
				defer close(done)
				workerErr = service.retryPendingSessionCleanup(ctx)
			}()
			require.Eventually(t, func() bool {
				var waiting bool
				return db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
					WHERE locktype='advisory' AND classid=6492261 AND objid=411 AND NOT granted)`).Scan(&waiting) == nil && waiting
			}, 3*time.Second, 10*time.Millisecond, "worker must reach the actual blocked native session deletion")

			var claim int64
			require.NoError(t, db.QueryRowContext(ctx, `SELECT RetryAt FROM WorkspaceMigrationSessionCleanup WHERE UserId=$1`, userID).Scan(&claim))
			require.Greater(t, claim, model.GetMillis(), "the claim must be visible while native session revocation waits")
			other := &Service{app: th.App}
			otherCtx, cancelOther := context.WithTimeout(ctx, time.Second)
			err = other.retryPendingSessionCleanup(otherCtx)
			cancelOther()
			require.NoError(t, err, "another worker must skip an active claim without waiting on native deletion")

			replacementCutoff := cutoff
			if tc.newCutoff {
				replacementCutoff += 2
			}
			replacementRetry := claim + time.Minute.Milliseconds()
			updateCtx, cancelUpdate := context.WithTimeout(ctx, time.Second)
			_, err = db.ExecContext(updateCtx, `UPDATE WorkspaceMigrationSessionCleanup SET CutoffAt=$2, RetryAt=$3 WHERE UserId=$1`,
				userID, replacementCutoff, replacementRetry)
			cancelUpdate()
			require.NoError(t, err, "native deletion must not retain a lock on the cleanup intent")
			require.NoError(t, blocker.Rollback())
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("worker did not finish after its native deletion was released")
			}
			require.NoError(t, workerErr)

			var storedCutoff, storedRetry int64
			require.NoError(t, db.QueryRowContext(ctx, `SELECT CutoffAt, RetryAt FROM WorkspaceMigrationSessionCleanup WHERE UserId=$1`, userID).Scan(&storedCutoff, &storedRetry))
			require.Equal(t, replacementCutoff, storedCutoff)
			require.Equal(t, replacementRetry, storedRetry, "old success or failure must never clear or reschedule a replacement intent")
			var preserved int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM Sessions WHERE Id=$1`, newer.Id).Scan(&preserved))
			require.Equal(t, 1, preserved, "old work must retain a session after its own cutoff")

			_, err = db.ExecContext(ctx, `DROP TRIGGER workspace_cleanup_pool_block ON Sessions`)
			require.NoError(t, err)
			// Simulate a due/expired claim without a wall-clock sleep.
			_, err = db.ExecContext(ctx, `UPDATE WorkspaceMigrationSessionCleanup SET RetryAt=0 WHERE UserId=$1`, userID)
			require.NoError(t, err)
			require.NoError(t, other.retryPendingSessionCleanup(ctx))
			var pending, remaining int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM WorkspaceMigrationSessionCleanup WHERE UserId=$1`, userID).Scan(&pending))
			require.Zero(t, pending, "replacement work must still be retryable")
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM Sessions WHERE UserId=$1`, userID).Scan(&remaining))
			if tc.newCutoff {
				require.Zero(t, remaining, "the newer intent includes the newly created session")
			} else {
				require.Equal(t, 1, remaining, "reclaiming the same cutoff must preserve the newer session")
			}
		})
	}
}
