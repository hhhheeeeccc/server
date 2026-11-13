package database_migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"
)

// RepairRetentionPolicyID restores the PRIMARY KEY required by the core's
// grouped policy queries. A UNIQUE index alone does not establish the
// functional dependency PostgreSQL requires for ORDER BY displayname.
// Existing policies and associations are never changed or removed.
func RepairRetentionPolicyID(ctx context.Context, db *sql.DB) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, q := range []string{"SET LOCAL lock_timeout='5s'", "SET LOCAL statement_timeout='30s'"} {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return false, err
		}
	}
	var oid int64
	var schema, name string
	if err = tx.QueryRowContext(ctx, `SELECT c.oid,n.nspname,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.oid=to_regclass('retentionpolicies') AND c.relkind='r'`).Scan(&oid, &schema, &name); err != nil {
		return false, fmt.Errorf("resolve retentionpolicies table: %w", err)
	}
	table := pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(name)
	if _, err = tx.ExecContext(ctx, "LOCK TABLE "+table+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return false, fmt.Errorf("lock retention policies: %w", err)
	}
	var constraint string
	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT k.conname,k.conkey=ARRAY[a.attnum] AND k.convalidated AND NOT k.condeferrable AND i.indisvalid AND i.indisready AND i.indimmediate
 FROM pg_constraint k JOIN pg_index i ON i.indexrelid=k.conindid
 JOIN pg_attribute a ON a.attrelid=k.conrelid AND a.attname='id' AND NOT a.attisdropped
 WHERE k.conrelid=$1::oid AND k.contype='p'`, oid).Scan(&constraint, &valid)
	if err == nil {
		if !valid {
			return false, fmt.Errorf("existing primary key %s is incompatible with retention policy queries; no policies changed", constraint)
		}
		return false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var nulls, duplicates bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE id IS NULL),EXISTS(SELECT 1 FROM "+table+" GROUP BY id HAVING count(*)>1)").Scan(&nulls, &duplicates); err != nil {
		return false, err
	}
	if nulls || duplicates {
		return false, fmt.Errorf("retentionpolicies.id contains NULLs (%t) or duplicates (%t); no policies changed", nulls, duplicates)
	}
	if _, err = tx.ExecContext(ctx, "ALTER TABLE "+table+" ADD CONSTRAINT retentionpolicies_pkey PRIMARY KEY(id)"); err != nil {
		return false, fmt.Errorf("restore retention policy primary key: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
