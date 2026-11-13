package ldap

import (
	"context"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestLDAPSyncCompletesWithOneApplicationConnection(t *testing.T) {
	for _, mode := range []string{"snapshot", "first_login"} {
		t.Run(mode, func(t *testing.T) {
			th, directory := setupDirectoryAPI(t)
			directory.setUser(t, userFixture())
			th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.LdapSettings.EnableSync = new(true)
				cfg.LdapSettings.FirstNameAttribute = new("givenName")
			})
			service := th.App.Ldap().(*Service)
			user, appErr := service.DoLogin(th.Context, "user-1", "user-password")
			require.Nil(t, appErr)
			_, appErr = th.App.CreateGroup(&model.Group{DisplayName: "Pool fixture", Source: model.GroupSourceLdap, RemoteId: new("group-a")})
			require.Nil(t, appErr)
			attrs := userFixture()
			attrs["givenName"] = []string{"Updated with one application connection"}
			directory.setUser(t, attrs)
			db := th.Store.GetInternalMasterDB()
			previousLimit := db.Stats().MaxOpenConnections
			db.SetMaxOpenConns(1)
			defer db.SetMaxOpenConns(previousLimit)
			ctx, cancel := context.WithTimeout(th.Context.Context(), 1500*time.Millisecond)
			defer cancel()
			if mode == "snapshot" {
				appErr = service.synchronize(th.Context.WithContext(ctx), false)
			} else {
				appErr = service.FirstLoginSync(th.Context.WithContext(ctx), user)
			}
			require.Nil(t, appErr, "the coordination lock must leave the application pool available for native reads and writes")
			require.NoError(t, ctx.Err())
			if mode == "snapshot" {
				stored, err := th.App.GetUser(th.Context, user.Id)
				require.Nil(t, err)
				require.Equal(t, attrs["givenName"][0], stored.FirstName)
			}
		})
	}
}

func TestLDAPSyncLockWaitersLeaveApplicationPoolAvailable(t *testing.T) {
	th, _ := setupDirectoryAPI(t)
	service := th.App.Ldap().(*Service)
	owner := newService(th.App)
	releaseOwner, err := owner.acquireSyncLock(th.Context.Context())
	require.NoError(t, err)
	defer releaseOwner()
	db := th.Store.GetInternalMasterDB()
	previousLimit := db.Stats().MaxOpenConnections
	db.SetMaxOpenConns(1)
	defer db.SetMaxOpenConns(previousLimit)
	ctx, cancel := context.WithCancel(th.Context.Context())
	defer cancel()
	done := make(chan error, 3)
	for range 3 {
		go func() {
			release, err := service.acquireSyncLock(ctx)
			if release != nil {
				release()
			}
			done <- err
		}()
	}
	probe, stopProbe := context.WithTimeout(th.Context.Context(), time.Second)
	defer stopProbe()
	var one int
	require.NoError(t, db.QueryRowContext(probe, `SELECT 1`).Scan(&one), "lock waiters must not occupy the native SQL pool")
	require.Eventually(t, func() bool {
		var waiting int
		return db.QueryRow(`SELECT count(*) FROM pg_locks
			WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
			AND classid=1 AND objid=2197294107 AND NOT granted`).Scan(&waiting) == nil && waiting == 1
	}, 2*time.Second, 10*time.Millisecond, "the local gate must allow only one private database waiter")
	cancel()
	for range 3 {
		select {
		case err := <-done:
			require.Error(t, err, "a waiting caller must not pass another service's held lock")
		case <-time.After(2 * time.Second):
			t.Fatal("database or local-gate waiter ignored cancellation")
		}
	}
	releaseOwner()
	probe, stop := context.WithTimeout(th.Context.Context(), time.Second)
	defer stop()
	release, err := service.acquireSyncLock(probe)
	require.NoError(t, err, "all failed waiters must release the gate and connection")
	release()
}

func TestLDAPSyncLockUsesDatabaseSelectedAtConstruction(t *testing.T) {
	th, _ := setupDirectoryAPI(t)
	service := th.App.Ldap().(*Service)
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.SqlSettings.DataSource = new("postgres://invalid-saved-settings@127.0.0.1:1/unused?sslmode=disable")
	})
	ctx, cancel := context.WithTimeout(th.Context.Context(), time.Second)
	defer cancel()
	release, err := service.acquireSyncLock(ctx)
	require.NoError(t, err, "saved unapplied SQL settings must not redirect synchronization away from the running application database")
	release()
}
