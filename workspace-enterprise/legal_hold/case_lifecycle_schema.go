package legal_hold

import (
	"context"
	"errors"
	"strings"
	"time"
)

// InstallCaseLifecycle is explicit, before serving requests. Existing file
// reservations have unknown provenance and are conservatively imported as
// independent holds; an upgrade must never silently release one of them.
func InstallCaseLifecycle(ctx context.Context, journal *CaseJournal) error {
	if err := InstallCaseExportJournal(ctx, journal); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := journal.coordinator
	lease, err := c.vault.mutationLease(ctx)
	if err != nil {
		return err
	}
	defer lease.Close()
	tx, err := c.fence.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var installed bool
	if err = tx.QueryRowContext(ctx, `SELECT to_regclass('workspace_legal_hold.case_lifecycle_installation') IS NOT NULL`).Scan(&installed); err != nil {
		return err
	}
	if !installed {
		schema := caseLifecycleSchema
		// Keep a single source for each capture function's behavior. Add the
		// release gate to all four entry points, including direct SQL capture,
		// before any reservation, pending-intent check, or nested snapshot.
		for _, name := range []string{"capture_case_object", "capture_case_file", "capture_case_post", "capture_case_temporary"} {
			function, err := caseVoidFunction(name)
			if err != nil {
				return err
			}
			function = strings.Replace(function, "BEGIN\n", "BEGIN\n IF NOT workspace_legal_hold.case_is_open(c) THEN RETURN; END IF;\n", 1)
			if name == "capture_case_file" {
				start := strings.Index(function, " IF reserve THEN\n")
				end := strings.LastIndex(function, " END IF;\n")
				if start < 0 || end < start {
					return errors.New("legal hold capture reservation definition is incomplete")
				}
				function = function[:start] + " IF reserve THEN PERFORM workspace_legal_hold.reserve_file_metadata(f->>'id',f,false); END IF;\n" + function[end+len(" END IF;\n"):]
			}
			schema += function
		}
		guards := strings.Replace(fileNativeGuardsSQL,
			"IF NOT EXISTS(SELECT 1 FROM workspace_legal_hold.file_reservations WHERE file_id=OLD.id) THEN",
			"IF NOT workspace_legal_hold.file_preservation_required(OLD.id) THEN", 1)
		schema += guards
		for name, source := range journal.sources {
			schema = strings.ReplaceAll(schema, "{{"+name+"}}", source)
		}
		if _, err = tx.ExecContext(ctx, schema); err != nil {
			return err
		}
	}
	if err = validateCaseLifecycle(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func caseVoidFunction(name string) (string, error) {
	start := strings.Index(caseSchema, "CREATE FUNCTION workspace_legal_hold."+name+"(")
	if start < 0 {
		return "", errors.New("legal hold capture function is missing")
	}
	end := strings.Index(caseSchema[start:], "END $$;")
	if end < 0 {
		return "", errors.New("legal hold capture function definition is incomplete")
	}
	return strings.Replace(caseSchema[start:start+end+len("END $$;")], "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1) + "\n", nil
}

func validateCaseLifecycle(ctx context.Context, db exportQueryer) error {
	var intact bool
	err := db.QueryRowContext(ctx, `WITH expected(relation,name) AS (
	 SELECT to_regclass('workspace_legal_hold.'||r)::oid,t FROM unnest(ARRAY['case_lifecycle_installation','case_releases','independent_file_holds']) r
	 CROSS JOIN unnest(ARRAY['immutable_rows','immutable_truncate']) t
	 UNION ALL VALUES('workspace_legal_hold.case_releases'::regclass::oid,'validate_release'),
	 ('workspace_legal_hold.case_members'::regclass::oid,'acquire_file_generation'))
	 SELECT (SELECT count(*)=1 AND bool_and(version=1) FROM workspace_legal_hold.case_lifecycle_installation)
	 AND count(*)=8 AND bool_and(t.oid IS NOT NULL AND t.tgenabled='A')
	 FROM expected e LEFT JOIN pg_trigger t ON t.tgrelid=e.relation AND t.tgname=e.name`).Scan(&intact)
	if err != nil || !intact {
		return errors.Join(errors.New("legal hold case lifecycle journal or triggers are incomplete"), err)
	}
	return nil
}

func lifecycleInstalled(ctx context.Context, db exportQueryer) (bool, error) {
	var installed bool
	err := db.QueryRowContext(ctx, `SELECT to_regclass('workspace_legal_hold.case_lifecycle_installation') IS NOT NULL`).Scan(&installed)
	return installed, err
}

const caseLifecycleSchema = `
CREATE TABLE workspace_legal_hold.case_lifecycle_installation(version integer PRIMARY KEY CHECK(version=1));
INSERT INTO workspace_legal_hold.case_lifecycle_installation VALUES(1);
CREATE INDEX ON workspace_legal_hold.cases(created_at,id);
CREATE TABLE workspace_legal_hold.independent_file_holds(file_id varchar(26) PRIMARY KEY REFERENCES workspace_legal_hold.file_reservations(file_id),origin text NOT NULL CHECK(origin IN('legacy','explicit')),created_at timestamptz NOT NULL DEFAULT clock_timestamp());
INSERT INTO workspace_legal_hold.independent_file_holds(file_id,origin) SELECT file_id,'legacy' FROM workspace_legal_hold.file_reservations;
CREATE TABLE workspace_legal_hold.case_releases(case_id varchar(26) PRIMARY KEY REFERENCES workspace_legal_hold.cases(id),export_id varchar(26) NOT NULL UNIQUE REFERENCES workspace_legal_hold.case_export_snapshots(export_id),actor_id varchar(26) NOT NULL,request_key varchar(26) NOT NULL,reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 2048),released_at timestamptz NOT NULL);
DO $$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['case_lifecycle_installation','independent_file_holds','case_releases'] LOOP
  EXECUTE format('CREATE TRIGGER immutable_rows BEFORE UPDATE OR DELETE ON workspace_legal_hold.%I FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('CREATE TRIGGER immutable_truncate BEFORE TRUNCATE ON workspace_legal_hold.%I FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.reject_journal_mutation()',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_rows',name);
  EXECUTE format('ALTER TABLE workspace_legal_hold.%I ENABLE ALWAYS TRIGGER immutable_truncate',name);
 END LOOP;
END $$;
CREATE FUNCTION workspace_legal_hold.case_is_open(c text) RETURNS boolean LANGUAGE sql VOLATILE SET search_path=pg_catalog AS $$
 SELECT EXISTS(SELECT 1 FROM workspace_legal_hold.cases WHERE id=c) AND NOT EXISTS(SELECT 1 FROM workspace_legal_hold.case_releases WHERE case_id=c)
$$;
CREATE FUNCTION workspace_legal_hold.file_preservation_required(i text) RETURNS boolean LANGUAGE sql VOLATILE SET search_path=pg_catalog AS $$
 SELECT EXISTS(SELECT 1 FROM workspace_legal_hold.independent_file_holds WHERE file_id=i)
 OR EXISTS(SELECT 1 FROM workspace_legal_hold.case_members m WHERE m.kind='fileinfo' AND m.entity_id=i AND workspace_legal_hold.case_is_open(m.case_id))
$$;
CREATE FUNCTION workspace_legal_hold.reserve_file_metadata(i text,f jsonb,independent boolean,new_generation boolean DEFAULT false) RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE latest workspace_legal_hold.file_versions%ROWTYPE; reacquired boolean;
BEGIN
 -- A metadata-identical live file can contain different bytes after an
 -- unheld interval. A new acquisition must not trust its historical receipt.
 reacquired:=new_generation OR (independent AND NOT workspace_legal_hold.file_preservation_required(i));
 INSERT INTO workspace_legal_hold.file_reservations(file_id) VALUES(i) ON CONFLICT DO NOTHING;
 IF independent THEN INSERT INTO workspace_legal_hold.independent_file_holds VALUES(i,'explicit',clock_timestamp()) ON CONFLICT DO NOTHING; END IF;
 SELECT v.* INTO latest FROM workspace_legal_hold.file_versions v WHERE v.file_id=i ORDER BY id DESC LIMIT 1;
 IF NOT FOUND THEN
  INSERT INTO workspace_legal_hold.file_versions(file_id,metadata) VALUES(i,f);
 ELSIF latest.metadata IS DISTINCT FROM f OR reacquired THEN
  IF EXISTS(SELECT 1 FROM workspace_legal_hold.file_receipts WHERE version_id=latest.id) THEN
   INSERT INTO workspace_legal_hold.file_versions(file_id,metadata) VALUES(i,f);
  ELSIF latest.metadata IS DISTINCT FROM f THEN
   RAISE EXCEPTION 'legal hold previous file preservation is pending' USING ERRCODE='55000';
  END IF;
 END IF;
END $$;
CREATE FUNCTION workspace_legal_hold.acquire_case_file_generation() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE f jsonb;
BEGIN
 IF NEW.kind<>'fileinfo' THEN RETURN NEW; END IF;
 PERFORM workspace_legal_hold.case_barrier();
 IF NOT workspace_legal_hold.case_is_open(NEW.case_id)
 OR NOT EXISTS(SELECT 1 FROM workspace_legal_hold.file_reservations WHERE file_id=NEW.entity_id)
 OR workspace_legal_hold.file_preservation_required(NEW.entity_id) THEN RETURN NEW; END IF;
 -- This must run before the membership itself makes the file look held.
 -- Do not reserve fresh uploads here: derivatives may still be incomplete.
 PERFORM workspace_legal_hold.lock_case_entity('fileinfo',NEW.entity_id);
 IF workspace_legal_hold.file_preservation_required(NEW.entity_id) THEN RETURN NEW; END IF;
 PERFORM workspace_legal_hold.check_file_mutations();
 SELECT to_jsonb(src) INTO f FROM {{fileinfo}} src WHERE id=NEW.entity_id;
 IF FOUND THEN PERFORM workspace_legal_hold.reserve_file_metadata(NEW.entity_id,f,false,true); END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER acquire_file_generation BEFORE INSERT ON workspace_legal_hold.case_members FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.acquire_case_file_generation();
ALTER TABLE workspace_legal_hold.case_members ENABLE ALWAYS TRIGGER acquire_file_generation;
CREATE FUNCTION workspace_legal_hold.refresh_case_file_metadata(i text) RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE f jsonb;
BEGIN
 SELECT to_jsonb(src) INTO f FROM {{fileinfo}} src WHERE id=i;
 IF FOUND THEN PERFORM workspace_legal_hold.reserve_file_metadata(i,f,false); END IF;
END $$;
CREATE FUNCTION workspace_legal_hold.validate_case_release() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 PERFORM workspace_legal_hold.case_barrier();
 PERFORM workspace_legal_hold.check_file_mutations();
 IF NOT EXISTS(SELECT 1 FROM workspace_legal_hold.case_events WHERE case_id=NEW.case_id AND state='active')
 OR NOT EXISTS(SELECT 1 FROM workspace_legal_hold.case_export_snapshots s WHERE s.export_id=NEW.export_id
  AND s.manifest->>'case_id'=NEW.case_id AND s.manifest->'release'->>'actor_id'=NEW.actor_id
  AND s.manifest->'release'->>'request_key'=NEW.request_key AND s.manifest->'release'->>'reason'=NEW.reason
  AND (s.manifest->'release'->>'released_at')::timestamptz=NEW.released_at) THEN
  RAISE EXCEPTION 'legal hold release lacks a matching final snapshot' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER validate_release BEFORE INSERT ON workspace_legal_hold.case_releases FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.validate_case_release();
ALTER TABLE workspace_legal_hold.case_releases ENABLE ALWAYS TRIGGER validate_release;
`
