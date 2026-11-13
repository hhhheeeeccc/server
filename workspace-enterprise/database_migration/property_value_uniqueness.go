package database_migration

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lib/pq"
)

// RepairPropertyValueUniqueness restores the conflict target used by the core
// property value upsert (migration 129). It never updates or removes values.
func RepairPropertyValueUniqueness(ctx context.Context, db *sql.DB) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, query := range []string{"SET LOCAL lock_timeout='5s'", "SET LOCAL statement_timeout='30s'"} {
		if _, err = tx.ExecContext(ctx, query); err != nil {
			return false, err
		}
	}
	var oid int64
	var schema, name string
	if err = tx.QueryRowContext(ctx, `SELECT c.oid,n.nspname,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.oid=to_regclass('propertyvalues') AND c.relkind='r'`).Scan(&oid, &schema, &name); err != nil {
		return false, fmt.Errorf("resolve propertyvalues table: %w", err)
	}
	table := pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(name)
	if _, err = tx.ExecContext(ctx, "LOCK TABLE "+table+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return false, fmt.Errorf("lock property values: %w", err)
	}
	// PostgreSQL infers indexes by their key set, not name or column order.
	// INCLUDE columns are not part of the conflict target. A full unique index
	// is also a usable arbiter, although the core normally creates a partial one.
	rows, err := tx.QueryContext(ctx, `SELECT r.relname,i.indimmediate,COALESCE(pg_get_expr(i.indpred,i.indrelid),'')
 FROM pg_index i JOIN pg_class r ON r.oid=i.indexrelid
 WHERE i.indrelid=$1::oid AND i.indisunique AND i.indisvalid AND i.indisready
 AND i.indnkeyatts=3 AND i.indexprs IS NULL
 AND (SELECT array_agg(a.attname::text ORDER BY a.attname) FROM unnest(i.indkey) WITH ORDINALITY k(num,pos)
 JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.num WHERE k.pos<=i.indnkeyatts)
 =ARRAY['fieldid','groupid','targetid']::text[]`, oid)
	if err != nil {
		return false, fmt.Errorf("inspect property value indexes: %w", err)
	}
	usable := false
	var deferred string
	for rows.Next() {
		var indexName, predicate string
		var immediate bool
		if err = rows.Scan(&indexName, &immediate, &predicate); err != nil {
			rows.Close()
			return false, err
		}
		if predicate != "" && normalizePropertyPredicate(predicate) != "deleteat=0" {
			continue
		}
		if !immediate {
			deferred = indexName
		} else {
			usable = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if deferred != "" {
		return false, fmt.Errorf("propertyvalues has deferrable unique key %s incompatible with ON CONFLICT; no values changed", deferred)
	}
	if usable {
		return false, tx.Commit()
	}
	var nulls, duplicates bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE deleteat=0 AND (groupid IS NULL OR targetid IS NULL OR fieldid IS NULL)),"+
		"EXISTS(SELECT 1 FROM "+table+" WHERE deleteat=0 GROUP BY groupid,targetid,fieldid HAVING count(*)>1)").Scan(&nulls, &duplicates); err != nil {
		return false, fmt.Errorf("check active property values: %w", err)
	}
	if nulls || duplicates {
		return false, fmt.Errorf("active propertyvalues contains NULL keys (%t) or duplicate scopes (%t); no values changed", nulls, duplicates)
	}
	// Do not use IF NOT EXISTS: an unrelated object with this name must fail,
	// rather than silently marking an unusable schema as repaired.
	if _, err = tx.ExecContext(ctx, "CREATE UNIQUE INDEX idx_propertyvalues_unique ON "+table+" (groupid,targetid,fieldid) WHERE deleteat=0"); err != nil {
		return false, fmt.Errorf("restore property value conflict index: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
