package schema_migration

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/mattermost/enterprise/database_migration"
	coredb "github.com/mattermost/mattermost/server/v8/channels/db"
)

type migrationVersion struct {
	old  int
	new  int
	name string
}

var branchOnly = []migrationVersion{
	{153, 0, "add_download_count"},
	{154, 0, "increase_systems_value_size"},
	{155, 0, "add_status_last_activity_at_index"},
	{156, 0, "add_messagesource_to_posts"},
	{157, 0, "add_trgm_index"},
	{158, 0, "upgrade_cluster_discovery"},
	{177, 0, "make_email_nullable"},
	{178, 0, "create_tasks"},
	{179, 0, "add_tasks_creator_id_index"},
	{180, 0, "create_user_stories"},
	{181, 0, "add_posts_createat_id_composite_index"},
	{182, 0, "optimize_performance_indexes"},
	{183, 0, "localize_default_channel_names"},
}

var renumbered = []migrationVersion{
	{160, 153, "add_translation_channel_id"},
	{161, 154, "drop_translation_updateat_index"},
	{162, 155, "create_translation_channel_updateat_index"},
	{163, 156, "add_schemeid_to_roles"},
	{164, 157, "backfill_roles_schemeid"},
	{165, 158, "add_roles_schemeid_index"},
	{166, 159, "deduplicate_policy_names"},
	{169, 166, "create_views"},
	{170, 160, "add_user_tracking_to_properties"},
	{171, 161, "add_object_type_to_property_fields"},
	{172, 162, "drop_property_fields_old_unique_index"},
	{173, 163, "create_property_fields_legacy_index"},
	{174, 164, "create_property_fields_typed_index"},
	{175, 165, "add_protected_and_permissions_to_property_fields"},
}

// ReconcileLegacyVersions prepares the supported development-branch migration
// history for the current core migrations. Morph finds pending migrations by
// name but records them by version, so the old numbers must be moved first.
// Every expected pair is checked before this transaction changes anything.
func ReconcileLegacyVersions(ctx context.Context, db *sql.DB) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, statement := range []string{`SET LOCAL lock_timeout='5s'`, `SET LOCAL statement_timeout='30s'`} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return false, err
		}
	}

	var hasHistory bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('db_migrations') IS NOT NULL`).Scan(&hasHistory); err != nil {
		return false, err
	}
	if !hasHistory {
		return false, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `LOCK TABLE db_migrations IN EXCLUSIVE MODE`); err != nil {
		return false, err
	}

	var oldHistory, novemberHistory, legacyProperties bool
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM db_migrations WHERE Version = 169 AND Name = 'create_views'),
		EXISTS (SELECT 1 FROM db_migrations WHERE Version = 176 AND Name = 'create_views'),
		EXISTS (SELECT 1 FROM db_migrations WHERE Version = 170 AND Name = 'add_user_tracking_to_properties')`).Scan(&oldHistory, &novemberHistory, &legacyProperties); err != nil {
		return false, err
	}
	if oldHistory && novemberHistory {
		return false, fmt.Errorf("ambiguous legacy create_views records at versions 169 and 176; no history changed")
	}
	if !oldHistory && !novemberHistory {
		if legacyProperties {
			return false, fmt.Errorf("expected a supported legacy create_views record at version 169 or 176; no history changed")
		}
		repaired, err := reconcileReintroducedLegacyRecords(ctx, tx)
		if err != nil {
			return false, err
		}
		return repaired, tx.Commit()
	}

	archived := branchOnly
	relocated := renumbered
	required := []database_migration.Entry{
		{Version: 167, Name: "create_views_channel_id_delete_at_index"},
		{Version: 168, Name: "add_linked_field_id_to_property_fields"},
	}
	if novemberHistory {
		// The November branch assigned create_views to 176 and may contain
		// a second translation creation record at 159 besides the original 147.
		// Archive the duplicate at 159 only if present; require the original at 147.
		archived = append([]migrationVersion{}, branchOnly...)
		var has159 bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM db_migrations WHERE Version = 159 AND Name = 'create_autotranslation_tables')`).Scan(&has159); err != nil {
			return false, err
		}
		if has159 {
			archived = append(archived, migrationVersion{159, 0, "create_autotranslation_tables"})
		}
		relocated = append([]migrationVersion{}, renumbered...)
		for i := range relocated {
			if relocated[i].name == "create_views" {
				relocated[i].old = 176
			}
		}
		required = append(required,
			database_migration.Entry{Version: 147, Name: "create_autotranslation_tables"},
			database_migration.Entry{Version: 169, Name: "create_linked_field_id_index"},
		)
	}
	if err := applyMissingCoreMigrations(ctx, tx, novemberHistory); err != nil {
		return false, err
	}
	if err := validateLegacyHistory(ctx, tx, archived, relocated, required); err != nil {
		return false, err
	}

	var invalidIndex bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relname = 'idx_propertyfields_linkedfieldid' AND NOT i.indisvalid
	)`).Scan(&invalidIndex); err != nil {
		return false, err
	}
	if invalidIndex {
		return false, fmt.Errorf("idx_propertyfields_linkedfieldid is invalid; inspect the interrupted concurrent index build")
	}

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS db_migrations_legacy_versions (
		Version bigint PRIMARY KEY, Name varchar NOT NULL
	)`); err != nil {
		return false, err
	}
	for _, entry := range archived {
		if _, err := tx.ExecContext(ctx, `INSERT INTO db_migrations_legacy_versions (Version, Name) VALUES ($1, $2)`, entry.old, entry.name); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM db_migrations WHERE Version = $1 AND Name = $2`, entry.old, entry.name); err != nil {
			return false, err
		}
	}
	for _, entry := range relocated {
		if _, err := tx.ExecContext(ctx, `UPDATE db_migrations SET Version = $1 WHERE Version = $2 AND Name = $3`, -entry.old, entry.old, entry.name); err != nil {
			return false, err
		}
	}
	for _, entry := range relocated {
		if _, err := tx.ExecContext(ctx, `UPDATE db_migrations SET Version = $1 WHERE Version = $2 AND Name = $3`, entry.new, -entry.old, entry.name); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// applyMissingCoreMigrations applies core migrations that may be absent from
// legacy branch histories because they were introduced after the branch
// diverged. Each migration is applied (SQL + db_migrations record) only when
// no existing record is found. Non-concurrent index creation is used because
// the caller already holds an open transaction.
func applyMissingCoreMigrations(ctx context.Context, tx *sql.Tx, november bool) error {
	type pending struct {
		version int
		name    string
		sql     string
	}
	migrations := []pending{
		{167, "create_views_channel_id_delete_at_index",
			"CREATE INDEX IF NOT EXISTS idx_views_channel_id_delete_at ON Views(ChannelId, DeleteAt)"},
		{168, "add_linked_field_id_to_property_fields",
			"ALTER TABLE PropertyFields ADD COLUMN IF NOT EXISTS LinkedFieldID varchar(26)"},
	}
	if november {
		migrations = append(migrations, pending{
			169, "create_linked_field_id_index",
			"CREATE INDEX IF NOT EXISTS idx_propertyfields_linkedfieldid " +
				"ON PropertyFields (LinkedFieldID) WHERE LinkedFieldID IS NOT NULL AND DeleteAt = 0",
		})
	}
	for _, m := range migrations {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM db_migrations WHERE Version = $1 AND Name = $2)`,
			m.version, m.name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("apply missing core migration %d/%s: %w", m.version, m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO db_migrations (Version, Name) VALUES ($1, $2)`,
			m.version, m.name); err != nil {
			return fmt.Errorf("record missing core migration %d/%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

// Restored branch histories can contain both old and current migrations at the
// same version. Identify each row by (version, name), and validate the complete
// proposed history before archiving or changing anything. A lookup by version
// alone can select an unrelated row nondeterministically.
func validateLegacyHistory(ctx context.Context, tx *sql.Tx, archived, relocated []migrationVersion, required []database_migration.Entry) error {
	rows, err := tx.QueryContext(ctx, `SELECT Version, Name FROM db_migrations`)
	if err != nil {
		return err
	}
	var history []database_migration.Entry
	for rows.Next() {
		var entry database_migration.Entry
		if err := rows.Scan(&entry.Version, &entry.Name); err != nil {
			rows.Close()
			return err
		}
		history = append(history, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	expected, err := database_migration.Read(coredb.Assets())
	if err != nil {
		return err
	}
	counts := make(map[database_migration.Entry]int)
	var planned []database_migration.Entry
	for _, row := range history {
		counts[row]++
		archiveRow := false
		for _, entry := range archived {
			if row.Version == entry.old && row.Name == entry.name {
				archiveRow = true
				break
			}
		}
		if archiveRow {
			continue
		}
		for _, entry := range relocated {
			if row.Version == entry.old && row.Name == entry.name {
				row.Version = entry.new
				break
			}
		}
		planned = append(planned, row)
	}
	for _, entry := range append(append([]migrationVersion{}, archived...), relocated...) {
		if count := counts[database_migration.Entry{Version: entry.old, Name: entry.name}]; count != 1 {
			return fmt.Errorf("expected exactly one legacy migration %d/%s, found %d; no history changed", entry.old, entry.name, count)
		}
	}
	for _, entry := range required {
		if counts[entry] != 1 {
			return fmt.Errorf("expected exactly one migration %d/%s; no history changed", entry.Version, entry.Name)
		}
	}
	if err := database_migration.Validate(planned, expected); err != nil {
		return fmt.Errorf("validate reconciled legacy history: %w", err)
	}
	return nil
}

// A restored migration-history snapshot can reintroduce the six old names at
// versions 153-158 after the rest of the database has reached the current
// schema. Repair only this exact state; the schema changes were already
// applied under the previous version numbers.
func reconcileReintroducedLegacyRecords(ctx context.Context, tx *sql.Tx) (bool, error) {
	var hasArchive bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('db_migrations_legacy_versions') IS NOT NULL`).Scan(&hasArchive); err != nil {
		return false, err
	}
	if !hasArchive {
		return false, nil
	}

	var reintroduced bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM db_migrations WHERE Version = 153 AND Name = 'add_download_count'
	)`).Scan(&reintroduced); err != nil {
		return false, err
	}
	if !reintroduced {
		return false, nil
	}

	var appliedCount int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM db_migrations`).Scan(&appliedCount); err != nil {
		return false, err
	}
	if appliedCount != 227 {
		return false, fmt.Errorf("unexpected applied migration count: %d", appliedCount)
	}

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM db_migrations_legacy_versions`).Scan(&count); err != nil {
		return false, err
	}
	if count != len(branchOnly) {
		return false, fmt.Errorf("unexpected archived migration count: %d", count)
	}
	for _, entry := range branchOnly {
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT Name FROM db_migrations_legacy_versions WHERE Version = $1`, entry.old).Scan(&name); err != nil {
			return false, fmt.Errorf("read archived migration %d: %w", entry.old, err)
		}
		if name != entry.name {
			return false, fmt.Errorf("unexpected archived migration %d: %q", entry.old, name)
		}
	}
	for _, entry := range renumbered[:6] {
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT Name FROM db_migrations WHERE Version = $1`, entry.new).Scan(&name); err != nil {
			return false, fmt.Errorf("read migration %d: %w", entry.new, err)
		}
		if name != branchOnly[entry.new-153].name {
			return false, fmt.Errorf("unexpected migration %d: %q", entry.new, name)
		}
		var duplicate bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM db_migrations WHERE Name = $1)`, entry.name).Scan(&duplicate); err != nil {
			return false, err
		}
		if duplicate {
			return false, fmt.Errorf("migration %q is already recorded at another version", entry.name)
		}
	}
	for _, entry := range renumbered[6:] {
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT Name FROM db_migrations WHERE Version = $1`, entry.new).Scan(&name); err != nil {
			return false, fmt.Errorf("read migration %d: %w", entry.new, err)
		}
		if name != entry.name {
			return false, fmt.Errorf("unexpected migration %d: %q", entry.new, name)
		}
	}
	for _, entry := range []migrationVersion{
		{0, 167, "create_views_channel_id_delete_at_index"},
		{0, 168, "add_linked_field_id_to_property_fields"},
		{0, 169, "create_linked_field_id_index"},
		{0, 229, "add_access_control_policies_imports_index"},
	} {
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT Name FROM db_migrations WHERE Version = $1`, entry.new).Scan(&name); err != nil {
			return false, fmt.Errorf("read migration %d: %w", entry.new, err)
		}
		if name != entry.name {
			return false, fmt.Errorf("unexpected migration %d: %q", entry.new, name)
		}
	}
	// Validate all remaining identities before replacing the six old names.
	// A matching count and a few marker rows do not prove the rest is known.
	rows, err := tx.QueryContext(ctx, `SELECT Version,Name FROM db_migrations`)
	if err != nil {
		return false, err
	}
	var proposed []database_migration.Entry
	for rows.Next() {
		var row database_migration.Entry
		if err := rows.Scan(&row.Version, &row.Name); err != nil {
			rows.Close()
			return false, err
		}
		for _, entry := range renumbered[:6] {
			if row.Version == entry.new && row.Name == branchOnly[entry.new-153].name {
				row.Name = entry.name
				break
			}
		}
		proposed = append(proposed, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	expected, err := database_migration.Read(coredb.Assets())
	if err != nil {
		return false, err
	}
	if err := database_migration.Validate(proposed, expected); err != nil {
		return false, err
	}
	for _, entry := range renumbered[:6] {
		if _, err := tx.ExecContext(ctx, `UPDATE db_migrations SET Name = $1 WHERE Version = $2 AND Name = $3`, entry.name, entry.new, branchOnly[entry.new-153].name); err != nil {
			return false, err
		}
	}
	return true, nil
}
