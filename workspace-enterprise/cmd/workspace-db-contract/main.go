// workspace-db-contract captures the reference only from a freshly migrated
// disposable cluster. It never creates or changes a database.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	_ "github.com/lib/pq"
	"github.com/mattermost/enterprise/database_migration"
	coredb "github.com/mattermost/mattermost/server/v8/channels/db"
)

func run() error {
	db, err := sql.Open("postgres", "sslmode=disable connect_timeout=10")
	if err != nil {
		return err
	}
	defer db.Close()
	var name, dir string
	var local bool
	if err := db.QueryRow(`SELECT current_database(),current_setting('data_directory'),inet_server_addr() IS NULL`).Scan(&name, &dir, &local); err != nil {
		return err
	}
	if !local || !strings.HasPrefix(name, "mm_upgrade_rehearsal_") || !strings.HasPrefix(dir, "/tmp/workspace-license-migration-") {
		return fmt.Errorf("requires a fresh mm_upgrade_rehearsal_* database in a disposable local /tmp/workspace-license-migration-* cluster")
	}
	if err := database_migration.CheckDatabase(db); err != nil {
		return err
	}
	expected, err := database_migration.Read(coredb.Assets())
	if err != nil {
		return err
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM db_migrations`).Scan(&count); err != nil {
		return err
	}
	if count != len(expected) {
		return fmt.Errorf("reference database is not fully migrated")
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var schema string
	if err := tx.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		return err
	}
	contract, err := database_migration.CaptureSchemaContract(context.Background(), tx, schema)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(contract)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
