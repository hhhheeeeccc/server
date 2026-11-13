package autotranslation

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lib/pq"
)

// InstallPostLifecycle ties derived post text to its source, including deletion
// paths which do not pass through an Enterprise interface. It is database
// hygiene, independent of whether translation is currently licensed or enabled.
// The central startup registration must finish it before serving APIs.
func InstallPostLifecycle(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		"SET LOCAL lock_timeout='5s'",
		"SET LOCAL statement_timeout='30s'",
		"SELECT pg_advisory_xact_lock(6492261407)",
	} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("lock translation lifecycle installation: %w", err)
		}
	}
	// Resolve the same unqualified relations the store uses, then qualify every
	// reference in persistent functions. A later search_path must not redirect it.
	tables := make(map[string]string)
	var functionSchema string
	for _, name := range []string{"posts", "translations"} {
		var schema, relation string
		if err = tx.QueryRowContext(ctx, `SELECT n.nspname,c.relname FROM pg_class c
			JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE c.oid=to_regclass($1) AND c.relkind='r'`, name).Scan(&schema, &relation); err != nil {
			return fmt.Errorf("resolve %s for translation lifecycle: %w", name, err)
		}
		tables[name] = pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(relation)
		if name == "translations" {
			functionSchema = pq.QuoteIdentifier(schema)
		}
	}
	posts, translations := tables["posts"], tables["translations"]
	// Block source/translation writes during installation and removal of legacy
	// orphans. Readers remain available. DDL and cleanup either all commit or all
	// roll back; concurrent nodes serialize on the transaction advisory lock.
	for _, table := range []string{posts, translations} {
		if _, err = tx.ExecContext(ctx, "LOCK TABLE "+table+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return fmt.Errorf("lock translation lifecycle tables: %w", err)
		}
	}
	guard := functionSchema + ".workspace_translation_source_guard"
	deletePost := functionSchema + ".workspace_delete_post_translations"
	truncatePosts := functionSchema + ".workspace_truncate_post_translations"
	orderEdits := functionSchema + ".workspace_order_post_edits"
	statements := []string{
		// Native edit events use UpdateAt to reject out-of-order delivery. Two
		// source writes can share a millisecond or arrive from skewed node clocks.
		// PostgreSQL holds the row lock here, so the committed source versions
		// remain strictly ordered. No provider request holds this lock.
		`CREATE OR REPLACE FUNCTION ` + orderEdits + `() RETURNS trigger
		LANGUAGE plpgsql SET search_path=pg_catalog AS $workspace$
		BEGIN
			IF OLD.message IS DISTINCT FROM NEW.message THEN
				NEW.updateat=GREATEST(NEW.updateat,OLD.updateat+1);
			ELSE
				NEW.updateat=GREATEST(NEW.updateat,OLD.updateat);
			END IF;
			RETURN NEW;
		END
		$workspace$`,
		`CREATE OR REPLACE FUNCTION ` + guard + `() RETURNS trigger
		LANGUAGE plpgsql SET search_path=pg_catalog AS $workspace$
		BEGIN
			IF TG_OP='UPDATE' THEN
				IF (OLD.objecttype='post' OR NEW.objecttype='post') AND
					(OLD.objectid IS DISTINCT FROM NEW.objectid OR OLD.objecttype IS DISTINCT FROM NEW.objecttype) THEN
					RAISE EXCEPTION 'post translation source identity is immutable' USING ERRCODE='23514';
				END IF;
			ELSIF NEW.objecttype='post' THEN
				PERFORM 1 FROM ` + posts + ` WHERE id=NEW.objectid AND deleteat=0 FOR SHARE;
				IF NOT FOUND THEN
					RAISE EXCEPTION 'post translation requires a live source post' USING ERRCODE='23503';
				END IF;
			END IF;
			RETURN NEW;
		END
		$workspace$`,
		`CREATE OR REPLACE FUNCTION ` + deletePost + `() RETURNS trigger
		LANGUAGE plpgsql SET search_path=pg_catalog AS $workspace$
		BEGIN
			IF TG_OP='DELETE' THEN
				DELETE FROM ` + translations + ` WHERE objecttype='post' AND objectid=OLD.id;
			ELSIF NEW.deleteat<>0 THEN
				DELETE FROM ` + translations + ` WHERE objecttype='post' AND objectid=NEW.id;
			ELSIF OLD.message IS DISTINCT FROM NEW.message THEN
				UPDATE ` + translations + ` SET text='',normhash='',state='processing',confidence=NULL,
					updateat=floor(extract(epoch FROM clock_timestamp())*1000)::bigint,
					meta=CASE WHEN jsonb_typeof(meta)='object' THEN meta-'workspace_retry'-'workspace_publish_v1'-'error'-'src_lang'
						ELSE jsonb_build_object('type','string') END
				WHERE objecttype='post' AND objectid=NEW.id;
			END IF;
			RETURN NULL;
		END
		$workspace$`,
		`CREATE OR REPLACE FUNCTION ` + truncatePosts + `() RETURNS trigger
		LANGUAGE plpgsql SET search_path=pg_catalog AS $workspace$
		BEGIN
			DELETE FROM ` + translations + ` WHERE objecttype='post';
			RETURN NULL;
		END
		$workspace$`,
		`DROP TRIGGER IF EXISTS workspace_translation_source_guard ON ` + translations,
		`DROP TRIGGER IF EXISTS workspace_order_post_edits ON ` + posts,
		`CREATE TRIGGER workspace_order_post_edits BEFORE UPDATE ON ` + posts + `
		FOR EACH ROW EXECUTE FUNCTION ` + orderEdits + `()`,
		`CREATE TRIGGER workspace_translation_source_guard BEFORE INSERT OR UPDATE OF objectid,objecttype ON ` + translations + `
		FOR EACH ROW EXECUTE FUNCTION ` + guard + `()`,
		`DROP TRIGGER IF EXISTS workspace_delete_post_translations ON ` + posts,
		`CREATE TRIGGER workspace_delete_post_translations AFTER DELETE OR UPDATE OF deleteat,message ON ` + posts + `
		FOR EACH ROW EXECUTE FUNCTION ` + deletePost + `()`,
		`DROP TRIGGER IF EXISTS workspace_truncate_post_translations ON ` + posts,
		`CREATE TRIGGER workspace_truncate_post_translations AFTER TRUNCATE ON ` + posts + `
		FOR EACH STATEMENT EXECUTE FUNCTION ` + truncatePosts + `()`,
		`DELETE FROM ` + translations + ` t WHERE t.objecttype='post'
		AND NOT EXISTS (SELECT 1 FROM ` + posts + ` p WHERE p.id=t.objectid AND p.deleteat=0)`,
	}
	// Insertion takes the source SHARE lock before the translation row lock.
	// Updates to an existing translation's payload do not lock the source: the
	// deleting transaction will remove that row after the update commits. This
	// avoids reversing the lock order used by post deletion/recovery updates.
	for _, statement := range statements {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("install translation lifecycle: %w", err)
		}
	}
	return tx.Commit()
}
