package autotranslation

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

const recoveryIndexName = "idx_workspace_translation_recovery"
const recoveryIndexLock int64 = 0x57735472616e736c

// The explicit text casts give the partial predicate a stable representation
// on the varchar columns from the native translations migrations.
const recoveryIndexPredicate = `(((objecttype)::text = 'post'::text) AND ((providerid)::text = 'libretranslate'::text) AND ((state)::text = ANY (ARRAY['processing'::text, 'unavailable'::text])))`

// Build only when the licensed worker runs, outside a transaction so readers
// and writers can continue. The sweep's context bounds connection acquisition,
// node coordination, and PostgreSQL's concurrent index build phases together.
func ensureRecoveryIndex(ctx context.Context, db *sql.DB) error {
	return ensureTranslationQueueIndex(ctx, db, recoveryIndexName, recoveryIndexPredicate, recoveryIndexLock)
}

const publicationIndexName = "idx_workspace_translation_publication"
const publicationIndexPredicate = `(((objecttype)::text = 'post'::text) AND ((providerid)::text = 'libretranslate'::text) AND ((state)::text = ANY (ARRAY['ready'::text, 'skipped'::text])) AND (meta ? 'workspace_publish_v1'::text))`

func ensurePublicationIndex(ctx context.Context, db *sql.DB) error {
	return ensureTranslationQueueIndex(ctx, db, publicationIndexName, publicationIndexPredicate, recoveryIndexLock+1)
}

func ensureTranslationQueueIndex(ctx context.Context, db *sql.DB, name, predicate string, lock int64) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Even a canceled lock acquisition can race with a successful grant. Release
	// on an independent context; never return a session with our lock to the pool.
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, lock).Scan(&unlocked); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	// A blocking pg_advisory_lock statement would retain its transaction's
	// snapshot while waiting. CREATE INDEX CONCURRENTLY on the owning node can
	// then wait for that snapshot, forming a deadlock. End each attempt before
	// waiting so the other node can finish its index build.
	retry := time.NewTicker(100 * time.Millisecond)
	defer retry.Stop()
	for {
		var acquired bool
		if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, lock).Scan(&acquired); err != nil {
			return fmt.Errorf("lock translation recovery index: %w", err)
		}
		if acquired {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("lock translation recovery index: %w", ctx.Err())
		case <-retry.C:
		}
	}

	var tableID int64
	var schema, table string
	err = conn.QueryRowContext(ctx, `SELECT c.oid,n.nspname,c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.oid=to_regclass('translations') AND c.relkind='r'`).Scan(&tableID, &schema, &table)
	if err != nil {
		return fmt.Errorf("resolve translation recovery table: %w", err)
	}
	var valid, matches bool
	err = conn.QueryRowContext(ctx, `SELECT COALESCE(i.indisvalid AND i.indisready,false),
		COALESCE(i.indrelid=$1::oid AND NOT i.indisunique AND i.indnatts=3 AND i.indnkeyatts=3
		AND i.indexprs IS NULL AND am.amname='btree'
		AND pg_get_indexdef(c.oid,1,true)='updateat'
		AND pg_get_indexdef(c.oid,2,true)='objectid'
		AND pg_get_indexdef(c.oid,3,true)='dstlang'
		AND pg_get_expr(i.indpred,i.indrelid)=$3,false)
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		LEFT JOIN pg_index i ON i.indexrelid=c.oid LEFT JOIN pg_am am ON am.oid=c.relam
		WHERE n.nspname=$2 AND c.relname=$4`, tableID, schema, predicate, name).Scan(&valid, &matches)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect translation recovery index: %w", err)
	}
	qualifiedIndex := pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(name)
	if err == nil {
		if !matches {
			return fmt.Errorf("translation recovery index %s has an unexpected definition; left unchanged", qualifiedIndex)
		}
		if valid {
			return nil
		}
		// PostgreSQL retains an invalid index after an interrupted concurrent
		// build. IF NOT EXISTS would silently keep it unusable on every retry.
		if _, err = conn.ExecContext(ctx, "DROP INDEX CONCURRENTLY "+qualifiedIndex); err != nil {
			return fmt.Errorf("remove incomplete translation recovery index: %w", err)
		}
	}
	_, err = conn.ExecContext(ctx, "CREATE INDEX CONCURRENTLY "+pq.QuoteIdentifier(name)+
		" ON "+pq.QuoteIdentifier(schema)+"."+pq.QuoteIdentifier(table)+
		" (updateat,objectid,dstlang) WHERE "+predicate)
	if err != nil {
		return fmt.Errorf("build translation recovery index: %w", err)
	}
	return nil
}
