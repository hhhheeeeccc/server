package database_migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/lib/pq"
)

// RepairPropertyFieldUniqueness replaces one known restored, overly broad key
// with the two uniqueness rules used by core migrations 163 and 164. It never
// changes field data, options, links, or migration history.
func RepairPropertyFieldUniqueness(ctx context.Context, db *sql.DB) (bool, error) {
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
	if err = tx.QueryRowContext(ctx, `SELECT c.oid,n.nspname,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.oid=to_regclass('propertyfields') AND c.relkind='r'`).Scan(&oid, &schema, &name); err != nil {
		return false, fmt.Errorf("resolve propertyfields table: %w", err)
	}
	table := pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(name)
	if _, err = tx.ExecContext(ctx, "LOCK TABLE "+table+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return false, fmt.Errorf("lock property fields: %w", err)
	}
	const obsolete = "propertyfields_groupid_name_unique"
	old, err := readPropertyFieldIndex(ctx, tx, schema, obsolete)
	if errors.Is(err, sql.ErrNoRows) {
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	if old.tableOID != oid || !old.valid || old.columns != "groupid,name" || old.predicate != "" || (old.constraintType != "" && old.constraintType != "u") {
		return false, fmt.Errorf("%s has an unexpected definition; no fields changed", obsolete)
	}
	for _, rule := range []struct{ name, columns, predicate string }{
		{"idx_propertyfields_unique_legacy", "groupid,targetid,name", "deleteat = 0 AND objecttype = ''"},
		{"idx_propertyfields_unique_typed", "objecttype,groupid,targettype,targetid,name", "deleteat = 0 AND objecttype <> ''"},
	} {
		index, err := readPropertyFieldIndex(ctx, tx, schema, rule.name)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, "CREATE UNIQUE INDEX "+pq.QuoteIdentifier(rule.name)+" ON "+table+" ("+rule.columns+") WHERE "+rule.predicate)
			if err != nil {
				return false, fmt.Errorf("restore %s: %w", rule.name, err)
			}
		} else if err != nil {
			return false, err
		} else if index.tableOID != oid || !index.valid || index.columns != rule.columns || normalizePropertyPredicate(index.predicate) != normalizePropertyPredicate(rule.predicate) || index.constraintType != "" {
			return false, fmt.Errorf("%s has an unexpected definition; no fields changed", rule.name)
		}
	}
	query := "DROP INDEX " + pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(obsolete)
	if old.constraintType == "u" {
		query = "ALTER TABLE " + table + " DROP CONSTRAINT " + pq.QuoteIdentifier(obsolete)
	}
	// No CASCADE: dependent foreign keys must cause the whole repair to roll back.
	if _, err = tx.ExecContext(ctx, query); err != nil {
		return false, fmt.Errorf("replace obsolete property field key: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

type propertyFieldIndex struct {
	tableOID                           int64
	valid                              bool
	columns, predicate, constraintType string
}

func readPropertyFieldIndex(ctx context.Context, tx *sql.Tx, schema, name string) (propertyFieldIndex, error) {
	var index propertyFieldIndex
	err := tx.QueryRowContext(ctx, `SELECT i.indrelid,
 i.indisunique AND i.indisvalid AND i.indisready AND i.indimmediate AND i.indexprs IS NULL AND i.indnatts=i.indnkeyatts,
 COALESCE((SELECT string_agg(a.attname,',' ORDER BY k.pos) FROM unnest(i.indkey) WITH ORDINALITY k(num,pos) JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.num),''),
 COALESCE(pg_get_expr(i.indpred,i.indrelid),''),COALESCE(c.contype::text,'')
 FROM pg_class r JOIN pg_namespace n ON n.oid=r.relnamespace
 JOIN pg_index i ON i.indexrelid=r.oid LEFT JOIN pg_constraint c ON c.conindid=i.indexrelid AND c.conrelid=i.indrelid AND c.contype IN ('p','u','x')
 WHERE n.nspname=$1 AND r.relname=$2`, schema, name).Scan(&index.tableOID, &index.valid, &index.columns, &index.predicate, &index.constraintType)
	return index, err
}

var propertyPredicateFormatting = regexp.MustCompile(`::(?:text|character varying|bigint|integer)|[\s()]`)

func normalizePropertyPredicate(value string) string {
	return propertyPredicateFormatting.ReplaceAllString(strings.ToLower(value), "")
}
