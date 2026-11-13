package legal_hold

import (
	"context"
	"database/sql"
	"strings"
)

// Keep the twelve version-1 dataset names and the ability to reopen old frozen
// snapshots. New scope audit entries in events.jsonl carry event_type; their
// payload, phase and provenance are covered by the existing dataset hashes.
func scopeExportDatasets(ctx context.Context, tx *sql.Tx) ([]exportDatasetQuery, error) {
	installed, err := scopeJournalInstalled(ctx, tx)
	if err != nil || !installed {
		return exportDatasets, err
	}
	datasets := append([]exportDatasetQuery(nil), exportDatasets...)
	for i, dataset := range datasets {
		switch dataset.name {
		case "scopes.jsonl":
			datasets[i].query = `SELECT to_jsonb(r) FROM workspace_legal_hold.effective_case_scopes r WHERE case_id=$1 ORDER BY kind,target_id`
		case "events.jsonl":
			datasets[i].query = `SELECT data FROM (
	 SELECT to_jsonb(e) AS data,0 AS category,e.id AS ordering FROM workspace_legal_hold.case_events e WHERE case_id=$1
	 UNION ALL SELECT to_jsonb(r)||jsonb_build_object('event_type','scope_request'),1,0 FROM workspace_legal_hold.case_scope_requests r WHERE case_id=$1
	 UNION ALL SELECT to_jsonb(a)||jsonb_build_object('event_type','scope_action'),2,a.id FROM workspace_legal_hold.case_scope_actions a WHERE case_id=$1
	 UNION ALL SELECT to_jsonb(c)||jsonb_build_object('event_type','scope_complete'),3,0 FROM workspace_legal_hold.case_scope_completions c JOIN workspace_legal_hold.case_scope_requests r ON r.id=c.request_id WHERE r.case_id=$1
	 UNION ALL SELECT to_jsonb(m)||jsonb_build_object('event_type','scope_member'),4,0 FROM workspace_legal_hold.case_scope_members m WHERE case_id=$1
	 ) events ORDER BY category,ordering,data::text`
		default:
			query := strings.Replace(dataset.query,
				"versions AS (SELECT v.* FROM workspace_legal_hold.file_versions v JOIN files f USING(file_id))",
				`versions AS (SELECT v.* FROM workspace_legal_hold.file_versions v JOIN files f USING(file_id)
	 WHERE EXISTS(SELECT 1 FROM workspace_legal_hold.case_file_windows w WHERE w.case_id=$1 AND w.file_id=v.file_id
	 AND v.id>=w.version_floor AND (w.version_ceiling IS NULL OR v.id<=w.version_ceiling)))`, 1)
			query = strings.Replace(query,
				"links AS (SELECT m.* FROM workspace_legal_hold.mutation_files m JOIN files f USING(file_id))",
				`links AS (SELECT m.* FROM workspace_legal_hold.mutation_files m JOIN files f USING(file_id)
	 WHERE EXISTS(SELECT 1 FROM workspace_legal_hold.case_file_windows w JOIN workspace_legal_hold.case_scope_intent_order o ON o.intent_id=m.intent_id WHERE w.case_id=$1 AND w.file_id=m.file_id
	 AND o.sequence>=w.intent_floor AND (w.intent_ceiling IS NULL OR o.sequence<=w.intent_ceiling)))`, 1)
			datasets[i].query = query
		}
	}
	return datasets, nil
}
