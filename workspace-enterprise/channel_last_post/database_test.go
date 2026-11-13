package channel_last_post

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

// The verifier supplies a disposable cluster; never fall back to an application DSN.
func previewTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("WORKSPACE_PREVIEW_TEST_DSN")
	if dsn == "" {
		t.Skip("set WORKSPACE_PREVIEW_TEST_DSN to an isolated PostgreSQL cluster")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	schema := "preview_" + model.NewId()
	_, err = db.Exec(`CREATE SCHEMA ` + pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec(`DROP SCHEMA ` + pq.QuoteIdentifier(schema) + ` CASCADE`)
		require.NoError(t, err)
	})
	_, err = db.Exec(`SET search_path TO ` + pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	return db
}

func TestLatestPostCandidatesQueryPostgres(t *testing.T) {
	db := previewTestDB(t)
	_, err := db.Exec(`CREATE TABLE posts (
		id varchar PRIMARY KEY, channelid varchar, createat bigint, deleteat bigint DEFAULT 0,
		updateat bigint DEFAULT 0, editat bigint DEFAULT 0, ispinned boolean DEFAULT false,
		userid varchar DEFAULT '', rootid varchar DEFAULT '', originalid varchar DEFAULT '',
		message text DEFAULT '', type varchar DEFAULT '', props jsonb DEFAULT '{}',
		hashtags text DEFAULT '', fileids jsonb DEFAULT '[]', hasreactions boolean DEFAULT false, remoteid varchar);
		CREATE INDEX ON posts(channelid,deleteat,createat);
		INSERT INTO posts(id,channelid,createat,type,deleteat,message) VALUES
		('old','one',1,'',0,'old'),
		('tie-a','one',2,'',0,'earlier ID'),
		('tie-z','one',2,'',0,'newest visible'),
		('system','one',3,'system_join_channel',0,'hidden'),
		('deleted','one',4,'',5,'deleted'),
		('root-two','two',0,'',0,'main'),
		('other','two',1,'',0,'reply'),
		('only-reply','replies-only',2,'',0,'reply'),
		('only-system','empty',9,'system_leave_channel',0,'hidden'),
		('unrequested','private',100,'',0,'must not return');
		UPDATE posts SET rootid='thread',props='{"key":"value"}',fileids='["file"]',remoteid='remote'
		WHERE id IN ('other','only-reply')`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, latestPostsQuery, pq.Array([]string{"one", "two", "replies-only", "empty", "absent"}))
	require.NoError(t, err)
	defer rows.Close()
	posts := map[string]*model.Post{}
	for rows.Next() {
		var post model.Post
		require.NoError(t, rows.Scan(&post.Id, &post.ChannelId, &post.RootId))
		posts[post.ChannelId] = &post
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, posts, 2)
	require.Equal(t, "tie-z", posts["one"].Id)
	require.Equal(t, "root-two", posts["two"].Id)
	require.Empty(t, posts["two"].RootId)
	require.NotContains(t, posts, "replies-only")

	// Deleting the latest main message selects the previous main message.
	_, err = db.Exec(`UPDATE posts SET deleteat=10 WHERE id='tie-z'`)
	require.NoError(t, err)
	var previous model.Post
	err = db.QueryRowContext(ctx, latestPostsQuery, pq.Array([]string{"one"})).Scan(&previous.Id, &previous.ChannelId, &previous.RootId)
	require.NoError(t, err)
	require.Equal(t, "tie-a", previous.Id)

	// A remaining thread reply must not become the preview after its root is deleted.
	_, err = db.Exec(`UPDATE posts SET deleteat=10 WHERE id='root-two'`)
	require.NoError(t, err)
	err = db.QueryRowContext(ctx, latestPostsQuery, pq.Array([]string{"two"})).Scan(&previous.Id, &previous.ChannelId, &previous.RootId)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestPreviewPriorityMetadataPostgres(t *testing.T) {
	db := previewTestDB(t)
	_, err := db.Exec(`CREATE TABLE postspriority (
		postid varchar PRIMARY KEY,channelid varchar,priority varchar,requestedack boolean,persistentnotifications boolean);
		CREATE TABLE postacknowledgements (
		postid varchar,userid varchar,channelid varchar,acknowledgedat bigint,remoteid varchar);
		INSERT INTO postspriority VALUES
		('first','channel','urgent',true,false),('nullable','channel',NULL,NULL,NULL),('private','other','important',true,true);
		INSERT INTO postacknowledgements VALUES
		('first','alice','channel',123,NULL),('first','bob','channel',124,'remote'),
		('first','removed','channel',0,NULL),('private','alice','other',125,NULL)`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	metadata, err := previewPriorityMetadata(ctx, db, []string{"first", "nullable", "missing"})
	require.NoError(t, err)
	require.Len(t, metadata, 3)
	priority := metadata["first"].Priority
	require.NotNil(t, priority)
	require.Equal(t, "urgent", *priority.Priority)
	require.True(t, *priority.RequestedAck)
	require.False(t, *priority.PersistentNotifications)
	require.Len(t, metadata["first"].Acknowledgements, 2)
	acks := map[string]*model.PostAcknowledgement{}
	for _, ack := range metadata["first"].Acknowledgements {
		acks[ack.UserId] = ack
	}
	require.Nil(t, acks["alice"].RemoteId)
	require.Equal(t, "remote", *acks["bob"].RemoteId)
	require.NotContains(t, acks, "removed")
	require.NotNil(t, metadata["nullable"].Priority)
	require.Nil(t, metadata["nullable"].Priority.Priority)
	require.Nil(t, metadata["nullable"].Priority.RequestedAck)
	require.Nil(t, metadata["nullable"].Priority.PersistentNotifications)
	require.Nil(t, metadata["missing"].Priority)
	require.Empty(t, metadata["missing"].Acknowledgements)
	_, err = db.Exec(`UPDATE postspriority SET priority='important' WHERE postid='first';
		UPDATE postacknowledgements SET acknowledgedat=0 WHERE postid='first'`)
	require.NoError(t, err)
	metadata, err = previewPriorityMetadata(ctx, db, []string{"first"})
	require.NoError(t, err)
	require.Equal(t, "important", *metadata["first"].Priority.Priority)
	require.Empty(t, metadata["first"].Acknowledgements)
	canceled, stop := context.WithCancel(context.Background())
	stop()
	_, err = previewPriorityMetadata(canceled, db, []string{"first"})
	require.ErrorIs(t, err, context.Canceled)
	_, err = db.Exec(`DROP TABLE postacknowledgements`)
	require.NoError(t, err)
	_, err = previewPriorityMetadata(ctx, db, []string{"first"})
	require.Error(t, err, "query failures must trigger the caller's existing preparation fallback")
}

func TestPreviewPriorityMetadataEmpty(t *testing.T) {
	metadata, err := previewPriorityMetadata(context.Background(), nil, nil)
	require.NoError(t, err)
	require.Empty(t, metadata)
}
