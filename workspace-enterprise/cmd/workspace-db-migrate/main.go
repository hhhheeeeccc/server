package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	_ "github.com/lib/pq"
	"github.com/mattermost/enterprise/database_migration"
	"github.com/mattermost/enterprise/file_metadata_repair"
	"github.com/mattermost/enterprise/schema_migration"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/store/sqlstore"
	"github.com/mattermost/mattermost/server/v8/config"
)

func migrate(configPath string, licenseOnly bool, expectedDatabase string) error {
	// Validate the compiled reference before any history/schema changes.
	if !licenseOnly {
		if _, err := database_migration.LoadSchemaContract(); err != nil {
			return err
		}
	}
	configStore, err := config.NewStoreFromDSN(configPath, true, nil, false)
	if err != nil {
		return fmt.Errorf("load Mattermost config: %w", err)
	}
	defer configStore.Close()

	settings := configStore.Get().SqlSettings
	if settings.DriverName == nil || *settings.DriverName != model.DatabaseDriverPostgres || settings.DataSource == nil {
		return fmt.Errorf("workspace-db-migrate requires a PostgreSQL data source")
	}

	db, err := sql.Open("postgres", *settings.DataSource)
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(context.Background()); err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	var database string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&database); err != nil {
		return err
	}
	if expectedDatabase != "" && database != expectedDatabase {
		return fmt.Errorf("database mismatch: connected to %q, expected %q; no changes made", database, expectedDatabase)
	}
	// Hold one session lock across history reconciliation, core migrations and
	// repairs. Do not allow two instances of this maintenance tool to race.
	guard, err := db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer guard.Close()
	var locked bool
	if err := guard.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock(hashtext(current_database()), hashtext('workspace-db-migrate'))`).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("another workspace-db-migrate is already running on this database")
	}
	defer guard.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtext(current_database()), hashtext('workspace-db-migrate'))`)
	if licenseOnly {
		return repairLicenseID(db)
	}

	reconciled, err := schema_migration.ReconcileLegacyVersions(context.Background(), db)
	if err != nil {
		return fmt.Errorf("reconcile legacy migration versions: %w", err)
	}
	if reconciled {
		fmt.Println("Reconciled legacy migration versions.")
	}
	if err := database_migration.CheckDatabase(db); err != nil {
		return fmt.Errorf("validate migration history: %w", err)
	}

	logger := mlog.CreateConsoleLogger()
	defer logger.Shutdown()
	migrator, err := sqlstore.NewMigrator(settings, logger, false)
	if err != nil {
		return fmt.Errorf("create Mattermost migrator: %w", err)
	}
	defer migrator.Close()

	if err := migrator.PreMigrate(); err != nil {
		return fmt.Errorf("run Mattermost pre-migrations: %w", err)
	}
	plan, err := migrator.GeneratePlan(false)
	if err != nil {
		return fmt.Errorf("plan Mattermost migrations: %w", err)
	}
	if len(plan.Migrations) == 0 {
		fmt.Println("No migrations to apply.")
	} else if err := migrator.MigrateWithPlan(plan, false); err != nil {
		return fmt.Errorf("apply Mattermost migrations: %w", err)
	} else {
		fmt.Printf("Applied %d migrations.\n", len(plan.Migrations))
	}
	if err := database_migration.CheckDatabase(db); err != nil {
		return err
	}
	report, err := database_migration.RepairSchema(context.Background(), db)
	if err != nil {
		return fmt.Errorf("verify/repair core schema: %w", err)
	}
	encoded, _ := json.Marshal(report)
	fmt.Printf("Core schema: %s\n", encoded)
	metadata, err := file_metadata_repair.Run(context.Background(), db, database, true)
	if err != nil {
		return fmt.Errorf("repair restored file metadata: %w", err)
	}
	encoded, _ = json.Marshal(metadata)
	fmt.Printf("File metadata: %s\n", encoded)
	return nil
}

func repairLicenseID(db *sql.DB) error {
	changed, err := database_migration.RepairLicenseID(context.Background(), db)
	if err != nil {
		return fmt.Errorf("repair license ID constraint: %w", err)
	}
	if changed {
		fmt.Println("Restored licenses.id constraint; license records preserved.")
	} else {
		fmt.Println("licenses.id already has a valid conflict target; no changes needed.")
	}
	return nil
}

func main() {
	configPath := flag.String("config", "config/config.json", "Mattermost config path or DSN (relative to the working directory)")
	licenseOnly := flag.Bool("repair-license-id-only", false, "repair only the licenses.id constraint; do not run core migrations")
	database := flag.String("database", "", "expected database name; mismatch aborts before any writes (recommended for deployment)")
	flag.Parse()
	if err := migrate(*configPath, *licenseOnly, *database); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
