package file_metadata_repair

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

func testDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("WORKSPACE_FILE_METADATA_TEST_DSN")
	if dsn == "" {
		t.Skip("set WORKSPACE_FILE_METADATA_TEST_DSN to an isolated local test cluster")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	var local bool
	var directory, database string
	require.NoError(t, db.QueryRow(`SELECT inet_server_addr() IS NULL,current_setting('data_directory'),current_database()`).Scan(&local, &directory, &database))
	if !local || !(strings.HasPrefix(directory, "/tmp/workspace-file-metadata-") || strings.HasPrefix(directory, "/tmp/workspace-license-migration-")) {
		t.Fatal("refusing tests outside a disposable local workspace-file-metadata cluster")
	}
	schema := fmt.Sprintf("metadata_%d", time.Now().UnixNano())
	_, err = db.Exec("CREATE SCHEMA " + pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec("DROP SCHEMA " + pq.QuoteIdentifier(schema) + " CASCADE")
		require.NoError(t, err)
	})
	target, err := sql.Open("postgres", dsn+" search_path="+schema)
	require.NoError(t, err)
	t.Cleanup(func() { target.Close() })
	return target, database
}

const fixture = `CREATE TABLE fileinfo(id varchar(26),postid text,path text,name text,size bigint,
	thumbnailpath text,previewpath text,extension text,mimetype text,updateat bigint);
	INSERT INTO fileinfo VALUES('restored','post-a','original/path','العربي.pdf',123,NULL,NULL,NULL,NULL,12345),
	('healthy','post-b','another/path','image.png',456,'thumb.png','preview.png','png','image/png',45678),
	('partial','post-c','third/path','archive.zip',789,'existing-thumb',NULL,'zip','application/zip',99999)`

func snapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var result string
	require.NoError(t, db.QueryRow(`SELECT jsonb_agg(to_jsonb(f) ORDER BY id)::text FROM fileinfo f`).Scan(&result))
	return result
}

func TestInspectRepairRepeatAndRollback(t *testing.T) {
	db, name := testDB(t)
	_, err := db.Exec(fixture)
	require.NoError(t, err)
	before := snapshot(t, db)
	// Reproduce the exact string scan failure before repairing.
	var thumbnail string
	require.ErrorContains(t, db.QueryRow(`SELECT thumbnailpath FROM fileinfo WHERE id='restored'`).Scan(&thumbnail), "converting NULL to string")
	ctx := context.Background()
	report, err := Run(ctx, db, name, false)
	require.NoError(t, err)
	require.EqualValues(t, 2, report.Affected)
	require.EqualValues(t, 1, report.ThumbnailPath)
	require.EqualValues(t, 2, report.PreviewPath)
	require.False(t, report.Applied)
	require.Empty(t, report.BackupTable)
	require.Equal(t, before, snapshot(t, db))
	report, err = Run(ctx, db, name, true)
	require.NoError(t, err)
	require.True(t, report.Applied)
	require.NotEmpty(t, report.BackupTable)
	var archived int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM "+report.BackupTable).Scan(&archived))
	require.Equal(t, 2, archived)
	var preview, extension, mime string
	require.NoError(t, db.QueryRow(`SELECT thumbnailpath,previewpath,extension,mimetype FROM fileinfo WHERE id='restored'`).Scan(&thumbnail, &preview, &extension, &mime))
	require.Equal(t, []string{"", "", "", ""}, []string{thumbnail, preview, extension, mime})
	var unchanged string
	require.NoError(t, db.QueryRow(`SELECT thumbnailpath FROM fileinfo WHERE id='partial'`).Scan(&unchanged))
	require.Equal(t, "existing-thumb", unchanged)
	// Full-row JSON comparison after rollback proves IDs, bytes paths, names,
	// sizes, post links, timestamps, and healthy metadata were preserved.
	second, err := Run(ctx, db, name, true)
	require.NoError(t, err)
	require.Zero(t, second.Affected)
	require.False(t, second.Applied)
	_, err = db.Exec(report.RollbackSQL)
	require.NoError(t, err)
	require.Equal(t, before, snapshot(t, db))
}

func TestRefusesWrongDatabaseAndInvalidIDs(t *testing.T) {
	for _, tc := range []struct{ name, extra, expected string }{
		{"wrong database", "", "different_database"},
		{"duplicate ID", `INSERT INTO fileinfo(id) VALUES('restored')`, ""},
		{"null ID", `INSERT INTO fileinfo(id) VALUES(NULL)`, ""},
		{"update constraint", `ALTER TABLE fileinfo ADD CHECK(thumbnailpath IS NULL OR thumbnailpath<>'')`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, name := testDB(t)
			_, err := db.Exec(fixture + ";" + tc.extra)
			require.NoError(t, err)
			before := snapshot(t, db)
			if tc.expected != "" {
				name = tc.expected
			}
			_, err = Run(context.Background(), db, name, true)
			require.Error(t, err)
			require.Equal(t, before, snapshot(t, db))
			var archives int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_tables WHERE schemaname=current_schema() AND tablename LIKE 'workspace_fileinfo_nulls_%'`).Scan(&archives))
			require.Zero(t, archives, "failed repair must roll back the archive too")
		})
	}
}

func TestRollbackPreservesNewerMetadata(t *testing.T) {
	db, name := testDB(t)
	_, err := db.Exec(fixture)
	require.NoError(t, err)
	report, err := Run(context.Background(), db, name, true)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE fileinfo SET thumbnailpath='new-thumbnail' WHERE id='restored'`)
	require.NoError(t, err)
	result, err := db.Exec(report.RollbackSQL)
	require.NoError(t, err)
	n, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	var thumbnail string
	require.NoError(t, db.QueryRow(`SELECT thumbnailpath FROM fileinfo WHERE id='restored'`).Scan(&thumbnail))
	require.Equal(t, "new-thumbnail", thumbnail)
}
