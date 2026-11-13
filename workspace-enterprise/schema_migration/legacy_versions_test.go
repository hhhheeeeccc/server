package schema_migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/enterprise/database_migration"
	coredb "github.com/mattermost/mattermost/server/v8/channels/db"
	"github.com/stretchr/testify/require"
)

func isolatedHistoryDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("WORKSPACE_LICENSE_MIGRATION_TEST_DSN")
	if dsn == "" {
		t.Skip("set WORKSPACE_LICENSE_MIGRATION_TEST_DSN to a disposable local cluster")
	}
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })
	var local bool
	var directory string
	require.NoError(t, admin.QueryRow(`SELECT inet_server_addr() IS NULL,current_setting('data_directory')`).Scan(&local, &directory))
	require.True(t, local && strings.HasPrefix(directory, "/tmp/workspace-license-migration-"), "tests require a disposable cluster over a Unix socket")
	schema := fmt.Sprintf("history_test_%d", time.Now().UnixNano())
	_, err = admin.Exec("CREATE SCHEMA " + pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec("DROP SCHEMA " + pq.QuoteIdentifier(schema) + " CASCADE")
		require.NoError(t, err)
	})
	db, err := sql.Open("postgres", dsn+" search_path="+schema)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestReconcileRestoredHistory(t *testing.T) {
	fixture, err := os.ReadFile("testdata/sofa_history_20261015.json")
	require.NoError(t, err)
	var history []struct{ Version, Name string }
	require.NoError(t, json.Unmarshal(fixture, &history))
	require.Len(t, history, 240)
	for _, tc := range []struct {
		name, alter, errorText string
	}{
		{name: "overlapping old and current versions"},
		{name: "duplicate legacy identity", alter: `INSERT INTO db_migrations VALUES (153,'add_download_count')`, errorText: "found 2"},
		{name: "missing legacy identity", alter: `DELETE FROM db_migrations WHERE version=153 AND name='add_download_count'`, errorText: "found 0"},
		{name: "unknown migration", alter: `INSERT INTO db_migrations VALUES (999,'unknown_migration')`, errorText: "incompatible database migration history"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := isolatedHistoryDB(t)
			// The restored database has no primary key and contains overlapping
			// version numbers. Preserve that layout to reproduce the failure.
			_, err := db.Exec(`CREATE TABLE db_migrations(version bigint,name varchar)`)
			require.NoError(t, err)
			for _, row := range history {
				_, err := db.Exec(`INSERT INTO db_migrations VALUES ($1,$2)`, row.Version, row.Name)
				require.NoError(t, err)
			}
			if tc.alter != "" {
				_, err := db.Exec(tc.alter)
				require.NoError(t, err)
			}
			const snapshot = `SELECT jsonb_agg(to_jsonb(m) ORDER BY version,name)::text FROM db_migrations m`
			var before, after string
			require.NoError(t, db.QueryRow(snapshot).Scan(&before))
			changed, err := ReconcileLegacyVersions(context.Background(), db)
			if tc.errorText != "" {
				require.ErrorContains(t, err, tc.errorText)
				require.False(t, changed)
				require.NoError(t, db.QueryRow(snapshot).Scan(&after))
				require.Equal(t, before, after, "failed reconciliation must preserve every history row")
				var noArchive bool
				require.NoError(t, db.QueryRow(`SELECT to_regclass('db_migrations_legacy_versions') IS NULL`).Scan(&noArchive))
				require.True(t, noArchive)
				return
			}
			require.NoError(t, err)
			require.True(t, changed)
			expected, err := database_migration.Read(coredb.Assets())
			require.NoError(t, err)
			var count int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM db_migrations`).Scan(&count))
			// Reconciliation renumbers the historical fixture; it must not
			// mark migrations added since that snapshot as already applied.
			require.Equal(t, len(history)-len(branchOnly), count)
			rows, err := db.Query(`SELECT version,name FROM db_migrations`)
			require.NoError(t, err)
			for rows.Next() {
				var version int
				var actual string
				require.NoError(t, rows.Scan(&version, &actual))
				require.Contains(t, expected, version)
				require.Equal(t, expected[version], actual)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM db_migrations_legacy_versions`).Scan(&count))
			require.Equal(t, 13, count)
			var namesBefore, namesAfter string
			require.NoError(t, db.QueryRow(`SELECT string_agg(value->>'name',',' ORDER BY value->>'name') FROM jsonb_array_elements($1::jsonb)`, before).Scan(&namesBefore))
			require.NoError(t, db.QueryRow(`SELECT string_agg(name,',' ORDER BY name) FROM (SELECT name FROM db_migrations UNION ALL SELECT name FROM db_migrations_legacy_versions) m`).Scan(&namesAfter))
			require.Equal(t, namesBefore, namesAfter, "all original migration identities must remain active or archived")
			require.NoError(t, db.QueryRow(snapshot).Scan(&before))
			changed, err = ReconcileLegacyVersions(context.Background(), db)
			require.NoError(t, err)
			require.False(t, changed)
			require.NoError(t, db.QueryRow(snapshot).Scan(&after))
			require.Equal(t, before, after)
		})
	}
}

func TestReintroducedHistoryValidatesEveryRecord(t *testing.T) {
	db := isolatedHistoryDB(t)
	fixture, err := os.ReadFile("testdata/sofa_history_20261015.json")
	require.NoError(t, err)
	var history []struct{ Version, Name string }
	require.NoError(t, json.Unmarshal(fixture, &history))
	_, err = db.Exec(`CREATE TABLE db_migrations(version bigint,name varchar)`)
	require.NoError(t, err)
	for _, row := range history {
		_, err := db.Exec(`INSERT INTO db_migrations VALUES($1,$2)`, row.Version, row.Name)
		require.NoError(t, err)
	}
	_, err = ReconcileLegacyVersions(context.Background(), db)
	require.NoError(t, err)
	for _, entry := range renumbered[:6] {
		_, err := db.Exec(`UPDATE db_migrations SET name=$1 WHERE version=$2`, branchOnly[entry.new-153].name, entry.new)
		require.NoError(t, err)
	}
	_, err = db.Exec(`UPDATE db_migrations SET name='unknown_record' WHERE version=42`)
	require.NoError(t, err)
	const snapshot = `SELECT jsonb_agg(to_jsonb(m) ORDER BY version,name)::text FROM db_migrations m`
	var before, after string
	require.NoError(t, db.QueryRow(snapshot).Scan(&before))
	changed, err := ReconcileLegacyVersions(context.Background(), db)
	require.ErrorContains(t, err, "incompatible database migration history")
	require.False(t, changed)
	require.NoError(t, db.QueryRow(snapshot).Scan(&after))
	require.Equal(t, before, after)
	expected, err := database_migration.Read(coredb.Assets())
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE db_migrations SET name=$1 WHERE version=42`, expected[42])
	require.NoError(t, err)
	changed, err = ReconcileLegacyVersions(context.Background(), db)
	require.NoError(t, err)
	require.True(t, changed)
	require.NoError(t, database_migration.CheckDatabase(db))
	changed, err = ReconcileLegacyVersions(context.Background(), db)
	require.NoError(t, err)
	require.False(t, changed)
}
