package legal_hold

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

var ErrCaseReleased = errors.New("legal hold case is released; use its final export")

type CaseReleaseInfo struct {
	ActorID    string    `json:"actor_id"`
	RequestKey string    `json:"request_key"`
	Reason     string    `json:"reason"`
	ReleasedAt time.Time `json:"released_at"`
}

type CaseReleaseRequest struct {
	CaseID, RequestKey, Reason string
}

type CaseReleaseResult struct {
	CaseStatus
	ExportID string
}

func caseReleased(ctx context.Context, db exportQueryer, id string) (bool, error) {
	installed, err := lifecycleInstalled(ctx, db)
	if err != nil || !installed {
		return false, err
	}
	var released bool
	err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_legal_hold.case_releases WHERE case_id=$1)`, id).Scan(&released)
	return released, err
}

// Release atomically freezes a final export snapshot and closes future capture
// for the case. Existing evidence is retained forever by these primitives.
// A failed ZIP build can return state released: Resume the returned ExportID;
// the committed cutoff and release must not be undone or replaced.
func (m *CaseManager) Release(rctx request.CTX, exporter *CaseExporter, input CaseReleaseRequest) (result CaseReleaseResult, err error) {
	actor, err := m.authorize(rctx)
	if err != nil {
		return result, err
	}
	if exporter == nil || exporter.manager != m || !model.IsValidId(input.CaseID) || !model.IsValidId(input.RequestKey) {
		return result, errors.New("legal hold release requires a matching exporter and valid case/request IDs")
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if !utf8.ValidString(input.Reason) || utf8.RuneCountInString(input.Reason) < 1 || utf8.RuneCountInString(input.Reason) > 2048 {
		return result, errors.New("legal hold release requires a reason of one to 2048 characters")
	}
	ctx, cancel := context.WithTimeout(rctx.Context(), 10*time.Minute)
	defer cancel()
	if err = validateCaseLifecycle(ctx, m.journal.coordinator.fence.db); err != nil {
		return result, err
	}
	result, err = m.releaseSnapshot(ctx, rctx, exporter, input, actor)
	if err != nil {
		return result, err
	}
	_, err = exporter.Resume(rctx, result.ExportID)
	return result, err
}

func (m *CaseManager) releaseSnapshot(ctx context.Context, rctx request.CTX, e *CaseExporter, input CaseReleaseRequest, actor string) (CaseReleaseResult, error) {
	result := CaseReleaseResult{CaseStatus: CaseStatus{ID: input.CaseID}}
	// Same order as export: export-store lease, then archive lease, then SQL.
	lease, err := (&LocalVault{root: e.root}).mutationLease(ctx)
	if err != nil {
		return result, err
	}
	defer lease.Close()
	var key, reason, releasedBy string
	err = m.journal.coordinator.fence.db.QueryRowContext(ctx, `SELECT export_id,request_key,reason,actor_id FROM workspace_legal_hold.case_releases WHERE case_id=$1`, input.CaseID).Scan(&result.ExportID, &key, &reason, &releasedBy)
	if err == nil {
		result.State = "released"
		if key != input.RequestKey || reason != input.Reason || releasedBy != actor {
			return result, ErrCaseReleased
		}
		return result, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	request, err := e.Request(rctx, input.CaseID, input.RequestKey)
	if err != nil {
		return result, err
	}
	result.ExportID = request.ID
	c := m.journal.coordinator
	archiveLease, err := c.vault.mutationLease(ctx)
	if err != nil {
		return result, err
	}
	defer archiveLease.Close()
	if err = c.recoverLocked(ctx); err != nil {
		return result, err
	}
	tx, err := c.fence.begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_legal_hold.case_export_snapshots WHERE export_id=$1)`, request.ID).Scan(&exists); err != nil {
		return result, err
	}
	if exists {
		return result, errors.New("legal hold release key was already used for an ordinary export")
	}
	work := "staging/" + request.ID
	if err = e.root.RemoveAll(work); err != nil {
		return result, err
	}
	if err = e.root.Mkdir(work, 0o700); err != nil {
		return result, err
	}
	defer e.root.RemoveAll(work)
	release := &CaseReleaseInfo{ActorID: actor, RequestKey: input.RequestKey, Reason: input.Reason}
	manifest, err := e.snapshotTx(ctx, tx, work, request, actor, &exportBudget{ctx, e.limit}, true, release)
	if err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_releases(case_id,export_id,actor_id,request_key,reason,released_at) VALUES($1,$2,$3,$4,$5,$6)`, input.CaseID, request.ID, actor, input.RequestKey, input.Reason, manifest.CapturedAt); err != nil {
		return result, err
	}
	if _, err = m.authorize(rctx); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	result.State = "released"
	return result, nil
}

type CaseOverview struct {
	ID, Title, CreatedBy, State string
	CreatedAt                   time.Time
}

type CaseDetails struct {
	CaseOverview
	Scopes               []CaseScope
	Release              *CaseReleaseInfo
	FinalExportID        string
	PendingScopeChangeID string
}

// Get reads a consistent administrative view, including a release committed
// concurrently. It does not expose evidence bodies or caller-supplied roles.
func (m *CaseManager) Get(rctx request.CTX, id string) (details CaseDetails, err error) {
	if _, err = m.authorize(rctx); err != nil {
		return details, err
	}
	if !model.IsValidId(id) {
		return details, errors.New("invalid legal hold case ID")
	}
	ctx, cancel := context.WithTimeout(rctx.Context(), 10*time.Second)
	defer cancel()
	tx, err := m.journal.coordinator.fence.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return details, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT c.id,c.title,c.created_by,c.created_at,e.state FROM workspace_legal_hold.cases c
	 JOIN LATERAL(SELECT state FROM workspace_legal_hold.case_events WHERE case_id=c.id ORDER BY id DESC LIMIT 1)e ON true WHERE c.id=$1`, id).Scan(&details.ID, &details.Title, &details.CreatedBy, &details.CreatedAt, &details.State)
	if err != nil {
		return details, err
	}
	scopes := "workspace_legal_hold.case_scopes"
	if scoped, scopeErr := scopeJournalInstalled(ctx, tx); scopeErr != nil {
		return details, scopeErr
	} else if scoped {
		scopes = "workspace_legal_hold.effective_case_scopes"
		err = tx.QueryRowContext(ctx, `SELECT r.id FROM workspace_legal_hold.case_scope_requests r WHERE r.case_id=$1 AND NOT EXISTS(SELECT 1 FROM workspace_legal_hold.case_scope_completions WHERE request_id=r.id)`, id).Scan(&details.PendingScopeChangeID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return details, err
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT kind,target_id FROM "+scopes+" WHERE case_id=$1 ORDER BY kind,target_id", id)
	if err != nil {
		return details, err
	}
	for rows.Next() {
		var scope CaseScope
		if err = rows.Scan(&scope.Kind, &scope.TargetID); err != nil {
			break
		}
		details.Scopes = append(details.Scopes, scope)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return details, err
	}
	if installed, installErr := lifecycleInstalled(ctx, tx); installErr != nil {
		return details, installErr
	} else if installed {
		release := &CaseReleaseInfo{}
		err = tx.QueryRowContext(ctx, `SELECT export_id,actor_id,request_key,reason,released_at FROM workspace_legal_hold.case_releases WHERE case_id=$1`, id).Scan(&details.FinalExportID, &release.ActorID, &release.RequestKey, &release.Reason, &release.ReleasedAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return details, err
		}
		if err == nil {
			details.Release, details.State = release, "released"
		}
	}
	return details, tx.Commit()
}

// List uses created_at/id keyset pagination, at most 100 cases per call.
// Each page is a fresh view; new cases can appear on subsequent pages.
func (m *CaseManager) List(rctx request.CTX, afterID string, limit int) ([]CaseOverview, error) {
	if _, err := m.authorize(rctx); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 || afterID != "" && !model.IsValidId(afterID) {
		return nil, errors.New("legal hold case list requires a valid cursor and limit between one and 100")
	}
	ctx, cancel := context.WithTimeout(rctx.Context(), 10*time.Second)
	defer cancel()
	db := m.journal.coordinator.fence.db
	installed, err := lifecycleInstalled(ctx, db)
	if err != nil {
		return nil, err
	}
	state := "e.state"
	if installed {
		state = "CASE WHEN EXISTS(SELECT 1 FROM workspace_legal_hold.case_releases WHERE case_id=c.id) THEN 'released' ELSE e.state END"
	}
	if afterID != "" {
		var exists bool
		if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_legal_hold.cases WHERE id=$1)`, afterID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, sql.ErrNoRows
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT c.id,c.title,c.created_by,c.created_at,`+state+` FROM workspace_legal_hold.cases c
	 JOIN LATERAL(SELECT state FROM workspace_legal_hold.case_events WHERE case_id=c.id ORDER BY id DESC LIMIT 1)e ON true
	 WHERE $1='' OR (c.created_at,c.id)>(SELECT created_at,id FROM workspace_legal_hold.cases WHERE id=$1)
	 ORDER BY c.created_at,c.id LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CaseOverview
	for rows.Next() {
		var entry CaseOverview
		if err = rows.Scan(&entry.ID, &entry.Title, &entry.CreatedBy, &entry.CreatedAt, &entry.State); err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}
