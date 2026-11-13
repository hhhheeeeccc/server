package autotranslation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestPublicationIndexLifecycle(t *testing.T) {
	db, open := recoveryIndexSchema(t)
	identity := func() (int64, bool) {
		var oid int64
		var valid bool
		require.NoError(t, db.QueryRow(`SELECT c.oid,i.indisvalid AND i.indisready FROM pg_class c
			JOIN pg_index i ON i.indexrelid=c.oid WHERE c.oid=to_regclass($1)`, publicationIndexName).Scan(&oid, &valid))
		return oid, valid
	}
	var before, after string
	require.NoError(t, db.QueryRow(`SELECT md5(string_agg(row_to_json(t)::text,'' ORDER BY objectid)) FROM translations t`).Scan(&before))
	require.NoError(t, ensurePublicationIndex(context.Background(), db))
	first, valid := identity()
	require.True(t, valid)
	require.NoError(t, ensurePublicationIndex(context.Background(), db))
	second, valid := identity()
	require.True(t, valid)
	require.Equal(t, first, second)
	require.NoError(t, db.QueryRow(`SELECT md5(string_agg(row_to_json(t)::text,'' ORDER BY objectid)) FROM translations t`).Scan(&after))
	require.Equal(t, before, after, "installing an index must not rewrite results")
	_, err := db.Exec("DROP INDEX " + publicationIndexName)
	require.NoError(t, err)
	_, err = db.Exec("CREATE INDEX " + publicationIndexName + " ON translations(updateat,objectid,dstlang)")
	require.NoError(t, err)
	first, _ = identity()
	require.ErrorContains(t, ensurePublicationIndex(context.Background(), db), "unexpected definition")
	second, _ = identity()
	require.Equal(t, first, second, "conflicting definition must be left intact")
	_, err = db.Exec("DROP INDEX " + publicationIndexName)
	require.NoError(t, err)

	// Interrupt a real concurrent build after it has created the catalog entry.
	holder, observer := open(), open()
	tx, err := holder.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE translations SET text='existing writer' WHERE objectid='ready'`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ensurePublicationIndex(ctx, db) }()
	require.Eventually(t, func() bool {
		var waiting bool
		err := observer.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_progress_create_index
			WHERE relid='translations'::regclass AND phase LIKE 'waiting for writers%')`).Scan(&waiting)
		return err == nil && waiting
	}, 10*time.Second, 20*time.Millisecond)
	writeCtx, writeCancel := context.WithTimeout(context.Background(), time.Second)
	defer writeCancel()
	_, err = observer.ExecContext(writeCtx, `UPDATE translations SET text='new writer' WHERE objectid='processing'`)
	require.NoError(t, err, "concurrent installation must allow ordinary writes")
	cancel()
	select {
	case err = <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled publication index build did not return")
	}
	_, valid = identity()
	require.False(t, valid)
	require.NoError(t, tx.Rollback())
	require.NoError(t, ensurePublicationIndex(context.Background(), db))
	_, valid = identity()
	require.True(t, valid, "the next sweep must replace the interrupted build")
}

func TestPublicationIndexClaimPlanSkipsAcknowledgedRows(t *testing.T) {
	db, _ := recoveryIndexSchema(t)
	_, err := db.Exec(`TRUNCATE translations;
		INSERT INTO channels(id) VALUES('synthetic-channel');
		INSERT INTO users(id,locale) VALUES('synthetic-user','ar');
		INSERT INTO channelmembers(channelid,userid) VALUES('synthetic-channel','synthetic-user');
		INSERT INTO posts(id,channelid,message) SELECT substr(md5(i::text),1,26),'synthetic-channel','Synthetic text '||i FROM generate_series(1,150000) i;
		INSERT INTO translations(objectid,objecttype,dstlang,providerid,state,updateat,channelid,meta)
		SELECT id,'post','ar','libretranslate','ready',1,channelid,'{}'::jsonb FROM posts;
		UPDATE translations SET meta=jsonb_build_object('workspace_publish_v1',jsonb_build_object('id',objectid))
		WHERE objectid IN (SELECT id FROM posts ORDER BY id LIMIT 8); ANALYZE`)
	require.NoError(t, err)
	measure := func(indexed bool) {
		tx, err := db.Begin()
		require.NoError(t, err)
		var raw []byte
		err = tx.QueryRow(`EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+publicationClaimSQL,
			publicationLease.Milliseconds(), model.NewId(), "", "").Scan(&raw)
		require.NoError(t, tx.Rollback())
		require.NoError(t, err)
		var plan []struct{ Plan recoveryPlanNode }
		require.NoError(t, json.Unmarshal(raw, &plan))
		removed, found := 0, false
		var walk func(recoveryPlanNode)
		walk = func(node recoveryPlanNode) {
			if node.Relation == "translations" {
				removed += node.Removed
			}
			found = found || node.Index == publicationIndexName
			for _, child := range node.Plans {
				walk(child)
			}
		}
		walk(plan[0].Plan)
		if indexed {
			require.True(t, found, "the real claim should use the publication index without a forced plan")
			require.LessOrEqual(t, removed, 10)
		} else {
			require.False(t, found)
			require.GreaterOrEqual(t, removed, 149900)
		}
		t.Logf("PUBLICATION_INDEX_PLAN indexed=%t %s", indexed, raw)
	}
	measure(false)
	require.NoError(t, ensurePublicationIndex(context.Background(), db))
	measure(true)
}
