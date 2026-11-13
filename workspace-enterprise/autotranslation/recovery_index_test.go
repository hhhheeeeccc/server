package autotranslation

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func recoveryIndexSchema(t *testing.T) (*sql.DB, func() *sql.DB) {
	t.Helper()
	th := setupTranslationLifecycle(t)
	schemaName := "workspace_recovery_" + model.NewId()
	schema := pq.QuoteIdentifier(schemaName)
	dsn, err := url.Parse(*th.App.Config().SqlSettings.DataSource)
	require.NoError(t, err)
	query := dsn.Query()
	query.Set("search_path", schemaName+",pg_catalog")
	dsn.RawQuery = query.Encode()
	open := func() *sql.DB {
		// Cancellation can discard a connection. Every replacement must retain
		// the isolated schema rather than silently falling back to public.
		connector, err := pq.NewConnector(dsn.String())
		require.NoError(t, err)
		db := sql.OpenDB(connector)
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		return db
	}
	db := open()
	_, err = db.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		require.NoError(t, err)
	})
	// Keep the native lookup indexes: the plan must improve with the new index
	// under the existing schema, without forcing PostgreSQL's plan selection.
	_, err = db.Exec(`CREATE TABLE channels(id varchar(26) PRIMARY KEY,deleteat bigint NOT NULL DEFAULT 0,autotranslation boolean NOT NULL DEFAULT true);
		CREATE TABLE users(id varchar(26) PRIMARY KEY,locale varchar(5),deleteat bigint NOT NULL DEFAULT 0);
		CREATE TABLE channelmembers(channelid varchar(26),userid varchar(26),autotranslationdisabled boolean NOT NULL DEFAULT false,PRIMARY KEY(channelid,userid));
		CREATE TABLE posts(id varchar(26) PRIMARY KEY,channelid varchar(26),type text NOT NULL DEFAULT '',deleteat bigint NOT NULL DEFAULT 0,message text);
		CREATE TABLE translations(objectid varchar(26) NOT NULL,objecttype varchar NOT NULL,dstlang varchar NOT NULL,
			providerid varchar NOT NULL,normhash char(64) NOT NULL DEFAULT '',text text NOT NULL DEFAULT '',confidence real,
			meta jsonb,updateat bigint NOT NULL,state varchar(20) NOT NULL,channelid varchar(26),PRIMARY KEY(objectid,objecttype,dstlang));
		CREATE INDEX idx_translations_state ON translations(state) WHERE state IN ('processing');
		CREATE INDEX idx_translations_channel_updateat ON translations(channelid,objecttype,updateat DESC,dstlang);
		INSERT INTO translations(objectid,objecttype,dstlang,providerid,updateat,state) VALUES
			('ready','post','ar','libretranslate',1,'ready'),('processing','post','ar','libretranslate',1,'processing'),
			('unavailable','post','ar','libretranslate',1,'unavailable'),('other','other','ar','libretranslate',1,'unavailable')`)
	require.NoError(t, err)
	return db, open
}

func recoveryIndexIdentity(t *testing.T, db *sql.DB) (int64, bool) {
	t.Helper()
	var oid int64
	var valid bool
	require.NoError(t, db.QueryRow(`SELECT c.oid,i.indisvalid AND i.indisready FROM pg_class c
		JOIN pg_index i ON i.indexrelid=c.oid WHERE c.oid=to_regclass($1)`, recoveryIndexName).Scan(&oid, &valid))
	return oid, valid
}

func TestRecoveryIndexPreservesRowsAndIsRepeatable(t *testing.T) {
	db, _ := recoveryIndexSchema(t)
	digest := func() string {
		var value string
		require.NoError(t, db.QueryRow(`SELECT md5(string_agg(row_to_json(t)::text,'' ORDER BY objectid)) FROM translations t`).Scan(&value))
		return value
	}
	before := digest()
	require.NoError(t, ensureRecoveryIndex(context.Background(), db))
	oid, valid := recoveryIndexIdentity(t, db)
	require.True(t, valid)
	require.NoError(t, ensureRecoveryIndex(context.Background(), db))
	secondOID, valid := recoveryIndexIdentity(t, db)
	require.True(t, valid)
	require.Equal(t, oid, secondOID, "a valid index must not be rebuilt each sweep")
	require.Equal(t, before, digest(), "index installation must not rewrite translation records")
}

func TestRecoveryIndexWorkerRespectsFeatureAvailability(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec("DROP INDEX CONCURRENTLY IF EXISTS " + recoveryIndexName)
	require.NoError(t, err)
	_, err = db.Exec("DROP INDEX CONCURRENTLY IF EXISTS " + publicationIndexName)
	require.NoError(t, err)
	license := th.Server.License()
	for _, reason := range []string{"unlicensed", "disabled", "closed"} {
		t.Run(reason, func(t *testing.T) {
			s := New(th.Server)
			switch reason {
			case "unlicensed":
				th.Server.SetLicense(nil)
				defer th.Server.SetLicense(license)
			case "disabled":
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.AutoTranslationSettings.Enable = new(false) })
				defer th.App.UpdateConfig(func(cfg *model.Config) { cfg.AutoTranslationSettings.Enable = new(true) })
			case "closed":
				require.NoError(t, s.Shutdown())
			}
			require.NoError(t, s.recoverTranslations(context.Background(), th.App.Log()))
			var absent bool
			require.NoError(t, db.QueryRow(`SELECT to_regclass($1) IS NULL`, recoveryIndexName).Scan(&absent))
			require.True(t, absent, "an unavailable feature must not start an index build")
			require.NoError(t, db.QueryRow(`SELECT to_regclass($1) IS NULL`, publicationIndexName).Scan(&absent))
			require.True(t, absent, "an unavailable feature must not start a publication index build")
		})
	}
}

func TestRecoveryIndexRejectsUnexpectedNamedObjects(t *testing.T) {
	db, _ := recoveryIndexSchema(t)
	for _, definition := range []string{
		`CREATE TABLE ` + recoveryIndexName + `(value text)`,
		`CREATE INDEX ` + recoveryIndexName + ` ON translations(updateat,objectid,dstlang)`,
		`CREATE INDEX ` + recoveryIndexName + ` ON translations(dstlang,objectid,updateat) WHERE objecttype='post'`,
		`CREATE INDEX ` + recoveryIndexName + ` ON posts(id)`,
	} {
		t.Run(definition, func(t *testing.T) {
			_, err := db.Exec(definition)
			require.NoError(t, err)
			var before, after int64
			require.NoError(t, db.QueryRow(`SELECT to_regclass($1)::oid`, recoveryIndexName).Scan(&before))
			require.ErrorContains(t, ensureRecoveryIndex(context.Background(), db), "unexpected definition")
			require.NoError(t, db.QueryRow(`SELECT to_regclass($1)::oid`, recoveryIndexName).Scan(&after))
			require.Equal(t, before, after, "do not drop an unrelated object which happens to have the managed name")
			if definition[:12] == "CREATE TABLE" {
				_, err = db.Exec("DROP TABLE " + recoveryIndexName)
			} else {
				_, err = db.Exec("DROP INDEX " + recoveryIndexName)
			}
			require.NoError(t, err)
		})
	}
}

func TestRecoveryIndexInterruptedBuildAllowsWritesAndIsRepaired(t *testing.T) {
	db, open := recoveryIndexSchema(t)
	holder := open()
	observer := open()
	tx, err := holder.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE translations SET text='writer remains open' WHERE objectid='processing'`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- ensureRecoveryIndex(ctx, db) }()
	require.Eventually(t, func() bool {
		var waiting bool
		err := observer.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_progress_create_index
			WHERE relid='translations'::regclass AND phase LIKE 'waiting for writers%')`).Scan(&waiting)
		return err == nil && waiting
	}, 10*time.Second, 20*time.Millisecond, "concurrent build should wait for the existing writer")
	writeCtx, writeCancel := context.WithTimeout(context.Background(), time.Second)
	defer writeCancel()
	_, err = observer.ExecContext(writeCtx, `UPDATE translations SET text='a new writer also succeeds' WHERE objectid='ready'`)
	require.NoError(t, err, "the concurrent build must allow new ordinary writes")
	cancel()
	select {
	case err = <-finished:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled build did not stop")
	}
	_, valid := recoveryIndexIdentity(t, observer)
	require.False(t, valid, "a canceled PostgreSQL build should leave a real invalid index to repair")
	require.NoError(t, tx.Rollback())
	repairCtx, repairCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer repairCancel()
	require.NoError(t, ensureRecoveryIndex(repairCtx, db))
	_, valid = recoveryIndexIdentity(t, observer)
	require.True(t, valid)
	var text string
	require.NoError(t, observer.QueryRow(`SELECT text FROM translations WHERE objectid='ready'`).Scan(&text))
	require.Equal(t, "a new writer also succeeds", text)
}

func TestRecoveryIndexCoordinatesNodesAndHonorsDeadline(t *testing.T) {
	db, open := recoveryIndexSchema(t)
	other := open()
	holder := open()
	tx, err := holder.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`SELECT pg_advisory_xact_lock($1)`, recoveryIndexLock)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	require.Error(t, ensureRecoveryIndex(ctx, db))
	require.NoError(t, tx.Rollback())
	finished := make(chan error, 2)
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, node := range []*sql.DB{db, other} {
		go func() { finished <- ensureRecoveryIndex(ctx, node) }()
	}
	for range 2 {
		select {
		case err = <-finished:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("concurrent index installations did not finish")
		}
	}
	_, valid := recoveryIndexIdentity(t, db)
	require.True(t, valid)
	var unlocked bool
	require.NoError(t, holder.QueryRow(`SELECT pg_try_advisory_lock($1)`, recoveryIndexLock).Scan(&unlocked))
	require.True(t, unlocked, "neither the canceled waiter nor successful installers may leave a pooled lock")
	_, err = holder.Exec(`SELECT pg_advisory_unlock($1)`, recoveryIndexLock)
	require.NoError(t, err)
}

type recoveryPlanNode struct {
	NodeType string `json:"Node Type"`
	Relation string `json:"Relation Name"`
	Index    string `json:"Index Name"`
	Removed  int    `json:"Rows Removed by Filter"`
	Plans    []recoveryPlanNode
}

func TestRecoveryIndexClaimPlanSkipsTerminalRows(t *testing.T) {
	db, _ := recoveryIndexSchema(t)
	_, err := db.Exec(`TRUNCATE translations;
		INSERT INTO channels(id) VALUES('synthetic-channel');
		INSERT INTO users(id,locale) VALUES('synthetic-user','ar');
		INSERT INTO channelmembers(channelid,userid) VALUES('synthetic-channel','synthetic-user');
		INSERT INTO posts(id,channelid,message) SELECT substr(md5(i::text),1,26),'synthetic-channel','Synthetic text '||i FROM generate_series(1,150000) i;
		INSERT INTO translations(objectid,objecttype,dstlang,providerid,state,updateat,channelid)
		SELECT id,'post','ar','libretranslate','ready',1,channelid FROM posts;
		ANALYZE`)
	require.NoError(t, err)
	measure := func(name string, indexed bool) {
		for run := range 3 {
			tx, err := db.Begin()
			require.NoError(t, err)
			var raw []byte
			err = tx.QueryRow(`EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+recoveryClaimSQL,
				recoveryAge.Milliseconds(), model.NewId()).Scan(&raw)
			require.NoError(t, tx.Rollback())
			require.NoError(t, err)
			var plan []struct {
				Plan recoveryPlanNode
			}
			require.NoError(t, json.Unmarshal(raw, &plan))
			removed, found := 0, false
			var walk func(recoveryPlanNode)
			walk = func(node recoveryPlanNode) {
				if node.Relation == "translations" {
					removed += node.Removed
				}
				found = found || node.Index == recoveryIndexName
				for _, child := range node.Plans {
					walk(child)
				}
			}
			walk(plan[0].Plan)
			if indexed {
				require.True(t, found, "planner should select the candidate index without a forced plan")
				require.LessOrEqual(t, removed, 100, "ready translations must not be scanned while claiming work")
			} else {
				require.False(t, found)
				require.GreaterOrEqual(t, removed, 149900, "baseline should demonstrate the original scan")
			}
			t.Logf("RECOVERY_INDEX_PLAN %s %d %s", name, run, raw)
		}
	}
	measure("all-ready-before", false)
	_, err = db.Exec(`UPDATE translations SET state='unavailable' WHERE objectid IN (SELECT id FROM posts ORDER BY id LIMIT 100); ANALYZE translations`)
	require.NoError(t, err)
	measure("pending-before", false)
	require.NoError(t, ensureRecoveryIndex(context.Background(), db))
	measure("pending-after", true)
	_, err = db.Exec(`UPDATE translations SET state='ready' WHERE state='unavailable'; ANALYZE translations`)
	require.NoError(t, err)
	measure("all-ready-after", true)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM translations WHERE state='ready'`).Scan(&count))
	require.Equal(t, 150000, count, "EXPLAIN claims must roll back their updates")
}
