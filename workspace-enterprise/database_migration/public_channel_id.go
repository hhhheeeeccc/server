package database_migration

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lib/pq"
)

// RepairPublicChannelID restores the conflict target required by SqlChannelStore.upsertPublicChannelT.
// It never deletes, merges, or replaces public channel records. A valid existing key
// is a no-op; inconsistent data or unsupported constraints abort the transaction.
func RepairPublicChannelID(ctx context.Context, connection *sql.DB) (bool, error) {
	tx, err := connection.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, statement := range []string{"SET LOCAL lock_timeout='5s'", "SET LOCAL statement_timeout='30s'"} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return false, err
		}
	}
	var oid int64
	var schema, name string
	// Resolve exactly the unquoted table used by the core query, respecting
	// search_path. Do not accidentally repair a different public.publicchannels table.
	err = tx.QueryRowContext(ctx, `SELECT c.oid, n.nspname, c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.oid=to_regclass('publicchannels') AND c.relkind='r'`).Scan(&oid, &schema, &name)
	if err != nil {
		return false, fmt.Errorf("resolve publicchannels table (must be an existing ordinary table): %w", err)
	}
	table := pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(name)
	// Block writes and competing repairs while checking data and creating the
	// key. Normal reads remain possible until ALTER TABLE upgrades the lock.
	if _, err := tx.ExecContext(ctx, "LOCK TABLE "+table+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return false, fmt.Errorf("lock publicchannels table: %w", err)
	}
	var nulls, duplicates bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM "+table+" WHERE id IS NULL), "+
		"EXISTS (SELECT 1 FROM "+table+" GROUP BY id HAVING count(*)>1)").Scan(&nulls, &duplicates); err != nil {
		return false, fmt.Errorf("check public channel IDs: %w", err)
	}
	if nulls || duplicates {
		return false, fmt.Errorf("publicchannels.id contains NULLs (%t) or duplicates (%t); no records changed", nulls, duplicates)
	}
	var usable, deferred bool
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(bool_or(i.indimmediate), false),
		COALESCE(bool_or(NOT i.indimmediate), false)
		FROM pg_index i JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attname='id' AND NOT a.attisdropped
		WHERE i.indrelid=$1::oid AND i.indisunique AND i.indisvalid AND i.indisready
		AND i.indnkeyatts=1 AND i.indkey[0]=a.attnum
		AND i.indpred IS NULL AND i.indexprs IS NULL`, oid).Scan(&usable, &deferred); err != nil {
		return false, fmt.Errorf("inspect public channel ID indexes: %w", err)
	}
	if deferred {
		return false, fmt.Errorf("publicchannels.id has a deferrable unique constraint incompatible with ON CONFLICT; manual review required")
	}
	if usable {
		return false, tx.Commit()
	}
	var hasPrimaryKey bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid=$1::oid AND contype='p')`, oid).Scan(&hasPrimaryKey); err != nil {
		return false, err
	}
	constraint := "publicchannels_pkey"
	definition := "PRIMARY KEY (id)"
	if hasPrimaryKey {
		constraint = "workspace_publicchannels_id_key"
		definition = "UNIQUE (id)"
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE "+table+" ADD CONSTRAINT "+pq.QuoteIdentifier(constraint)+" "+definition); err != nil {
		return false, fmt.Errorf("restore public channel ID constraint: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
