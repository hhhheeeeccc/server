package disappearing_messages

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestDurableSendTimeExpiry(t *testing.T) {
	dsn := os.Getenv("WORKSPACE_DISAPPEARING_TEST_DSN")
	if dsn == "" {
		t.Skip("requires an isolated workspace_disappearing_test database")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx := context.Background()
	var database string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database))
	require.Equal(t, "workspace_disappearing_test", database, "refuse any non-test database")
	_, err = db.Exec(`DROP SCHEMA IF EXISTS workspace_message_expiry CASCADE; DROP SCHEMA public CASCADE; CREATE SCHEMA public;
	 CREATE TABLE channels(id varchar(26) PRIMARY KEY,type char(1),deleteat bigint DEFAULT 0,shared boolean DEFAULT false);
	 CREATE TABLE users(id varchar(26) PRIMARY KEY,deleteat bigint DEFAULT 0);
	 CREATE TABLE bots(userid varchar(26) PRIMARY KEY);
	 CREATE TABLE channelmembers(channelid varchar(26),userid varchar(26),PRIMARY KEY(channelid,userid));
	 CREATE TABLE posts(id varchar(26) PRIMARY KEY,channelid varchar(26),type text DEFAULT '',rootid text DEFAULT '',deleteat bigint DEFAULT 0,message text DEFAULT '',createat bigint DEFAULT 0);
	 INSERT INTO channels(id,type) VALUES('conversation','D'),('public','O');
	 INSERT INTO users(id) VALUES('sender'),('recipient'),('outsider');
	 INSERT INTO channelmembers VALUES('conversation','sender'),('conversation','recipient'),('public','sender');`)
	require.NoError(t, err)
	r := repository{db: db}
	require.NoError(t, r.install(ctx))
	require.NoError(t, r.install(ctx), "restart must preserve queues and policies")
	insert := func(id, root, kind string) error {
		_, err := db.Exec(`INSERT INTO posts(id,channelid,rootid,type,createat) VALUES($1,'conversation',$2,$3,1)`, id, root, kind)
		return err
	}
	require.NoError(t, insert("old", "", ""))
	require.ErrorIs(t, r.setPolicy(ctx, "conversation", "outsider", Policy{86400}), errForbidden)
	require.ErrorIs(t, r.setPolicy(ctx, "public", "sender", Policy{86400}), errForbidden)
	require.NoError(t, r.setPolicy(ctx, "conversation", "sender", Policy{86400}))
	require.NoError(t, insert("new", "", ""))
	var sent, expires, count int64
	require.NoError(t, db.QueryRow(`SELECT sent_at,expires_at FROM workspace_message_expiry.messages WHERE post_id='new'`).Scan(&sent, &expires))
	require.Greater(t, sent, int64(1), "client-supplied backdated timestamp is not the timer authority")
	require.Equal(t, int64(86400000), expires-sent)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM workspace_message_expiry.messages WHERE post_id='old'`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, insert("thread", "new", ""))
	var threadExpiry int64
	require.NoError(t, db.QueryRow(`SELECT expires_at FROM workspace_message_expiry.messages WHERE post_id='thread'`).Scan(&threadExpiry))
	require.Equal(t, expires, threadExpiry, "native threaded replies cannot outlive their disappearing parent")
	require.Error(t, insert("bor", "", "burn_on_read"), "normal visible messages must not become reveal-on-read messages")
	require.NoError(t, insert("notice", "", "system_generic"))
	_, err = db.Exec(`UPDATE posts SET message='edited' WHERE id='new'`)
	require.NoError(t, err)
	require.NoError(t, r.setPolicy(ctx, "conversation", "recipient", Policy{7776000}))
	var unchanged int64
	require.NoError(t, db.QueryRow(`SELECT expires_at FROM workspace_message_expiry.messages WHERE post_id='new'`).Scan(&unchanged))
	require.Equal(t, expires, unchanged, "editing or changing policy cannot extend a sent message")
	require.NoError(t, r.setPolicy(ctx, "conversation", "recipient", Policy{}))
	require.NoError(t, insert("thread_after_off", "new", ""))
	require.NoError(t, db.QueryRow(`SELECT expires_at FROM workspace_message_expiry.messages WHERE post_id='thread_after_off'`).Scan(&threadExpiry))
	require.Equal(t, expires, threadExpiry, "switching off does not make an existing temporary thread permanent")
	require.NoError(t, insert("after_off", "", ""))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM workspace_message_expiry.messages WHERE post_id='after_off'`).Scan(&count))
	require.Zero(t, count)
	_, err = db.Exec(`UPDATE workspace_message_expiry.messages SET expires_at=1,next_attempt_at=1 WHERE post_id='new'`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE posts SET message='late edit' WHERE id='new'`)
	require.Error(t, err)
	_, err = db.Exec(`UPDATE posts SET deleteat=10 WHERE id='new'`)
	require.NoError(t, err, "native deletion must remain possible after expiration")
	var wg sync.WaitGroup
	claims := make(chan *dueMessage, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := r.claim(ctx)
			require.NoError(t, err)
			if m != nil {
				claims <- m
			}
		}()
	}
	wg.Wait()
	close(claims)
	require.Len(t, claims, 1, "only one node owns an unexpired lease")
	claim := <-claims
	restarted := repository{db: db}
	_, err = db.Exec(`UPDATE workspace_message_expiry.messages SET next_attempt_at=1 WHERE post_id='new'`)
	require.NoError(t, err)
	retry, err := restarted.claim(ctx)
	require.NoError(t, err)
	require.NotNil(t, retry, "crashed work can be reclaimed after restart")
	require.NoError(t, r.complete(ctx, claim))
	require.NoError(t, db.QueryRow(`SELECT completed_at FROM workspace_message_expiry.messages WHERE post_id='new'`).Scan(&count))
	require.Zero(t, count, "stale worker cannot complete another node's lease")
	require.NoError(t, restarted.complete(ctx, retry))
	end, err := r.claim(ctx)
	require.NoError(t, err)
	require.Nil(t, end)
}
