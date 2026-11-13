package partial_search

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func searchIndexDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_POSTGRESQL_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	schema := "workspace_search_" + model.NewId()
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.Exec(`CREATE SCHEMA ` + pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec(`DROP SCHEMA ` + pq.QuoteIdentifier(schema) + ` CASCADE`)
		require.NoError(t, err)
	})
	_, err = db.Exec(`CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;
		CREATE TABLE posts(id integer PRIMARY KEY,message text);
		INSERT INTO posts VALUES (1,'first message'),(2,'second message')`)
	require.NoError(t, err)
	return db
}

func TestSearchIndexBuildKeepsMessageWritesAvailable(t *testing.T) {
	db := searchIndexDatabase(t)
	writer, err := db.Begin()
	require.NoError(t, err)
	defer writer.Rollback()
	_, err = writer.Exec(`UPDATE posts SET message='uncommitted message' WHERE id=1`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ensurePostTrigramIndex(ctx, db) }()
	require.Eventually(t, func() bool {
		var present bool
		err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_locks
			WHERE relation=to_regclass('posts') AND mode IN ('ShareLock','ShareUpdateExclusiveLock'))`).Scan(&present)
		return err == nil && present
	}, 4*time.Second, 10*time.Millisecond, "the index builder must be waiting for the existing writer")
	writeCtx, stopWrite := context.WithTimeout(context.Background(), time.Second)
	defer stopWrite()
	_, writeErr := db.ExecContext(writeCtx, `UPDATE posts SET message='new message' WHERE id=2`)
	require.NoError(t, writer.Rollback())
	require.NoError(t, <-done)
	require.NoError(t, writeErr, "building a search index must not block unrelated message writes")
	var message string
	require.NoError(t, db.QueryRow(`SELECT message FROM posts WHERE id=2`).Scan(&message))
	require.Equal(t, "new message", message)
}

func searchIndexIdentity(t *testing.T, db *sql.DB) (int64, bool) {
	t.Helper()
	var oid int64
	var valid bool
	require.NoError(t, db.QueryRow(`SELECT indexrelid,indisvalid AND indisready FROM pg_index
		WHERE indexrelid=to_regclass($1)`, postTrigramIndexName).Scan(&oid, &valid))
	return oid, valid
}

func TestSearchIndexPreservesRowsAndReusesValidIndex(t *testing.T) {
	db := searchIndexDatabase(t)
	require.NoError(t, ensurePostTrigramIndex(context.Background(), db))
	oid, valid := searchIndexIdentity(t, db)
	require.True(t, valid)
	require.NoError(t, ensurePostTrigramIndex(context.Background(), db))
	secondOID, valid := searchIndexIdentity(t, db)
	require.Equal(t, oid, secondOID)
	require.True(t, valid)
	var messages []string
	rows, err := db.Query(`SELECT message FROM posts ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var message string
		require.NoError(t, rows.Scan(&message))
		messages = append(messages, message)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"first message", "second message"}, messages)
}

func TestSearchIndexRejectsConflictingDefinition(t *testing.T) {
	db := searchIndexDatabase(t)
	_, err := db.Exec(`CREATE INDEX idx_posts_message_trgm ON posts(id)`)
	require.NoError(t, err)
	oid, valid := searchIndexIdentity(t, db)
	require.True(t, valid)
	require.ErrorContains(t, ensurePostTrigramIndex(context.Background(), db), "unexpected definition; left unchanged")
	secondOID, valid := searchIndexIdentity(t, db)
	require.Equal(t, oid, secondOID)
	require.True(t, valid)
}

func TestSearchIndexRepairsInterruptedBuild(t *testing.T) {
	db := searchIndexDatabase(t)
	writer, err := db.Begin()
	require.NoError(t, err)
	defer writer.Rollback()
	_, err = writer.Exec(`UPDATE posts SET message='pending' WHERE id=1`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ensurePostTrigramIndex(ctx, db) }()
	require.Eventually(t, func() bool {
		var exists bool
		err := db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, postTrigramIndexName).Scan(&exists)
		return err == nil && exists
	}, 4*time.Second, 10*time.Millisecond)
	cancel()
	require.Error(t, <-done)
	require.NoError(t, writer.Rollback())
	oldOID, valid := searchIndexIdentity(t, db)
	require.False(t, valid)
	require.NoError(t, ensurePostTrigramIndex(context.Background(), db))
	newOID, valid := searchIndexIdentity(t, db)
	require.True(t, valid)
	require.NotEqual(t, oldOID, newOID)
}

func TestSearchIndexCoordinationDoesNotWaitOrLeakLocks(t *testing.T) {
	db := searchIndexDatabase(t)
	owner, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer owner.Close()
	_, err = owner.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, postTrigramIndexLock)
	require.NoError(t, err)
	defer owner.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, postTrigramIndexLock)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, ensurePostTrigramIndex(ctx, db))
	var absent bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass($1) IS NULL`, postTrigramIndexName).Scan(&absent))
	require.True(t, absent)
	_, err = owner.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, postTrigramIndexLock)
	require.NoError(t, err)
	require.NoError(t, ensurePostTrigramIndex(ctx, db))
	_, valid := searchIndexIdentity(t, db)
	require.True(t, valid)
}
