// Package file_metadata_repair supplies a maintenance operation for
// restored optional strings that the core FileInfo store scans into Go strings.
// Enterprise startup also uses it to repair restored databases before serving
// APIs. It never touches attachment bytes and archives every changed value.
package file_metadata_repair

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/lib/pq"
)

type Report struct {
	Database      string `json:"database"`
	Table         string `json:"table"`
	Affected      int64  `json:"affected_rows"`
	ThumbnailPath int64  `json:"null_thumbnailpath"`
	PreviewPath   int64  `json:"null_previewpath"`
	Extension     int64  `json:"null_extension"`
	MimeType      int64  `json:"null_mimetype"`
	Applied       bool   `json:"applied"`
	BackupTable   string `json:"backup_table,omitempty"`
	RollbackSQL   string `json:"rollback_sql,omitempty"`
}

const predicate = `thumbnailpath IS NULL OR previewpath IS NULL OR extension IS NULL OR mimetype IS NULL`

// Run defaults to a read-only transaction. Applying requires an exact database
// name as an independent check against accidentally selecting another config.
// Original metadata is archived in the same transaction as the update.
func Run(ctx context.Context, db *sql.DB, expectedDatabase string, apply bool) (Report, error) {
	var report Report
	if expectedDatabase == "" {
		return report, fmt.Errorf("an explicit expected database name is required")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: !apply})
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT current_database()`).Scan(&report.Database); err != nil {
		return Report{}, err
	}
	if report.Database != expectedDatabase {
		return Report{}, fmt.Errorf("database mismatch: connected to %q, expected %q; no changes made", report.Database, expectedDatabase)
	}
	for _, statement := range []string{`SET LOCAL lock_timeout='5s'`, `SET LOCAL statement_timeout='30s'`} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return Report{}, err
		}
	}
	var schema, name string
	if err := tx.QueryRowContext(ctx, `SELECT n.nspname,c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.oid=to_regclass('fileinfo') AND c.relkind='r'`).Scan(&schema, &name); err != nil {
		return Report{}, fmt.Errorf("resolve ordinary fileinfo table: %w", err)
	}
	table := pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(name)
	report.Table = table
	if apply {
		// Ordinary reads continue; writes cannot race with the snapshot/update.
		if _, err := tx.ExecContext(ctx, "LOCK TABLE "+table+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return Report{}, err
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*),
		count(*) FILTER(WHERE thumbnailpath IS NULL),count(*) FILTER(WHERE previewpath IS NULL),
		count(*) FILTER(WHERE extension IS NULL),count(*) FILTER(WHERE mimetype IS NULL)
		FROM `+table+` WHERE `+predicate).Scan(&report.Affected, &report.ThumbnailPath, &report.PreviewPath, &report.Extension, &report.MimeType); err != nil {
		return Report{}, err
	}
	if !apply || report.Affected == 0 {
		return report, tx.Commit()
	}
	var invalidIDs bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id IS NULL)
		OR EXISTS(SELECT 1 FROM `+table+` GROUP BY id HAVING count(*)>1)`).Scan(&invalidIDs); err != nil {
		return Report{}, err
	}
	if invalidIDs {
		return Report{}, fmt.Errorf("fileinfo IDs contain NULLs or duplicates; no changes made")
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return Report{}, err
	}
	backup := pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier("workspace_fileinfo_nulls_"+time.Now().UTC().Format("20060102_150405")+"_"+hex.EncodeToString(suffix[:]))
	if _, err := tx.ExecContext(ctx, `CREATE TABLE `+backup+` AS SELECT id,thumbnailpath,previewpath,extension,mimetype FROM `+table+` WHERE `+predicate); err != nil {
		return Report{}, err
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE "+backup+" ADD PRIMARY KEY(id)"); err != nil {
		return Report{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+table+` SET thumbnailpath=COALESCE(thumbnailpath,''),
		previewpath=COALESCE(previewpath,''),extension=COALESCE(extension,''),mimetype=COALESCE(mimetype,'') WHERE `+predicate)
	if err != nil {
		return Report{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Report{}, err
	}
	if count != report.Affected {
		return Report{}, fmt.Errorf("unexpected affected count: %d, expected %d", count, report.Affected)
	}
	if err := tx.Commit(); err != nil {
		return Report{}, err
	}
	report.Applied = true
	report.BackupTable = backup
	// Skip rows subsequently edited by the application. A human must review any
	// skipped rows; blindly restoring them would overwrite newer metadata.
	report.RollbackSQL = `UPDATE ` + table + ` f SET thumbnailpath=b.thumbnailpath,previewpath=b.previewpath,extension=b.extension,mimetype=b.mimetype FROM ` + backup + ` b WHERE f.id=b.id AND f.thumbnailpath IS NOT DISTINCT FROM COALESCE(b.thumbnailpath,'') AND f.previewpath IS NOT DISTINCT FROM COALESCE(b.previewpath,'') AND f.extension IS NOT DISTINCT FROM COALESCE(b.extension,'') AND f.mimetype IS NOT DISTINCT FROM COALESCE(b.mimetype,'');`
	return report, nil
}
