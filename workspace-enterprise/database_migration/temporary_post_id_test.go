package database_migration

import (
	"context"
	"database/sql"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func temporaryPostTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db := licenseTestDatabase(t)
	_, err := db.Exec(`CREATE TABLE posts(id varchar(26) PRIMARY KEY,message text,type varchar(26));
 CREATE TABLE temporaryposts(postid varchar(26),type varchar(26) NOT NULL,expireat bigint NOT NULL,message varchar(65535),fileids varchar(300));
 CREATE TABLE readreceipts(postid varchar(26),userid varchar(26),expireat bigint);
 INSERT INTO posts VALUES ('normal','Ordinary message',''),('existing','','burn_on_read'),('expired','','burn_on_read');
 INSERT INTO temporaryposts VALUES ('existing','burn_on_read',900000,'Preserve existing content','["file-one"]'),('expired','burn_on_read',1,'Expired content','[]');
 INSERT INTO readreceipts VALUES ('existing','reader',800000)`)
	require.NoError(t, err)
	return db
}

func temporaryPostSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var snapshot string
	require.NoError(t, db.QueryRow(`SELECT jsonb_build_object(
 'posts',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM posts p),
 'temporaryposts',(SELECT jsonb_agg(to_jsonb(p) ORDER BY postid) FROM temporaryposts p),
 'readreceipts',(SELECT jsonb_agg(to_jsonb(r) ORDER BY postid,userid) FROM readreceipts r))::text`).Scan(&snapshot))
	return snapshot
}

// Matches the insert and ON CONFLICT target in SqlTemporaryPostStore.saveT.
const temporaryPostUpsert = `INSERT INTO temporaryposts(postid,type,expireat,message,fileids)
 VALUES ($1,'burn_on_read',1000000,'New content','["new-file"]')
 ON CONFLICT(postid) DO UPDATE SET type=EXCLUDED.type,expireat=EXCLUDED.expireat,
 message=EXCLUDED.message,fileids=EXCLUDED.fileids`

func saveTemporaryPostTransactionForTest(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO posts VALUES ('new','','burn_on_read')`); err != nil {
		return err
	}
	if _, err = tx.Exec(temporaryPostUpsert, "new"); err != nil {
		return err
	}
	return tx.Commit()
}

func TestRepairTemporaryPostIDRestoresSave(t *testing.T) {
	for _, tc := range []struct {
		name, ddl string
		changed   bool
	}{
		{name: "missing key", changed: true},
		{name: "existing primary key", ddl: `ALTER TABLE temporaryposts ADD PRIMARY KEY(postid)`},
		{name: "renamed unique index with include", ddl: `CREATE UNIQUE INDEX custom_temporary_posts ON temporaryposts(postid) INCLUDE(expireat)`},
		{name: "partial index insufficient", ddl: `CREATE UNIQUE INDEX partial_temporary_posts ON temporaryposts(postid) WHERE expireat>0`, changed: true},
		{name: "composite index insufficient", ddl: `CREATE UNIQUE INDEX composite_temporary_posts ON temporaryposts(postid,type)`, changed: true},
		{name: "another primary key", ddl: `ALTER TABLE temporaryposts ADD PRIMARY KEY(expireat,postid)`, changed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := temporaryPostTestDatabase(t)
			if tc.ddl != "" {
				_, err := db.Exec(tc.ddl)
				require.NoError(t, err)
			}
			before := temporaryPostSnapshot(t, db)
			if tc.changed {
				err := saveTemporaryPostTransactionForTest(db)
				var pgErr *pq.Error
				require.ErrorAs(t, err, &pgErr)
				require.Equal(t, pq.ErrorCode("42P10"), pgErr.Code)
				require.Equal(t, before, temporaryPostSnapshot(t, db), "failed save must not leave a partial post")
			}
			changed, err := RepairTemporaryPostID(context.Background(), db)
			require.NoError(t, err)
			require.Equal(t, tc.changed, changed)
			require.Equal(t, before, temporaryPostSnapshot(t, db), "content, file IDs, expiry and read receipts must be preserved")
			changed, err = RepairTemporaryPostID(context.Background(), db)
			require.NoError(t, err)
			require.False(t, changed)
			require.NoError(t, saveTemporaryPostTransactionForTest(db))
			_, err = db.Exec(temporaryPostUpsert, "existing")
			require.NoError(t, err)
			var matches bool
			require.NoError(t, db.QueryRow(`SELECT (SELECT count(*) FROM posts WHERE id='new')=1
 AND (SELECT count(*) FROM temporaryposts WHERE postid='existing')=1
 AND EXISTS(SELECT 1 FROM temporaryposts WHERE postid='existing' AND expireat=1000000 AND message='New content' AND fileids='["new-file"]')
 AND EXISTS(SELECT 1 FROM posts WHERE id='normal' AND message='Ordinary message')
 AND EXISTS(SELECT 1 FROM readreceipts WHERE postid='existing' AND userid='reader' AND expireat=800000)
 AND EXISTS(SELECT 1 FROM temporaryposts WHERE postid='expired' AND expireat=1 AND message='Expired content')`).Scan(&matches))
			require.True(t, matches)
		})
	}
}

func TestRepairTemporaryPostIDRejectsConflicts(t *testing.T) {
	for _, tc := range []struct{ name, ddl, errorText string }{
		{"duplicates", `INSERT INTO temporaryposts SELECT * FROM temporaryposts WHERE postid='existing'`, "duplicates (true)"},
		{"null ID", `UPDATE temporaryposts SET postid=NULL WHERE postid='existing'`, "NULLs (true)"},
		{"deferred key", `ALTER TABLE temporaryposts ADD UNIQUE(postid) DEFERRABLE`, "deferrable"},
		{"constraint collision", `ALTER TABLE temporaryposts ADD CONSTRAINT temporaryposts_pkey CHECK(expireat>=0)`, "restore temporary post ID constraint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := temporaryPostTestDatabase(t)
			_, err := db.Exec(tc.ddl)
			require.NoError(t, err)
			before := temporaryPostSnapshot(t, db)
			changed, err := RepairTemporaryPostID(context.Background(), db)
			require.ErrorContains(t, err, tc.errorText)
			require.False(t, changed)
			require.Equal(t, before, temporaryPostSnapshot(t, db))
			var newKey bool
			require.NoError(t, db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_index WHERE indrelid='temporaryposts'::regclass AND indisunique AND indimmediate)`).Scan(&newKey))
			require.False(t, newKey)
			_, err = db.Exec(`UPDATE temporaryposts SET expireat=expireat`)
			require.NoError(t, err, "failed repair must release its lock")
		})
	}
}

func TestRepairTemporaryPostIDMissingTable(t *testing.T) {
	db := licenseTestDatabase(t)
	changed, err := RepairTemporaryPostID(context.Background(), db)
	require.ErrorContains(t, err, "resolve temporaryposts table")
	require.False(t, changed)
}
