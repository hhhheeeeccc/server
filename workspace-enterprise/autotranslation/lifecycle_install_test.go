package autotranslation

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func lifecycleSchema(t *testing.T) (*sql.DB, func() *sql.DB, string) {
	t.Helper()
	th := setupTranslationLifecycle(t)
	schema := "workspace_translation_" + model.NewId()
	quoted := pq.QuoteIdentifier(schema)
	open := func() *sql.DB {
		connector, err := pq.NewConnector(*th.App.Config().SqlSettings.DataSource)
		require.NoError(t, err)
		db := sql.OpenDB(connector)
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		_, err = db.Exec(`SET search_path=` + quoted + `,pg_catalog`)
		require.NoError(t, err)
		return db
	}
	db := open()
	_, err := db.Exec(`CREATE SCHEMA ` + quoted)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec(`DROP SCHEMA ` + quoted + ` CASCADE`)
		require.NoError(t, err)
	})
	_, err = db.Exec(`CREATE TABLE posts(id text PRIMARY KEY,deleteat bigint NOT NULL DEFAULT 0,message text NOT NULL DEFAULT '',updateat bigint NOT NULL DEFAULT 0);
		CREATE TABLE translations(objectid text NOT NULL,objecttype text NOT NULL,dstlang text NOT NULL DEFAULT 'ar',
		text text NOT NULL,PRIMARY KEY(objectid,objecttype,dstlang));
		INSERT INTO posts(id,deleteat) VALUES ('live',0),('deleted',1);
		INSERT INTO translations(objectid,objecttype,text) VALUES
		('live','post','keep live'),('deleted','post','remove deleted'),('missing','post','remove orphan'),
		('missing','other_object','keep other'),('deleted','other_object','keep other deleted')`)
	require.NoError(t, err)
	return db, open, quoted
}

func TestTranslationLifecycleInstallCleansLegacyRowsAndIsRepeatable(t *testing.T) {
	db, _, quoted := lifecycleSchema(t)
	for range 2 {
		require.NoError(t, InstallPostLifecycle(context.Background(), db))
		var posts, others, triggers int
		require.NoError(t, db.QueryRow(`SELECT count(*) FILTER(WHERE objecttype='post'),
			count(*) FILTER(WHERE objecttype='other_object') FROM translations`).Scan(&posts, &others))
		require.Equal(t, 1, posts)
		require.Equal(t, 2, others)
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_trigger WHERE tgrelid IN ('posts'::regclass,'translations'::regclass)
			AND NOT tgisinternal AND tgenabled='O'`).Scan(&triggers))
		require.Equal(t, 4, triggers)
	}
	_, err := db.Exec(`SET search_path=pg_catalog`)
	require.NoError(t, err)
	// Persistent trigger references must not depend on the caller's search path.
	_, err = db.Exec(`INSERT INTO ` + quoted + `.translations(objectid,objecttype,text) VALUES ('deleted','post','late')`)
	var pgErr *pq.Error
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, pq.ErrorCode("23503"), pgErr.Code)
	_, err = db.Exec(`INSERT INTO ` + quoted + `.translations(objectid,objecttype,text) VALUES ('missing','post','late')`)
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, pq.ErrorCode("23503"), pgErr.Code)
	_, err = db.Exec(`UPDATE ` + quoted + `.translations SET objecttype='other_object' WHERE objecttype='post'`)
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, pq.ErrorCode("23514"), pgErr.Code)
	_, err = db.Exec(`TRUNCATE ` + quoted + `.posts`)
	require.NoError(t, err)
	var posts, others int
	require.NoError(t, db.QueryRow(`SELECT count(*) FILTER(WHERE objecttype='post'),
		count(*) FILTER(WHERE objecttype='other_object') FROM `+quoted+`.translations`).Scan(&posts, &others))
	require.Zero(t, posts)
	require.Equal(t, 2, others)
}

func TestTranslationLifecycleInstallRollsBackPartialDDL(t *testing.T) {
	db, _, _ := lifecycleSchema(t)
	_, err := db.Exec(`CREATE FUNCTION workspace_truncate_post_translations() RETURNS integer LANGUAGE sql AS 'SELECT 1'`)
	require.NoError(t, err)
	require.Error(t, InstallPostLifecycle(context.Background(), db))
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM translations`).Scan(&count))
	require.Equal(t, 5, count, "failed installation must not remove existing rows")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_proc WHERE pronamespace=current_schema()::regnamespace
		AND proname IN ('workspace_translation_source_guard','workspace_delete_post_translations')`).Scan(&count))
	require.Zero(t, count, "earlier function DDL must roll back too")
	_, err = db.Exec(`DROP FUNCTION workspace_truncate_post_translations()`)
	require.NoError(t, err)
	require.NoError(t, InstallPostLifecycle(context.Background(), db))
}

func TestTranslationLifecycleInstallHonorsDeadlineAndConcurrentNodes(t *testing.T) {
	db, open, _ := lifecycleSchema(t)
	other := open()
	holder, err := db.Begin()
	require.NoError(t, err)
	defer holder.Rollback()
	_, err = holder.Exec(`LOCK TABLE posts IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	require.Error(t, InstallPostLifecycle(ctx, other))
	require.NoError(t, holder.Rollback())
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM translations`).Scan(&count))
	require.Equal(t, 5, count)
	finished := make(chan error, 2)
	for _, node := range []*sql.DB{db, other} {
		go func() { finished <- InstallPostLifecycle(context.Background(), node) }()
	}
	for range 2 {
		select {
		case err = <-finished:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent lifecycle installations did not finish")
		}
	}
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM translations`).Scan(&count))
	require.Equal(t, 3, count)
}
