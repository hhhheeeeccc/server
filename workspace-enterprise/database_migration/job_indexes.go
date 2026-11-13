package database_migration

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lib/pq"
)

// RepairJobIndexes restores the core Jobs lookup indexes lost in some restored
// databases. Migration history can already contain the original migrations.
// Run before serving requests; rows and existing index definitions are preserved.
func RepairJobIndexes(ctx context.Context, db *sql.DB) (bool, error) {
	return repairLookupIndexes(ctx, db, "jobs")
}

// RepairThreadMembershipIndexes restores the original user and timestamp lookup
// indexes when a restored database already records their migration as applied.
func RepairThreadMembershipIndexes(ctx context.Context, db *sql.DB) (bool, error) {
	return repairLookupIndexes(ctx, db, "threadmemberships")
}

func repairLookupIndexes(ctx context.Context, db *sql.DB, tableName string) (bool, error) {
	contract, err := LoadSchemaContract()
	if err != nil {
		return false, err
	}
	var expected []ContractIndex
	for _, index := range contract.Indexes {
		if index.Table == tableName && !index.Unique && index.Primary == "" {
			expected = append(expected, index)
		}
	}
	if len(expected) == 0 {
		return false, fmt.Errorf("schema reference has no %s lookup indexes", tableName)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, statement := range []string{`SET LOCAL lock_timeout='5s'`, `SET LOCAL statement_timeout='120s'`} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return false, err
		}
	}
	var schema, table string
	if err := tx.QueryRowContext(ctx, `SELECT n.nspname,c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.oid=to_regclass($1) AND c.relkind='r'`, tableName).Scan(&schema, &table); err != nil {
		return false, fmt.Errorf("resolve %s table: %w", tableName, err)
	}
	actual, err := readContractIndexes(ctx, tx, schema)
	if err != nil {
		return false, err
	}
	missing, err := missingContractIndexes(expected, actual)
	if err != nil || len(missing) == 0 {
		return false, err
	}
	qualified := pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(table)
	if _, err := tx.ExecContext(ctx, "LOCK TABLE "+qualified+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return false, err
	}
	actual, err = readContractIndexes(ctx, tx, schema)
	if err != nil {
		return false, err
	}
	missing, err = missingContractIndexes(expected, actual)
	if err != nil {
		return false, err
	}
	for _, index := range missing {
		// Definitions come from the embedded reference of this source version.
		if _, err := tx.ExecContext(ctx, "CREATE INDEX "+pq.QuoteIdentifier(index.Name)+" ON "+qualified+index.Definition); err != nil {
			return false, fmt.Errorf("restore %s index %s: %w", tableName, index.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return len(missing) > 0, nil
}
