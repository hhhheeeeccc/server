package database_migration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/lib/pq"
	coredb "github.com/mattermost/mattermost/server/v8/channels/db"
)

// The reference is captured from an empty database migrated by this source
// tree. Never derive a reference from the damaged database being repaired.
//
//go:embed schema_contract.json
var contractJSON []byte

type SchemaContract struct {
	MigrationSHA256 string               `json:"migration_sha256"`
	Columns         []ContractColumn     `json:"columns"`
	Indexes         []ContractIndex      `json:"indexes"`
	Constraints     []ContractConstraint `json:"constraints"`
}

type ContractConstraint struct {
	Table      string `json:"table"`
	Name       string `json:"name"`
	Definition string `json:"definition"`
	Validated  bool   `json:"validated"`
}

type ContractColumn struct {
	Table string `json:"table"`
	Name  string `json:"name"`
	Type  string `json:"type"`
}

type ContractIndex struct {
	Table      string `json:"table"`
	Name       string `json:"name"`
	Definition string `json:"definition"`        // USING ...; includes keys, order, opclass, INCLUDE, predicate
	Primary    string `json:"primary,omitempty"` // PRIMARY KEY (...)
	Unique     bool   `json:"unique"`
	Valid      bool   `json:"valid"`
	Immediate  bool   `json:"immediate"`
}

type SchemaRepairReport struct {
	ColumnsChecked     int      `json:"columns_checked"`
	IndexesChecked     int      `json:"indexes_checked"`
	Created            []string `json:"created_indexes"`
	ConstraintsChecked int      `json:"constraints_checked"`
	CreatedConstraints []string `json:"created_constraints"`
}

func MigrationDigest() (string, error) {
	assets := coredb.Assets()
	files, err := fs.ReadDir(assets, "migrations/postgres")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, file := range files { // ReadDir sorts by filename
		if !strings.HasSuffix(file.Name(), ".up.sql") {
			continue
		}
		data, err := fs.ReadFile(assets, "migrations/postgres/"+file.Name())
		if err != nil {
			return "", err
		}
		h.Write([]byte(file.Name() + "\x00"))
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func LoadSchemaContract() (SchemaContract, error) {
	var contract SchemaContract
	if err := json.Unmarshal(contractJSON, &contract); err != nil {
		return contract, err
	}
	digest, err := MigrationDigest()
	if err != nil {
		return contract, err
	}
	if digest != contract.MigrationSHA256 {
		return contract, fmt.Errorf("schema reference is stale for these core migrations; regenerate on a disposable fully migrated database before building/deploying")
	}
	return contract, nil
}

// CaptureSchemaContract is for the reference generator and integration tests.
// schema must be an explicitly selected freshly migrated schema.
func CaptureSchemaContract(ctx context.Context, tx *sql.Tx, schema string) (SchemaContract, error) {
	var result SchemaContract
	digest, err := MigrationDigest()
	if err != nil {
		return result, err
	}
	result.MigrationSHA256 = digest
	rows, err := tx.QueryContext(ctx, `SELECT c.relname,a.attname,t.typname
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
		JOIN pg_type t ON t.oid=a.atttypid
		WHERE n.nspname=$1 AND c.relkind='r' ORDER BY c.relname,a.attname`, schema)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var c ContractColumn
		if err := rows.Scan(&c.Table, &c.Name, &c.Type); err != nil {
			rows.Close()
			return result, err
		}
		result.Columns = append(result.Columns, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	result.Indexes, err = readContractIndexes(ctx, tx, schema)
	if err != nil {
		return result, err
	}
	result.Constraints, err = readContractConstraints(ctx, tx, schema)
	return result, err
}

func readContractConstraints(ctx context.Context, tx *sql.Tx, schema string) ([]ContractConstraint, error) {
	rows, err := tx.QueryContext(ctx, `SELECT t.relname,c.conname,pg_get_constraintdef(c.oid),c.convalidated
	FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid JOIN pg_namespace n ON n.oid=t.relnamespace
	WHERE n.nspname=$1 AND c.contype IN ('f','c') ORDER BY t.relname,c.conname`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ContractConstraint
	for rows.Next() {
		var c ContractConstraint
		if err := rows.Scan(&c.Table, &c.Name, &c.Definition, &c.Validated); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func missingContractConstraints(expected, actual []ContractConstraint) ([]ContractConstraint, error) {
	var missing []ContractConstraint
	for _, want := range expected {
		found := false
		for _, got := range actual {
			if got.Table != want.Table {
				continue
			}
			if got.Name == want.Name || got.Definition == want.Definition {
				if got.Definition != want.Definition || !got.Validated {
					return nil, fmt.Errorf("constraint %s.%s conflicts or is not validated; review required", got.Table, got.Name)
				}
				found = true
			}
		}
		if !found {
			missing = append(missing, want)
		}
	}
	return missing, nil
}

func readContractIndexes(ctx context.Context, tx *sql.Tx, schema string) ([]ContractIndex, error) {
	rows, err := tx.QueryContext(ctx, `SELECT t.relname,c.relname,
		substring(pg_get_indexdef(i.indexrelid) FROM ' USING .*'),
		COALESCE((SELECT pg_get_constraintdef(k.oid) FROM pg_constraint k WHERE k.conindid=i.indexrelid AND k.contype='p'),''),
		i.indisunique,(i.indisvalid AND i.indisready),i.indimmediate
		FROM pg_index i JOIN pg_class t ON t.oid=i.indrelid JOIN pg_class c ON c.oid=i.indexrelid
		JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname=$1 ORDER BY t.relname,c.relname`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ContractIndex
	for rows.Next() {
		var i ContractIndex
		if err := rows.Scan(&i.Table, &i.Name, &i.Definition, &i.Primary, &i.Unique, &i.Valid, &i.Immediate); err != nil {
			return nil, err
		}
		result = append(result, i)
	}
	return result, rows.Err()
}

// RepairSchema restores missing core indexes and keys, including the conflict
// targets used by properties, users, licenses and migration history. Existing
// definitions are checked semantically, not just by index name. No drops,
// deduplication, row deletion or migration replay is performed. All DDL here
// commits together; a duplicate key, NULL primary key or conflicting object
// rolls the complete schema repair back.
func RepairSchema(ctx context.Context, db *sql.DB) (SchemaRepairReport, error) {
	var report SchemaRepairReport
	contract, err := LoadSchemaContract()
	if err != nil {
		return report, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	for _, q := range []string{`SET LOCAL lock_timeout='5s'`, `SET LOCAL statement_timeout='120s'`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return report, err
		}
	}
	var schema string
	if err := tx.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return report, err
	}
	// Require every expected ordinary table/column before restoring any key.
	actual, err := CaptureSchemaContract(ctx, tx, schema)
	if err != nil {
		return report, err
	}
	columns := make(map[string]string)
	for _, c := range actual.Columns {
		columns[c.Table+"."+c.Name] = c.Type
	}
	for _, c := range contract.Columns {
		if got := columns[c.Table+"."+c.Name]; got != c.Type {
			return report, fmt.Errorf("schema mismatch at %s.%s: type %q, expected %q; restore/review the incomplete schema", c.Table, c.Name, got, c.Type)
		}
	}
	report.ColumnsChecked = len(contract.Columns)
	missing, err := missingContractIndexes(contract.Indexes, actual.Indexes)
	if err != nil {
		return report, err
	}
	missingConstraints, err := missingContractConstraints(contract.Constraints, actual.Constraints)
	if err != nil {
		return report, err
	}
	// Serialize writes to affected tables, in fixed order. These are startup
	// repairs, before the application accepts writes; do not run on a busy DB.
	tables := map[string]bool{}
	for _, i := range missing {
		tables[i.Table] = true
	}
	for _, c := range missingConstraints {
		tables[c.Table] = true
	}
	var names []string
	for table := range tables {
		names = append(names, table)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := tx.ExecContext(ctx, "LOCK TABLE "+pq.QuoteIdentifier(schema)+"."+pq.QuoteIdentifier(name)+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return report, err
		}
	}
	// Re-read after locking, so a concurrent DDL operation cannot be mistaken
	// for a missing index and silently accepted by IF NOT EXISTS.
	indexes, err := readContractIndexes(ctx, tx, schema)
	if err != nil {
		return report, err
	}
	missing, err = missingContractIndexes(contract.Indexes, indexes)
	if err != nil {
		return report, err
	}
	for _, i := range missing {
		table := pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(i.Table)
		statement := "CREATE "
		if i.Unique {
			statement += "UNIQUE "
		}
		statement += "INDEX " + pq.QuoteIdentifier(i.Name) + " ON " + table + i.Definition
		if i.Primary != "" {
			statement = "ALTER TABLE " + table + " ADD CONSTRAINT " + pq.QuoteIdentifier(i.Name) + " " + i.Primary
		}
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return SchemaRepairReport{}, fmt.Errorf("restore %s.%s (transaction rolled back; no rows removed): %w", i.Table, i.Name, err)
		}
		report.Created = append(report.Created, i.Table+"."+i.Name)
	}
	constraints, err := readContractConstraints(ctx, tx, schema)
	if err != nil {
		return SchemaRepairReport{}, err
	}
	missingConstraints, err = missingContractConstraints(contract.Constraints, constraints)
	if err != nil {
		return SchemaRepairReport{}, err
	}
	for _, c := range missingConstraints {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE "+pq.QuoteIdentifier(schema)+"."+pq.QuoteIdentifier(c.Table)+" ADD CONSTRAINT "+pq.QuoteIdentifier(c.Name)+" "+c.Definition); err != nil {
			return SchemaRepairReport{}, fmt.Errorf("restore constraint %s.%s (transaction rolled back): %w", c.Table, c.Name, err)
		}
		report.CreatedConstraints = append(report.CreatedConstraints, c.Table+"."+c.Name)
	}
	report.ConstraintsChecked = len(contract.Constraints)
	report.IndexesChecked = len(contract.Indexes)
	if err := tx.Commit(); err != nil {
		return SchemaRepairReport{}, err
	}
	return report, nil
}

func missingContractIndexes(expected, actual []ContractIndex) ([]ContractIndex, error) {
	var missing []ContractIndex
	for _, want := range expected {
		found := false
		for _, got := range actual {
			if got.Table != want.Table {
				continue
			}
			// Supported legacy no-email accounts: keep the existing partial
			// email uniqueness policy; never replace it with a full unique key.
			emailAlternative := want.Table == "users" && want.Name == "users_email_key" && got.Definition == " USING btree (email) WHERE ((email)::text <> ''::text)" && got.Unique
			matches := got.Definition == want.Definition && got.Unique == want.Unique && (want.Primary == "" || got.Primary == want.Primary)
			if got.Name == want.Name || matches || emailAlternative {
				if !got.Valid || !got.Immediate {
					return nil, fmt.Errorf("index %s.%s is invalid, unfinished or deferrable; review required", got.Table, got.Name)
				}
				if !matches && !emailAlternative {
					return nil, fmt.Errorf("index %s.%s conflicts with the expected definition; no indexes replaced", got.Table, got.Name)
				}
				found = true
			}
		}
		if !found {
			missing = append(missing, want)
		}
	}
	return missing, nil
}
