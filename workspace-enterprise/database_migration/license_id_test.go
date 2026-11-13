package database_migration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func licenseTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("WORKSPACE_LICENSE_MIGRATION_TEST_DSN")
	if dsn == "" {
		t.Skip("set WORKSPACE_LICENSE_MIGRATION_TEST_DSN to an isolated local PostgreSQL cluster")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	var local bool
	var directory string
	require.NoError(t, db.QueryRow(`SELECT inet_server_addr() IS NULL, current_setting('data_directory')`).Scan(&local, &directory))
	// Never run DDL tests against a user's working database, even by mistake.
	if !local || !strings.HasPrefix(directory, "/tmp/workspace-license-migration-") {
		t.Fatal("tests require a disposable /tmp/workspace-license-migration-* cluster over a Unix socket")
	}
	schema := fmt.Sprintf("license_test_%d", time.Now().UnixNano())
	_, err = db.Exec("CREATE SCHEMA " + pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec("DROP SCHEMA " + pq.QuoteIdentifier(schema) + " CASCADE")
		require.NoError(t, err)
	})
	// The test DSN uses libpq keyword format; schema is generated locally.
	target, err := sql.Open("postgres", dsn+" search_path="+schema)
	require.NoError(t, err)
	t.Cleanup(func() { target.Close() })
	return target
}

func TestRepairLicenseIDPostgres(t *testing.T) {
	for _, tc := range []struct {
		name, ddl, rows, errorText string
		changed                    bool
	}{
		{name: "missing key", changed: true},
		{name: "empty table", rows: "empty", changed: true},
		{name: "existing primary key", ddl: "ALTER TABLE licenses ADD PRIMARY KEY(id)"},
		{name: "existing unique index", ddl: "CREATE UNIQUE INDEX license_unique ON licenses(id) INCLUDE(createat)"},
		{name: "partial index is insufficient", ddl: "CREATE UNIQUE INDEX license_partial ON licenses(id) WHERE createat>0", changed: true},
		{name: "composite index is insufficient", ddl: "CREATE UNIQUE INDEX license_composite ON licenses(id,createat)", changed: true},
		{name: "another primary key", ddl: "ALTER TABLE licenses ADD PRIMARY KEY(createat)", changed: true},
		{name: "duplicates", rows: "('one',1,'a'),('one',2,'b')", errorText: "duplicates (true)"},
		{name: "null IDs", rows: "(NULL,1,'a')", errorText: "NULLs (true)"},
		{name: "deferrable key", ddl: "ALTER TABLE licenses ADD UNIQUE(id) DEFERRABLE", errorText: "deferrable"},
		{name: "constraint name collision", ddl: "ALTER TABLE licenses ADD CONSTRAINT licenses_pkey CHECK(createat>0)", errorText: "restore license ID constraint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := licenseTestDatabase(t)
			_, err := db.Exec("CREATE TABLE licenses(id varchar(26),createat bigint,bytes varchar(10000))")
			require.NoError(t, err)
			if tc.ddl != "" {
				_, err = db.Exec(tc.ddl)
				require.NoError(t, err)
			}
			rows := tc.rows
			if rows == "" {
				rows = "('one',1,'a'),('two',2,'b')"
			}
			if rows != "empty" {
				_, err = db.Exec("INSERT INTO licenses VALUES " + rows)
				require.NoError(t, err)
			}
			var before, after string
			const snapshot = `SELECT COALESCE(jsonb_agg(to_jsonb(l) ORDER BY createat)::text,'[]') FROM licenses l`
			require.NoError(t, db.QueryRow(snapshot).Scan(&before))
			changed, err := RepairLicenseID(context.Background(), db)
			if tc.errorText != "" {
				require.ErrorContains(t, err, tc.errorText)
				require.False(t, changed)
				var keys int
				require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conrelid='licenses'::regclass AND conname='workspace_licenses_id_key'`).Scan(&keys))
				require.Zero(t, keys)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.changed, changed)
				changed, err = RepairLicenseID(context.Background(), db)
				require.NoError(t, err)
				require.False(t, changed, "second run must be a no-op")
				result, err := db.Exec(`INSERT INTO licenses(id,createat,bytes)
					SELECT id,createat,bytes FROM licenses ON CONFLICT(id) DO NOTHING`)
				require.NoError(t, err)
				count, err := result.RowsAffected()
				require.NoError(t, err)
				require.Zero(t, count)
			}
			require.NoError(t, db.QueryRow(snapshot).Scan(&after))
			require.Equal(t, before, after, "license records must be preserved")
		})
	}
}

func TestRepairLicenseIDMissingTable(t *testing.T) {
	db := licenseTestDatabase(t)
	changed, err := RepairLicenseID(context.Background(), db)
	require.ErrorContains(t, err, "resolve licenses table")
	require.False(t, changed)
}
