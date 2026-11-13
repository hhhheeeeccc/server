// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package database_migration

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/mattermost/mattermost/server/v8/channels/db"
)

// CheckDatabase only reads catalog and migration history. It must run before
// constructing a core migrator, because Morph initialization can create tables.
func CheckDatabase(connection *sql.DB) error {
	expected, err := Read(db.Assets())
	if err != nil {
		return err
	}
	tx, err := connection.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		WHERE table_schema=current_schema() AND table_name='db_migrations')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		// A partially restored schema without its history is not a fresh DB.
		var tables int
		if err = tx.QueryRow(`SELECT count(*) FROM information_schema.tables
			WHERE table_schema=current_schema() AND table_type='BASE TABLE'`).Scan(&tables); err != nil {
			return err
		}
		if tables != 0 {
			return fmt.Errorf("schema contains tables but no db_migrations; refusing to treat it as a fresh database")
		}
		return nil
	}
	rows, err := tx.Query("SELECT version,name FROM db_migrations ORDER BY version")
	if err != nil {
		return err
	}
	defer rows.Close()
	var history []Entry
	for rows.Next() {
		var migration Entry
		if err = rows.Scan(&migration.Version, &migration.Name); err != nil {
			return err
		}
		history = append(history, migration)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return Validate(history, expected)
}
