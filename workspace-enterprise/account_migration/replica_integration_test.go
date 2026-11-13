//go:build physicalreplica

package account_migration

import (
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/store/sqlstore"
	"github.com/stretchr/testify/require"
)

func migrationReplicaURL(t *testing.T) *url.URL {
	t.Helper()
	raw := os.Getenv("WORKSPACE_TEST_MIGRATION_REPLICA_DSN")
	if raw == "" {
		t.Skip("use deployment/account_migration/run_replica_test.py for a disposable physical standby")
	}
	u, err := url.Parse(raw)
	require.NoError(t, err)
	ip := net.ParseIP(u.Hostname())
	require.True(t, u.Scheme == "postgres" && ip != nil && ip.IsLoopback() && u.Port() != "", "only a literal loopback test database is allowed")
	return u
}

func catchUpMigrationReplica(t *testing.T, primary, replica *sql.DB) {
	t.Helper()
	var lsn string
	require.NoError(t, primary.QueryRow(`SELECT pg_current_wal_lsn()::text`).Scan(&lsn))
	require.Eventually(t, func() bool {
		var caughtUp bool
		return replica.QueryRow(`SELECT COALESCE(pg_last_wal_replay_lsn() >= $1::pg_lsn, false)`, lsn).Scan(&caughtUp) == nil && caughtUp
	}, 30*time.Second, 50*time.Millisecond, "the physical standby did not replay the primary's WAL")
}

func TestMigrationWithDelayedPhysicalReplica(t *testing.T) {
	base := migrationReplicaURL(t)
	for _, explicit := range []bool{false, true} {
		name := "native_email"
		if explicit {
			name = "external_id"
		}
		t.Run(name, func(t *testing.T) {
			th, service := setupMigration(t)
			primary := th.Store.GetInternalMasterDB()
			before := localUsers(t, th)
			if explicit {
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.IdAttribute = new("immutable-oid") })
			}
			// Wait using the always-existing postgres database before connecting
			// to the test database, whose creation must also be replayed.
			control, err := sql.Open("postgres", base.String())
			require.NoError(t, err)
			t.Cleanup(func() { _ = control.Close() })
			catchUpMigrationReplica(t, primary, control)
			settings := th.App.Config().SqlSettings
			primaryURL, err := url.Parse(*settings.DataSource)
			require.NoError(t, err)
			replicaURL := *base
			replicaURL.Path = primaryURL.Path
			settings.DataSourceReplicas = []string{replicaURL.String()}
			readStore, err := sqlstore.New(settings, th.App.Log(), nil, sqlstore.SkipMigrations())
			require.NoError(t, err)
			readStore.UpdateLicense(th.App.License())
			original := th.Server.Store()
			// Restore the app before its ordinary test cleanup closes stores.
			t.Cleanup(func() {
				_, _ = control.Exec(`SELECT pg_wal_replay_resume()`)
				th.Server.SetStore(original)
				readStore.Close()
			})
			var recovery bool
			require.NoError(t, readStore.GetInternalReplicaDB().QueryRow(`SELECT pg_is_in_recovery()`).Scan(&recovery))
			require.True(t, recovery, "the app must select the physical standby, not silently fall back to the primary")
			catchUpMigrationReplica(t, primary, control)
			_, err = control.Exec(`SELECT pg_wal_replay_pause()`)
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				var paused bool
				return control.QueryRow(`SELECT pg_is_wal_replay_paused()`).Scan(&paused) == nil && paused
			}, 5*time.Second, 10*time.Millisecond)

			// These commits exist on the primary before migration starts, but
			// cannot appear on the deliberately paused read-only standby.
			lateUser := th.CreateUser(t)
			lateUser, err = th.Store.User().Get(th.Context, lateUser.Id)
			require.NoError(t, err)
			lateSession := th.CreateClient()
			session, appErr := th.App.CreateSession(th.Context, &model.Session{UserId: th.BasicUser.Id,
				Roles: th.BasicUser.Roles, ExpiresAt: model.GetMillis() + time.Hour.Milliseconds()})
			require.Nil(t, appErr)
			lateSession.SetToken(session.Token)
			_, response, err := lateSession.GetMe(th.Context.Context(), "")
			require.NoError(t, err, "the session must be valid before migration")
			require.Equal(t, http.StatusOK, response.StatusCode)
			staleUsers, err := readStore.User().GetAllUsingAuthService("")
			require.NoError(t, err)
			require.Len(t, staleUsers, len(before))
			var missing int
			require.NoError(t, readStore.GetInternalReplicaDB().QueryRow(`SELECT count(*) FROM Sessions WHERE Token=$1`, lateSession.AuthToken).Scan(&missing))
			require.Zero(t, missing, "the newly committed session must really be absent from the replica")
			th.Server.SetStore(readStore)
			// The production constructor installs this after receiving its store.
			// Reapply it after this test's deliberate physical-store substitution.
			installPrimarySessionLookups(th.App)

			migrate := func(users []*model.User, dryRun bool) *model.AppError {
				if !explicit {
					return service.MigrateToSaml(th.Context, "email", samlMap(users), false, dryRun)
				}
				identities := make([]SAMLIdentity, 0, len(users))
				for _, user := range users {
					identities = append(identities, SAMLIdentity{UserID: user.Id, Username: user.Username, Email: user.Email, SAMLID: "oid-" + user.Id})
				}
				return service.MigrateToSamlIdentities(th.Context, "email", identities, dryRun)
			}
			appErr = migrate(before, false)
			require.NotNil(t, appErr, "a stale replica must not turn a map omitting the newly committed account into a successful partial migration")
			require.Equal(t, http.StatusBadRequest, appErr.StatusCode)
			all := append(before, lateUser)
			assertUnchanged(t, th, all)
			require.Nil(t, migrate(all, true))
			assertUnchanged(t, th, all)
			require.Nil(t, migrate(all, false))
			for _, old := range all {
				stored, err := th.Store.User().Get(th.Context, old.Id)
				require.NoError(t, err)
				require.Equal(t, model.UserAuthServiceSaml, stored.AuthService)
				require.Empty(t, stored.Password)
			}
			var remaining int
			require.NoError(t, primary.QueryRow(`SELECT count(*) FROM Sessions WHERE UserId=$1`, th.BasicUser.Id).Scan(&remaining))
			require.Zero(t, remaining, "cleanup must include the session missing from the replica")
			require.NoError(t, primary.QueryRow(`SELECT count(*) FROM WorkspaceMigrationSessionCleanup`).Scan(&remaining))
			require.Zero(t, remaining)
			for _, client := range []*model.Client4{th.Client, lateSession} {
				_, response, err = client.GetMe(th.Context.Context(), "")
				require.Error(t, err, "revoked sessions must remain invalid while the read replica still contains an old session")
				require.Equal(t, http.StatusUnauthorized, response.StatusCode)
			}
			_, err = control.Exec(`SELECT pg_wal_replay_resume()`)
			require.NoError(t, err)
			catchUpMigrationReplica(t, primary, control)
			for _, client := range []*model.Client4{th.Client, lateSession} {
				_, response, err = client.GetMe(th.Context.Context(), "")
				require.Error(t, err)
				require.Equal(t, http.StatusUnauthorized, response.StatusCode)
			}
			recordMigrationReplicaResult(t, name, len(all))
		})
	}
}

func recordMigrationReplicaResult(t *testing.T, name string, migrated int) {
	t.Helper()
	result := map[string]any{"status": "passed", "physicalReadOnlyReplica": true,
		"replayPausedDuringMigration": true, "staleSourceRejected": true,
		"lateAccountMigrated": true, "lateSessionRevoked": true, "dryRunPreservedSource": true,
		"migratedAccounts": migrated, "cleanupIntentsRemaining": 0, "replicationResumed": true,
		"revokedSessionsRejectedWhileReplicaPaused": 2, "lateSessionValidBeforeMigration": true}
	encoded, err := json.MarshalIndent(result, "", "  ")
	require.NoError(t, err)
	if folder := os.Getenv("WORKSPACE_TEST_MIGRATION_REPLICA_OUTPUT"); folder != "" {
		require.NoError(t, os.WriteFile(filepath.Join(folder, name+".json"), append(encoded, '\n'), 0o600))
	}
	t.Logf("PHYSICAL_REPLICA_RESULT %s", encoded)
}
