package partial_search

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

const postTrigramIndexName = "idx_posts_message_trgm"
const postTrigramIndexLock int64 = 0x5753506f73744978

// Concurrent builds keep message writes available while PostgreSQL waits for
// existing writers and snapshots. A session lock coordinates the background
// attempts on different nodes without holding a transaction or its snapshot.
func ensurePostTrigramIndex(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, postTrigramIndexLock).Scan(&unlocked); err != nil {
			// A canceled acquisition can still have obtained the lock. Never
			// return a connection with uncertain lock ownership to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, postTrigramIndexLock).Scan(&acquired); err != nil {
		return fmt.Errorf("coordinate post search index: %w", err)
	}
	if !acquired {
		// Another node is already doing this optional optimization. A blocking
		// lock query would retain a snapshot that its concurrent build needs.
		return nil
	}

	var tableID int64
	var schema, table string
	if err := conn.QueryRowContext(ctx, `SELECT c.oid,n.nspname,c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.oid=to_regclass('posts') AND c.relkind='r'`).Scan(&tableID, &schema, &table); err != nil {
		return fmt.Errorf("resolve post search table: %w", err)
	}
	var valid, matches bool
	err = conn.QueryRowContext(ctx, `SELECT COALESCE(i.indisvalid AND i.indisready,false),
		COALESCE(i.indrelid=$1::oid AND NOT i.indisunique AND NOT i.indisexclusion
		AND i.indnatts=1 AND i.indnkeyatts=1 AND i.indexprs IS NULL AND i.indpred IS NULL
		AND am.amname='gin' AND a.attnum=i.indkey[0] AND opc.opcname='gin_trgm_ops',false)
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		LEFT JOIN pg_index i ON i.indexrelid=c.oid LEFT JOIN pg_am am ON am.oid=c.relam
		LEFT JOIN pg_attribute a ON a.attrelid=$1::oid AND a.attname='message'
		LEFT JOIN pg_opclass opc ON opc.oid=i.indclass[0]
		WHERE n.nspname=$2 AND c.relname=$3`, tableID, schema, postTrigramIndexName).Scan(&valid, &matches)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect post search index: %w", err)
	}
	qualifiedIndex := pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(postTrigramIndexName)
	if err == nil {
		if !matches {
			return fmt.Errorf("post search index %s has an unexpected definition; left unchanged", qualifiedIndex)
		}
		if valid {
			return nil
		}
		// Interrupted concurrent builds leave an unusable index. Repair only a
		// matching definition; IF NOT EXISTS would keep it invalid forever.
		if _, err := conn.ExecContext(ctx, "DROP INDEX CONCURRENTLY "+qualifiedIndex); err != nil {
			return fmt.Errorf("remove incomplete post search index: %w", err)
		}
	}
	_, err = conn.ExecContext(ctx, "CREATE INDEX CONCURRENTLY "+pq.QuoteIdentifier(postTrigramIndexName)+
		" ON "+pq.QuoteIdentifier(schema)+"."+pq.QuoteIdentifier(table)+" USING gin (message gin_trgm_ops)")
	if err != nil {
		return fmt.Errorf("build post search index: %w", err)
	}
	return nil
}
