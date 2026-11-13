package schema_migration

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"

	"github.com/mattermost/enterprise/database_migration"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/store/sqlstore"
	"github.com/mattermost/morph/models"
	"github.com/stretchr/testify/require"
)

func legacyVersionKeyedDB(t *testing.T) *sql.DB {
	t.Helper()
	db := isolatedHistoryDB(t)
	_, err := db.Exec(`CREATE TABLE db_migrations(version bigint PRIMARY KEY, name varchar NOT NULL);
		CREATE TABLE propertygroups(id varchar(26) PRIMARY KEY, name varchar(64) NOT NULL);
		INSERT INTO propertygroups VALUES ('group1', 'Existing group')`)
	require.NoError(t, err)
	fixture, err := os.ReadFile("testdata/sofa_history_20261015.json")
	require.NoError(t, err)
	var history []struct{ Version, Name string }
	require.NoError(t, json.Unmarshal(fixture, &history))
	legacy := make(map[int]string)
	for _, entry := range append(append([]migrationVersion{}, branchOnly...), renumbered...) {
		legacy[entry.old] = entry.name
	}
	for _, row := range history {
		version, err := strconv.Atoi(row.Version)
		require.NoError(t, err)
		if name, ok := legacy[version]; ok && name != row.Name {
			continue
		}
		_, err = db.Exec(`INSERT INTO db_migrations VALUES ($1,$2)`, version, row.Name)
		require.NoError(t, err)
	}
	return db
}

func historySettings(t *testing.T, db *sql.DB) model.SqlSettings {
	t.Helper()
	var schema string
	require.NoError(t, db.QueryRow(`SELECT current_schema()`).Scan(&schema))
	settings := model.SqlSettings{}
	settings.SetDefaults(false)
	settings.DataSource = model.NewPointer(os.Getenv("WORKSPACE_LICENSE_MIGRATION_TEST_DSN") + " search_path=" + schema)
	return settings
}

func historyMigrator(t *testing.T, db *sql.DB) *sqlstore.Migrator {
	t.Helper()
	migrator, err := sqlstore.NewMigrator(historySettings(t, db), mlog.CreateConsoleTestLogger(t), false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, migrator.Close()) })
	return migrator
}

func propertyGroupsVersionPlan(t *testing.T, migrator *sqlstore.Migrator) *models.Plan {
	t.Helper()
	plan, err := migrator.GeneratePlan(false)
	require.NoError(t, err)
	for _, migration := range plan.Migrations {
		if migration.Name == "add_property_groups_version" {
			return models.NewPlan([]*models.Migration{migration}, nil, false)
		}
	}
	t.Fatal("property groups version migration must remain pending")
	return nil
}

func TestStartupReconcilesVersionPrimaryKey(t *testing.T) {
	db := legacyVersionKeyedDB(t)
	migrator := historyMigrator(t, db)
	// Reproduce the production failure with Morph's actual SQL and tracking.
	err := migrator.MigrateWithPlan(propertyGroupsVersionPlan(t, migrator), false)
	require.ErrorContains(t, err, `duplicate key value violates unique constraint "db_migrations_pkey"`)

	// SqlStore executes options before opening its own connection or applying
	// migrations. Stop after the Enterprise option to inspect that boundary.
	settings := historySettings(t, db)
	stop := errors.New("stop after startup history repair")
	_, err = sqlstore.New(settings, mlog.CreateConsoleTestLogger(t), nil,
		StartupOption(settings, mlog.CreateConsoleTestLogger(t)),
		func(*sqlstore.SqlStore) error { return stop })
	require.ErrorIs(t, err, stop)
	require.NoError(t, database_migration.CheckDatabase(db))
	var name string
	require.NoError(t, db.QueryRow(`SELECT name FROM db_migrations WHERE version=160`).Scan(&name))
	require.Equal(t, "add_user_tracking_to_properties", name)
	require.NoError(t, migrator.MigrateWithPlan(propertyGroupsVersionPlan(t, migrator), false))
	require.NoError(t, db.QueryRow(`SELECT name FROM db_migrations WHERE version=170`).Scan(&name))
	require.Equal(t, "add_property_groups_version", name)
	var version int
	require.NoError(t, db.QueryRow(`SELECT name, version FROM propertygroups WHERE id='group1'`).Scan(&name, &version))
	require.Equal(t, "Existing group", name)
	require.Equal(t, 1, version)
	// A subsequent startup must leave the reconciled history unchanged.
	var before, after string
	const snapshot = `SELECT jsonb_agg(to_jsonb(m) ORDER BY version,name)::text FROM db_migrations m`
	require.NoError(t, db.QueryRow(snapshot).Scan(&before))
	require.NoError(t, StartupOption(settings, mlog.CreateConsoleTestLogger(t))(nil))
	require.NoError(t, db.QueryRow(snapshot).Scan(&after))
	require.Equal(t, before, after)
}

func TestStartupRejectsUnknownLegacyHistory(t *testing.T) {
	db := legacyVersionKeyedDB(t)
	_, err := db.Exec(`UPDATE db_migrations SET name='unknown_migration' WHERE version=42`)
	require.NoError(t, err)
	var before, after string
	const snapshot = `SELECT jsonb_agg(to_jsonb(m) ORDER BY version,name)::text FROM db_migrations m`
	require.NoError(t, db.QueryRow(snapshot).Scan(&before))
	settings := historySettings(t, db)
	_, err = sqlstore.New(settings, mlog.CreateConsoleTestLogger(t), nil,
		StartupOption(settings, mlog.CreateConsoleTestLogger(t)))
	require.ErrorContains(t, err, "incompatible database migration history")
	require.NoError(t, db.QueryRow(snapshot).Scan(&after))
	require.Equal(t, before, after)
	var noArchive bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass('db_migrations_legacy_versions') IS NULL`).Scan(&noArchive))
	require.True(t, noArchive)
}

func TestStartupLeavesEmptyAndCurrentDatabasesUnchanged(t *testing.T) {
	for _, current := range []bool{false, true} {
		name := "empty database"
		if current {
			name = "current history"
		}
		t.Run(name, func(t *testing.T) {
			db := isolatedHistoryDB(t)
			if current {
				_, err := db.Exec(`CREATE TABLE db_migrations(version bigint PRIMARY KEY, name varchar NOT NULL);
					INSERT INTO db_migrations VALUES (170,'add_property_groups_version')`)
				require.NoError(t, err)
			}
			const snapshot = `SELECT string_agg(tablename,',' ORDER BY tablename) FROM pg_tables WHERE schemaname=current_schema()`
			var before, after sql.NullString
			require.NoError(t, db.QueryRow(snapshot).Scan(&before))
			require.NoError(t, StartupOption(historySettings(t, db), mlog.CreateConsoleTestLogger(t))(nil))
			require.NoError(t, db.QueryRow(snapshot).Scan(&after))
			require.Equal(t, before, after)
			if current {
				var name string
				require.NoError(t, db.QueryRow(`SELECT name FROM db_migrations WHERE version=170`).Scan(&name))
				require.Equal(t, "add_property_groups_version", name)
			}
		})
	}
}
