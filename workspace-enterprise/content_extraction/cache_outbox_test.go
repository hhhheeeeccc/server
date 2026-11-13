package content_extraction

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestExtractionCacheOldAcknowledgementPreservesNewIntent(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_POSTGRESQL_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL; use the audit runner")
	}
	// A temporary table keeps this protocol test independent of App schemas.
	// The same physical session reads a pending version, sees another commit,
	// then acknowledges its older invalidation using production SQL.
	session := newDatabaseSession(dsn, "workspace_extraction_cache_version_test", time.Second)
	session.readOnly = false
	defer session.Close()
	ctx := context.Background()
	old := cacheIntent{postID: model.NewId(), version: model.NewId()}
	newVersion := model.NewId()
	err := session.query(ctx, func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, `CREATE TEMP TABLE WorkspaceExtractionCacheOutbox (PostId varchar(26) PRIMARY KEY, Version varchar(26) NOT NULL)`)
		if err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, `INSERT INTO WorkspaceExtractionCacheOutbox (PostId,Version) VALUES ($1,$2)`, old.postID, old.version)
		if err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, `UPDATE WorkspaceExtractionCacheOutbox SET Version=$2 WHERE PostId=$1`, old.postID, newVersion)
		return err
	})
	require.NoError(t, err)
	require.NoError(t, session.ackCacheIntent(ctx, old))
	var stored string
	require.NoError(t, session.query(ctx, func(ctx context.Context, db *sql.DB) error {
		return db.QueryRowContext(ctx, `SELECT Version FROM WorkspaceExtractionCacheOutbox WHERE PostId=$1`, old.postID).Scan(&stored)
	}))
	require.Equal(t, newVersion, stored, "an old invalidation cannot acknowledge a newer content commit")
	require.NoError(t, session.ackCacheIntent(ctx, cacheIntent{postID: old.postID, version: newVersion}))
	var remaining int
	require.NoError(t, session.query(ctx, func(ctx context.Context, db *sql.DB) error {
		return db.QueryRowContext(ctx, `SELECT count(*) FROM WorkspaceExtractionCacheOutbox`).Scan(&remaining)
	}))
	require.Zero(t, remaining)
}
