package database_migration

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// RepairUserEmail makes restored user emails safe for the core's string scans.
// Real addresses remain unique; only SQL NULL is normalized to the supported
// empty-string representation. All DDL and data changes commit together.
func RepairUserEmail(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{"SET LOCAL lock_timeout='5s'", "SET LOCAL statement_timeout='30s'", "SELECT pg_advisory_xact_lock(6492261413)"} {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("lock user email migration: %w", err)
		}
	}
	var oid int64
	var schema, name string
	if err = tx.QueryRowContext(ctx, `SELECT c.oid,n.nspname,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.oid=to_regclass('users') AND c.relkind='r'`).Scan(&oid, &schema, &name); err != nil {
		return fmt.Errorf("resolve users table: %w", err)
	}
	table := pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(name)
	if _, err = tx.ExecContext(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("lock users table: %w", err)
	}
	// Do not choose winners or discard duplicate real addresses.
	var duplicate bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE email IS NOT NULL AND email<>'' GROUP BY email HAVING count(*)>1)").Scan(&duplicate); err != nil {
		return err
	}
	if duplicate {
		return fmt.Errorf("duplicate nonempty user emails; no records changed")
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.relname,pg_get_indexdef(i.indexrelid),COALESCE(pg_get_expr(i.indpred,i.indrelid),''),i.indisvalid AND i.indisready AND i.indimmediate,i.indisprimary,COALESCE(k.conname,''),COALESCE(k.contype::text,'')
 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
 JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attname='email' AND NOT a.attisdropped
 LEFT JOIN pg_constraint k ON k.conindid=i.indexrelid AND k.conrelid=i.indrelid AND k.contype IN ('u','p')
 WHERE i.indrelid=$1::oid AND i.indisunique AND i.indnkeyatts=1 AND i.indkey[0]=a.attnum AND i.indexprs IS NULL ORDER BY c.relname`, oid)
	if err != nil {
		return err
	}
	type index struct {
		name, definition, predicate string
		valid, primary              bool
		constraint, kind            string
	}
	var indexes []index
	for rows.Next() {
		var i index
		if err = rows.Scan(&i.name, &i.definition, &i.predicate, &i.valid, &i.primary, &i.constraint, &i.kind); err != nil {
			rows.Close()
			return err
		}
		indexes = append(indexes, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	suitable := false
	for _, i := range indexes {
		if !i.valid {
			return fmt.Errorf("email index %s is invalid, unfinished or deferrable; no records changed", i.name)
		}
		if i.primary {
			return fmt.Errorf("email is a primary key; manual schema review required")
		}
		if i.predicate != "" {
			predicate := strings.ReplaceAll(i.predicate, "::text", "")
			predicate = strings.Map(func(r rune) rune {
				if r == '(' || r == ')' || r == ' ' || r == '\n' || r == '\t' {
					return -1
				}
				return r
			}, predicate)
			if predicate == "email<>''" {
				suitable = true
			}
			continue
		}
		// A full unique email index accepts many NULLs but only one empty string.
		// Convert every such index, including legacy idx_users_email, preserving
		// its definition (key, collation and INCLUDE columns). Dependent foreign
		// keys cause an error and rollback; never drop dependencies with CASCADE.
		if i.constraint != "" {
			if _, err = tx.ExecContext(ctx, "ALTER TABLE "+table+" DROP CONSTRAINT "+pq.QuoteIdentifier(i.constraint)); err != nil {
				return fmt.Errorf("replace email constraint: %w", err)
			}
		} else if _, err = tx.ExecContext(ctx, "DROP INDEX "+pq.QuoteIdentifier(schema)+"."+pq.QuoteIdentifier(i.name)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, i.definition+" WHERE email <> ''"); err != nil {
			return fmt.Errorf("replace full email index %s: %w", i.name, err)
		}
		suitable = true
	}
	if !suitable {
		if _, err = tx.ExecContext(ctx, "CREATE UNIQUE INDEX idx_users_email_unique ON "+table+" (email) WHERE email <> ''"); err != nil {
			return fmt.Errorf("create email index: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE "+table+" SET email='' WHERE email IS NULL"); err != nil {
		return fmt.Errorf("normalize null user emails: %w", err)
	}
	var notNull bool
	var defaultExpr string
	if err = tx.QueryRowContext(ctx, `SELECT a.attnotnull,COALESCE(pg_get_expr(d.adbin,d.adrelid),'') FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum WHERE a.attrelid=$1::oid AND a.attname='email' AND NOT a.attisdropped`, oid).Scan(&notNull, &defaultExpr); err != nil {
		return err
	}
	if defaultExpr != "''::character varying" && defaultExpr != "''::text" && defaultExpr != "''" {
		if _, err = tx.ExecContext(ctx, "ALTER TABLE "+table+" ALTER COLUMN email SET DEFAULT ''"); err != nil {
			return err
		}
	}
	if !notNull {
		if _, err = tx.ExecContext(ctx, "ALTER TABLE "+table+" ALTER COLUMN email SET NOT NULL"); err != nil {
			return err
		}
	}
	return tx.Commit()
}
