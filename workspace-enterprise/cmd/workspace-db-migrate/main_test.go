package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/enterprise/database_migration"
	coredb "github.com/mattermost/mattermost/server/v8/channels/db"
	"github.com/stretchr/testify/require"
)

func isolatedMigrationDB(t *testing.T) (*sql.DB, string, string) {
	t.Helper()
	dsn := os.Getenv("WORKSPACE_LICENSE_MIGRATION_TEST_DSN")
	if dsn == "" {
		t.Skip("requires disposable local PostgreSQL cluster")
	}
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })
	var local bool
	var dir string
	require.NoError(t, admin.QueryRow(`SELECT inet_server_addr() IS NULL,current_setting('data_directory')`).Scan(&local, &dir))
	require.True(t, local && strings.HasPrefix(dir, "/tmp/workspace-license-migration-"), "refusing destructive tests outside a disposable local cluster")
	name := fmt.Sprintf("mm_upgrade_rehearsal_%d", time.Now().UnixNano())
	_, err = admin.Exec("CREATE DATABASE " + pq.QuoteIdentifier(name))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec("DROP DATABASE " + pq.QuoteIdentifier(name) + " WITH (FORCE)")
		require.NoError(t, err)
	})
	db, err := sql.Open("postgres", dsn+" dbname="+name)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	config := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(map[string]any{"SqlSettings": map[string]any{"DriverName": "postgres", "DataSource": dsn + " dbname=" + name}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(config, data, 0600))
	return db, config, name
}

func execSQL(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	_, err := db.Exec(query)
	require.NoError(t, err)
}

func readSchema(t *testing.T, db *sql.DB) database_migration.SchemaContract {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback()
	c, err := database_migration.CaptureSchemaContract(context.Background(), tx, "public")
	require.NoError(t, err)
	return c
}

func preservedRows(t *testing.T, db *sql.DB) string {
	t.Helper()
	var data string
	require.NoError(t, db.QueryRow(`SELECT jsonb_build_object(
		'users',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM users t),
		'licenses',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM licenses t),
		'fields',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM propertyfields t),
		'values',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM propertyvalues t),
		'files',(SELECT jsonb_agg(to_jsonb(t)-ARRAY['thumbnailpath','previewpath','extension','mimetype'] ORDER BY id) FROM fileinfo t)
	)::text`).Scan(&data))
	return data
}

func TestFullMigrateRestoreAndRepeat(t *testing.T) {
	db, config, name := isolatedMigrationDB(t)
	require.NoError(t, migrate(config, false, name))
	contract, err := database_migration.LoadSchemaContract()
	require.NoError(t, err)
	actual := readSchema(t, db)
	require.Equal(t, contract.Columns, actual.Columns, "reference must match a fresh core migration")
	require.Equal(t, contract.Indexes, actual.Indexes)
	require.Equal(t, contract.Constraints, actual.Constraints)
	require.NoError(t, migrate(config, false, name))
	require.Equal(t, actual, readSchema(t, db), "repeat must leave the schema unchanged")

	// Reproduce all 72 missing keys from the actual pre-repair audit.
	data, err := os.ReadFile("testdata/missing_keys_20261015.json")
	require.NoError(t, err)
	var missing []struct{ Table, Name string }
	require.NoError(t, json.Unmarshal(data, &missing))
	require.Len(t, missing, 72)
	for _, i := range missing {
		var constraint string
		err := db.QueryRow(`SELECT conname FROM pg_constraint WHERE conindid=to_regclass($1) AND contype IN ('p','u')`, i.Name).Scan(&constraint)
		if err == sql.ErrNoRows {
			execSQL(t, db, "DROP INDEX "+pq.QuoteIdentifier(i.Name))
		} else {
			require.NoError(t, err)
			execSQL(t, db, "ALTER TABLE "+pq.QuoteIdentifier(i.Table)+" DROP CONSTRAINT "+pq.QuoteIdentifier(constraint)+" CASCADE")
		}
	}
	execSQL(t, db, `ALTER TABLE db_migrations DROP CONSTRAINT db_migrations_pkey;
		TRUNCATE db_migrations; ALTER TABLE users DROP CONSTRAINT users_email_key;
		CREATE UNIQUE INDEX idx_users_email ON users(email) WHERE email<>'';
		ALTER INDEX users_username_key RENAME TO idx_users_username;
		DROP INDEX idx_posts_create_at;
		INSERT INTO users(id,username,email) VALUES('user1','first',''),('user2','second','');
		INSERT INTO licenses(id,bytes) VALUES('license1','preserved');
		INSERT INTO propertyfields(id,groupid,name,createat,updateat,deleteat,objecttype) VALUES('field1','access_control','classification',1,1,0,'');
		INSERT INTO propertyvalues(id,groupid,fieldid,targetid,targettype,createat,updateat,deleteat,value) VALUES('value1','access_control','field1','','system',1,1,0,'"value"');
		INSERT INTO fileinfo(id,path,name,size,thumbnailpath,previewpath,extension,mimetype) VALUES('file1','original/path','العربية.pdf',12345,NULL,NULL,NULL,NULL)`)
	// Exact restored 240-row history; do not simulate it by version alone.
	data, err = os.ReadFile("../../schema_migration/testdata/sofa_history_20261015.json")
	require.NoError(t, err)
	var history []struct{ Version, Name string }
	require.NoError(t, json.Unmarshal(data, &history))
	for _, h := range history {
		_, err := db.Exec(`INSERT INTO db_migrations VALUES($1,$2)`, h.Version, h.Name)
		require.NoError(t, err)
	}
	before := preservedRows(t, db)
	require.NoError(t, migrate(config, false, name))
	require.Equal(t, before, preservedRows(t, db), "business records and file paths must survive")
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM db_migrations`).Scan(&count))
	expectedHistory, err := database_migration.Read(coredb.Assets())
	require.NoError(t, err)
	require.Len(t, expectedHistory, count, "all migrations from this binary must be recorded after legacy reconciliation")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM db_migrations_legacy_versions`).Scan(&count))
	require.Equal(t, 13, count)
	var thumbnail string
	require.NoError(t, db.QueryRow(`SELECT thumbnailpath FROM fileinfo WHERE id='file1'`).Scan(&thumbnail))
	require.Empty(t, thumbnail)
	// Execute representative upserts which caused real 500 responses.
	execSQL(t, db, `INSERT INTO licenses(id,bytes) VALUES('license1','replacement') ON CONFLICT(id) DO NOTHING;
		INSERT INTO propertyvalues(id,groupid,fieldid,targetid,targettype,createat,updateat,deleteat,value) VALUES('value2','access_control','field1','','system',1,1,0,'"value"')
		ON CONFLICT(groupid,targetid,fieldid) WHERE deleteat=0 DO UPDATE SET value=excluded.value`)
	require.Equal(t, before, preservedRows(t, db))
	repaired := readSchema(t, db)
	require.NoError(t, migrate(config, false, name))
	require.Equal(t, repaired, readSchema(t, db))
	require.Equal(t, before, preservedRows(t, db))
	report, err := database_migration.RepairSchema(context.Background(), db)
	require.NoError(t, err)
	require.Empty(t, report.Created)
}

func TestSchemaRepairRejectsConflictsAtomically(t *testing.T) {
	for _, tc := range []struct{ name, sql, expected string }{
		{"duplicate IDs", `ALTER TABLE licenses DROP CONSTRAINT licenses_pkey; INSERT INTO licenses(id,bytes) VALUES('duplicate','a'),('duplicate','b')`, "licenses_pkey"},
		{"NULL primary key", `ALTER TABLE licenses DROP CONSTRAINT licenses_pkey; ALTER TABLE licenses ALTER COLUMN id DROP NOT NULL; INSERT INTO licenses(id,bytes) VALUES(NULL,'a')`, "licenses_pkey"},
		{"wrong index definition", `DROP INDEX idx_propertyvalues_unique; CREATE INDEX idx_propertyvalues_unique ON propertyvalues(id)`, "conflicts"},
		{"missing column", `ALTER TABLE teams RENAME COLUMN description TO missing_description`, "schema mismatch"},
		{"deferrable key", `ALTER TABLE licenses DROP CONSTRAINT licenses_pkey; ALTER TABLE licenses ADD CONSTRAINT licenses_pkey PRIMARY KEY(id) DEFERRABLE`, "deferrable"},
		{"duplicate active property values", `DROP INDEX idx_propertyvalues_unique; INSERT INTO propertyvalues(id,targetid,targettype,groupid,fieldid,value,createat,updateat,deleteat) VALUES('a','','system','access_control','field','"a"',1,1,0),('b','','system','access_control','field','"b"',1,1,0)`, "idx_propertyvalues_unique"},
		{"orphan foreign key", `ALTER TABLE retentionpolicieschannels DROP CONSTRAINT fk_retentionpolicieschannels_retentionpolicies; INSERT INTO retentionpolicieschannels(policyid,channelid) VALUES('missing-policy','channel')`, "fk_retentionpolicieschannels_retentionpolicies"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, config, name := isolatedMigrationDB(t)
			require.NoError(t, migrate(config, false, name))
			execSQL(t, db, `ALTER TABLE bots DROP CONSTRAINT bots_pkey`)
			execSQL(t, db, tc.sql)
			before := readSchema(t, db)
			rows := preservedRows(t, db)
			_, err := database_migration.RepairSchema(context.Background(), db)
			require.ErrorContains(t, err, tc.expected)
			require.Equal(t, before, readSchema(t, db), "a later error must roll back earlier index creations")
			require.Equal(t, rows, preservedRows(t, db))
		})
	}
}

func TestMigrateGuards(t *testing.T) {
	db, config, name := isolatedMigrationDB(t)
	require.ErrorContains(t, migrate(config, false, "other_database"), "database mismatch")
	var tables int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_tables WHERE schemaname='public'`).Scan(&tables))
	require.Zero(t, tables)
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(context.Background(), `SELECT pg_advisory_lock(hashtext(current_database()),hashtext('workspace-db-migrate'))`)
	require.NoError(t, err)
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock_all()`)
	require.ErrorContains(t, migrate(config, false, name), "already running")
	execSQL(t, db, `CREATE TABLE unrelated_data(id int);INSERT INTO unrelated_data VALUES(1)`)
	_, err = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock_all()`)
	require.NoError(t, err)
	require.ErrorContains(t, migrate(config, false, name), "no db_migrations")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM unrelated_data`).Scan(&tables))
	require.Equal(t, 1, tables)
}
