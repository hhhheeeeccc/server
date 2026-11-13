package schema_migration

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/lib/pq"
	"github.com/mattermost/enterprise/database_migration"
	coredb "github.com/mattermost/mattermost/server/v8/channels/db"
	"github.com/stretchr/testify/require"
)

func TestNovemberDumpHistory(t *testing.T) {
	fixture, err := os.ReadFile("testdata/workspace_history_20261109.json")
	require.NoError(t, err)
	var history []struct{ Version, Name string }
	require.NoError(t, json.Unmarshal(fixture, &history))
	require.Len(t, history, 182)
	expected, err := database_migration.Read(coredb.Assets())
	require.NoError(t, err)
	for _, tc := range []struct {
		name, alter, errorText string
		addedCore              []int
	}{
		{name: "version primary key and views at 176"},
		{name: "unknown migration", alter: `UPDATE db_migrations SET name='unknown_record' WHERE version=42`, errorText: "incompatible database migration history"},
		{name: "missing duplicate translation record", alter: `DELETE FROM db_migrations WHERE version=159`},
		{name: "unknown record at optional version", alter: `UPDATE db_migrations SET name='unknown_record' WHERE version=159`, errorText: "incompatible database migration history"},
		{name: "missing original translation record", alter: `DELETE FROM db_migrations WHERE version=147`, errorText: "147/create_autotranslation_tables"},
		{name: "missing views index", alter: `DELETE FROM db_migrations WHERE version=167; DROP INDEX idx_views_channel_id_delete_at`, addedCore: []int{167}},
		{name: "missing linked field column", alter: `DELETE FROM db_migrations WHERE version IN (168,169); ALTER TABLE propertyfields DROP COLUMN linkedfieldid`, addedCore: []int{168, 169}},
		{name: "missing linked field index", alter: `DELETE FROM db_migrations WHERE version=169; DROP INDEX idx_propertyfields_linkedfieldid`, addedCore: []int{169}},
		{name: "missing duplicate and core migrations", alter: `DELETE FROM db_migrations WHERE version IN (159,167,168,169); DROP INDEX idx_views_channel_id_delete_at; ALTER TABLE propertyfields DROP COLUMN linkedfieldid`, addedCore: []int{167, 168, 169}},
		{name: "missing index and unknown history", alter: `DELETE FROM db_migrations WHERE version=167; DROP INDEX idx_views_channel_id_delete_at; UPDATE db_migrations SET name='unknown_record' WHERE version=42`, errorText: "incompatible database migration history"},
		{name: "unknown views identity", alter: `UPDATE db_migrations SET name='unknown_views' WHERE version=176`, errorText: "legacy create_views"},
		{name: "ambiguous views identity", alter: `UPDATE db_migrations SET name='create_views' WHERE version=169`, errorText: "ambiguous legacy create_views"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := isolatedHistoryDB(t)
			_, err := db.Exec(`CREATE TABLE db_migrations(version bigint PRIMARY KEY, name varchar NOT NULL);
				CREATE TABLE propertygroups(id varchar(26) PRIMARY KEY, name varchar(64) NOT NULL);
				INSERT INTO propertygroups VALUES ('existing', 'Existing group');
				CREATE TABLE views(channelid varchar(26), deleteat bigint);
				CREATE INDEX idx_views_channel_id_delete_at ON views(channelid,deleteat);
				CREATE TABLE propertyfields(id varchar(26), deleteat bigint, linkedfieldid varchar(26));
				CREATE INDEX idx_propertyfields_linkedfieldid ON propertyfields(linkedfieldid) WHERE linkedfieldid IS NOT NULL AND deleteat=0`)
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
			var beforeCount, expectedArchived int
			require.NoError(t, db.QueryRow(`SELECT count(*),13 + count(*) FILTER (WHERE version=159 AND name='create_autotranslation_tables') FROM db_migrations`).Scan(&beforeCount, &expectedArchived))
			const schemaSnapshot = `SELECT string_agg(indexname || ':' || indexdef,',' ORDER BY indexname) FROM pg_indexes WHERE schemaname=current_schema()`
			var indexesBefore, indexesAfter string
			require.NoError(t, db.QueryRow(schemaSnapshot).Scan(&indexesBefore))
			changed, err := ReconcileLegacyVersions(context.Background(), db)
			if tc.errorText != "" {
				require.ErrorContains(t, err, tc.errorText)
				require.False(t, changed)
				require.NoError(t, db.QueryRow(snapshot).Scan(&after))
				require.Equal(t, before, after)
				require.NoError(t, db.QueryRow(schemaSnapshot).Scan(&indexesAfter))
				require.Equal(t, indexesBefore, indexesAfter, "failed reconciliation rolls back index creation as well as history")
				var noArchive bool
				require.NoError(t, db.QueryRow(`SELECT to_regclass('db_migrations_legacy_versions') IS NULL`).Scan(&noArchive))
				require.True(t, noArchive)
				return
			}
			require.NoError(t, err)
			require.True(t, changed, "the November dump must be reconciled before Morph")
			require.NoError(t, database_migration.CheckDatabase(db))
			var archived, active int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM db_migrations_legacy_versions`).Scan(&archived))
			require.Equal(t, expectedArchived, archived)
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM db_migrations`).Scan(&active))
			require.Equal(t, beforeCount-archived+len(tc.addedCore), active)
			addedNames := make([]string, 0, len(tc.addedCore))
			for _, version := range tc.addedCore {
				addedNames = append(addedNames, expected[version])
			}
			var namesBefore, namesAfter string
			require.NoError(t, db.QueryRow(`SELECT string_agg(name,',' ORDER BY name) FROM (SELECT value->>'name' AS name FROM jsonb_array_elements($1::jsonb) UNION ALL SELECT unnest($2::text[])) m`, before, pq.Array(addedNames)).Scan(&namesBefore))
			require.NoError(t, db.QueryRow(`SELECT string_agg(name,',' ORDER BY name) FROM (SELECT name FROM db_migrations UNION ALL SELECT name FROM db_migrations_legacy_versions) m`).Scan(&namesAfter))
			require.Equal(t, namesBefore, namesAfter, "preserve every original migration identity, including the duplicate name")
			var indexes int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname IN ('idx_views_channel_id_delete_at','idx_propertyfields_linkedfieldid')`).Scan(&indexes))
			require.Equal(t, 2, indexes, "missing core indexes must be created, not just recorded")
			migrator := historyMigrator(t, db)
			require.NoError(t, migrator.MigrateWithPlan(propertyGroupsVersionPlan(t, migrator), false))
			var version int
			require.NoError(t, db.QueryRow(`SELECT version FROM propertygroups WHERE id='existing'`).Scan(&version))
			require.Equal(t, 1, version)
			require.NoError(t, db.QueryRow(snapshot).Scan(&before))
			changed, err = ReconcileLegacyVersions(context.Background(), db)
			require.NoError(t, err)
			require.False(t, changed)
			require.NoError(t, db.QueryRow(snapshot).Scan(&after))
			require.Equal(t, before, after)
		})
	}
}
