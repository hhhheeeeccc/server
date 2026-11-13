// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

// migration-rehearsal runs the server migrator on a disposable local clone.
// Connection settings come from libpq PG* environment variables.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	_ "github.com/lib/pq"
	"github.com/mattermost/enterprise/database_migration"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/store/sqlstore"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	// Explicit sslmode allows a Unix socket; all other settings use libpq env.
	const dsn = "sslmode=disable connect_timeout=10"
	connection, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer connection.Close()
	var database string
	var unixSocket bool
	if err = connection.QueryRow("SELECT current_database(), inet_server_addr() IS NULL").Scan(&database, &unixSocket); err != nil {
		return err
	}
	if !unixSocket || !strings.HasPrefix(database, "mm_upgrade_rehearsal_") {
		return fmt.Errorf("refusing migration: requires a local Unix socket and a disposable mm_upgrade_rehearsal_* database")
	}
	if err = database_migration.CheckDatabase(connection); err != nil {
		return err
	}

	settings := model.SqlSettings{DataSource: new(dsn)}
	settings.SetDefaults(false)
	logger := mlog.CreateConsoleLogger()
	defer logger.Shutdown()
	migrator, err := sqlstore.NewMigrator(settings, logger, false)
	if err != nil {
		return err
	}
	defer migrator.Close()
	if err = migrator.PreMigrate(); err != nil {
		return err
	}
	plan, err := migrator.GeneratePlan(false)
	if err != nil {
		return err
	}
	if len(plan.Migrations) == 0 {
		fmt.Println("No pending migrations on the rehearsal database.")
		return nil
	}
	if err = migrator.MigrateWithPlan(plan, false); err != nil {
		return err
	}
	fmt.Printf("Applied %d migrations on the rehearsal database.\n", len(plan.Migrations))
	return nil
}
