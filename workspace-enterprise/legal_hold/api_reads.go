package legal_hold

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

type caseAPIOverview struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	State     string    `json:"state"`
}

func apiCaseOverview(c CaseOverview) caseAPIOverview {
	return caseAPIOverview{c.ID, c.Title, c.CreatedBy, c.CreatedAt, c.State}
}

type caseAPIDetails struct {
	caseAPIOverview
	Scopes               []caseAPIScope   `json:"scopes"`
	Release              *CaseReleaseInfo `json:"release,omitempty"`
	FinalExportID        string           `json:"final_export_id,omitempty"`
	PendingScopeChangeID string           `json:"pending_scope_change_id,omitempty"`
}

func apiCaseDetails(c CaseDetails) caseAPIDetails {
	scopes := make([]caseAPIScope, len(c.Scopes))
	for i, s := range c.Scopes {
		scopes[i] = caseAPIScope{s.Kind, s.TargetID}
	}
	return caseAPIDetails{apiCaseOverview(c.CaseOverview), scopes, c.Release, c.FinalExportID, c.PendingScopeChangeID}
}

type caseAPIExportStatus struct {
	ID        string    `json:"id"`
	CaseID    string    `json:"case_id"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	Size      int64     `json:"size,omitempty"`
	SHA256    string    `json:"sha256,omitempty"`
}
type caseAPIExportPage struct {
	Items []caseAPIExportStatus `json:"items"`
	Next  string                `json:"next_id"`
}

// A completed receipt wins over old failures. Otherwise the latest attempt's
// durable outcome is reported; a crashed writer remains preparing, not success.
const caseAPIExportsQuery = `WITH receipts AS (
 SELECT export_id,sha256,size FROM workspace_legal_hold.case_export_attempt_outcomes WHERE state='complete'
 UNION ALL SELECT export_id,sha256,size FROM workspace_legal_hold.case_export_outcomes WHERE state='complete')
 SELECT r.id,r.case_id,r.created_at,CASE WHEN receipt.export_id IS NOT NULL THEN 'complete'
 ELSE COALESCE(attempt.state,legacy.state,'preparing') END,COALESCE(receipt.sha256,''),COALESCE(receipt.size,0)
 FROM workspace_legal_hold.case_exports r
 LEFT JOIN LATERAL(SELECT * FROM receipts WHERE export_id=r.id LIMIT 1) receipt ON true
 LEFT JOIN LATERAL(SELECT COALESCE(o.state,'preparing') AS state FROM workspace_legal_hold.case_export_attempts a
 LEFT JOIN workspace_legal_hold.case_export_attempt_outcomes o ON o.attempt_id=a.id WHERE a.export_id=r.id ORDER BY a.created_at DESC,a.id DESC LIMIT 1) attempt ON true
 LEFT JOIN workspace_legal_hold.case_export_outcomes legacy ON legacy.export_id=r.id`

func (a *CaseAPI) getAPIExport(ctx context.Context, id string) (result caseAPIExportStatus, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = a.audit.db.QueryRowContext(ctx, caseAPIExportsQuery+` WHERE r.id=$1`, id).Scan(&result.ID, &result.CaseID, &result.CreatedAt, &result.State, &result.SHA256, &result.Size)
	return result, err
}
func (a *CaseAPI) listAPIExports(ctx context.Context, caseID, after string, limit int) (caseAPIExportPage, error) {
	result := caseAPIExportPage{Items: []caseAPIExportStatus{}}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var exists bool
	if err := a.audit.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_legal_hold.cases WHERE id=$1) AND ($2='' OR EXISTS(SELECT 1 FROM workspace_legal_hold.case_exports WHERE id=$2 AND case_id=$1))`, caseID, after).Scan(&exists); err != nil {
		return result, err
	}
	if !exists {
		return result, sql.ErrNoRows
	}
	rows, err := a.audit.db.QueryContext(ctx, caseAPIExportsQuery+` WHERE r.case_id=$1 AND ($2='' OR (r.created_at,r.id)>(SELECT created_at,id FROM workspace_legal_hold.case_exports WHERE id=$2)) ORDER BY r.created_at,r.id LIMIT $3`, caseID, after, limit)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var item caseAPIExportStatus
		if err = rows.Scan(&item.ID, &item.CaseID, &item.CreatedAt, &item.State, &item.SHA256, &item.Size); err != nil {
			return result, err
		}
		result.Items = append(result.Items, item)
	}
	if len(result.Items) == limit {
		result.Next = result.Items[len(result.Items)-1].ID
	}
	return result, rows.Err()
}

type caseAPIAuditPage struct {
	Items []json.RawMessage `json:"items"`
	Next  string            `json:"next_cursor"`
}

func (a *CaseAPI) listAPIAudit(ctx context.Context, caseID, after string, limit int) (caseAPIAuditPage, error) {
	result := caseAPIAuditPage{Items: []json.RawMessage{}}
	var cursor int64
	var err error
	if after != "" {
		cursor, err = strconv.ParseInt(after, 10, 64)
	}
	if err != nil || cursor < 0 || caseID != "" && !model.IsValidId(caseID) {
		return result, errAPIInvalidRequest
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := a.audit.db.QueryContext(ctx, `SELECT r.sequence,jsonb_build_object(
	 'id',r.id,'sequence',r.sequence,'operation',r.operation,'method',r.method,'started_at',r.started_at,
	 'identity',to_jsonb(i),'action',to_jsonb(a),'download',to_jsonb(d),'result',to_jsonb(o))
	 FROM workspace_legal_hold.api_requests r LEFT JOIN workspace_legal_hold.api_identities i ON i.request_id=r.id
	 LEFT JOIN workspace_legal_hold.api_actions a ON a.request_id=r.id
	 LEFT JOIN workspace_legal_hold.api_downloads d ON d.request_id=r.id
	 LEFT JOIN workspace_legal_hold.api_results o ON o.request_id=r.id
	 LEFT JOIN workspace_legal_hold.case_start_keys k ON k.actor_id=i.actor_id AND k.request_key=a.request_key AND r.operation='start_case'
	 WHERE r.sequence>$1 AND ($2='' OR COALESCE(a.case_id,o.case_id,k.case_id)=$2)
	 ORDER BY r.sequence LIMIT $3`, cursor, caseID, limit)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	var sequence int64
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&sequence, &data); err != nil {
			return result, err
		}
		result.Items = append(result.Items, json.RawMessage(data))
	}
	if len(result.Items) == limit {
		result.Next = strconv.FormatInt(sequence, 10)
	}
	return result, rows.Err()
}

var errAPIInvalidRequest = errors.New("invalid legal hold API request")
