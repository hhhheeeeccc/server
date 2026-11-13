package legal_hold

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

var (
	ErrCaseScopeKeyConflict = errors.New("legal hold scope request key belongs to another request")
	ErrCaseScopePending     = errors.New("legal hold case has an unfinished scope change")
)

type CaseScopeChangeRequest struct {
	CaseID, RequestKey, Reason string
	Add, Remove                []CaseScope
}

type CaseScopeChangeStatus struct {
	ID, CaseID, State string
}

// ChangeScope commits additions and their snapshots first, retaining every old
// scope. Removals take effect only when all active attachment evidence is sealed.
// A failed second phase returns the durable request in state preparing. Retrying
// the same actor/key/payload resumes it; completed requests never capture anew.
func (m *CaseManager) ChangeScope(rctx request.CTX, input CaseScopeChangeRequest) (CaseScopeChangeStatus, error) {
	actor, err := m.authorize(rctx)
	if err != nil {
		return CaseScopeChangeStatus{}, err
	}
	input, payload, err := normalizeScopeChange(input)
	if err != nil {
		return CaseScopeChangeStatus{}, err
	}
	ctx, cancel := context.WithTimeout(rctx.Context(), 2*time.Minute)
	defer cancel()
	c := m.journal.coordinator
	if err = validateCaseScopeJournal(ctx, c.fence.db); err != nil {
		return CaseScopeChangeStatus{}, err
	}
	lease, err := c.vault.mutationLease(ctx)
	if err != nil {
		return CaseScopeChangeStatus{}, err
	}
	defer lease.Close()
	if err = c.recoverLocked(ctx); err != nil {
		return CaseScopeChangeStatus{}, err
	}
	status, exists, err := m.existingScopeChange(ctx, actor, input, payload)
	if err != nil {
		return status, err
	}
	if exists {
		if err = m.reauthorize(rctx, actor); err != nil {
			return status, err
		}
		if status.State == "complete" {
			return status, nil
		}
		return m.completeScopeChangeLocked(ctx, rctx, actor, status)
	}
	tx, err := c.fence.begin(ctx)
	if err != nil {
		return CaseScopeChangeStatus{}, err
	}
	defer tx.Rollback()
	status, err = m.prepareScopeChangeTx(ctx, tx, actor, input, payload)
	if err != nil {
		return CaseScopeChangeStatus{}, err
	}
	if err = m.reauthorize(rctx, actor); err != nil {
		return CaseScopeChangeStatus{}, err
	}
	if err = tx.Commit(); err != nil {
		return CaseScopeChangeStatus{}, err
	}
	return m.completeScopeChangeLocked(ctx, rctx, actor, status)
}

func normalizeScopeChange(input CaseScopeChangeRequest) (CaseScopeChangeRequest, []byte, error) {
	input.Reason = strings.TrimSpace(input.Reason)
	if !model.IsValidId(input.CaseID) || !model.IsValidId(input.RequestKey) || !utf8.ValidString(input.Reason) || utf8.RuneCountInString(input.Reason) < 1 || utf8.RuneCountInString(input.Reason) > 2048 || len(input.Add)+len(input.Remove) < 1 || len(input.Add)+len(input.Remove) > 256 {
		return input, nil, errors.New("legal hold scope change requires valid IDs, a reason, and one to 256 distinct changes")
	}
	type scope struct {
		Kind     string `json:"kind"`
		TargetID string `json:"target_id"`
	}
	seen := map[CaseScope]bool{}
	canonical := func(items []CaseScope) ([]scope, error) {
		result := make([]scope, 0, len(items))
		for _, item := range items {
			if (item.Kind != "users" && item.Kind != "channels") || !model.IsValidId(item.TargetID) || seen[item] {
				return nil, errors.New("invalid, duplicated, or overlapping legal hold scope change")
			}
			seen[item] = true
			result = append(result, scope{item.Kind, item.TargetID})
		}
		sort.Slice(result, func(i, j int) bool {
			if result[i].Kind == result[j].Kind {
				return result[i].TargetID < result[j].TargetID
			}
			return result[i].Kind < result[j].Kind
		})
		return result, nil
	}
	add, err := canonical(input.Add)
	if err != nil {
		return input, nil, err
	}
	remove, err := canonical(input.Remove)
	if err != nil {
		return input, nil, err
	}
	payload, err := json.Marshal(struct {
		CaseID string  `json:"case_id"`
		Reason string  `json:"reason"`
		Add    []scope `json:"add"`
		Remove []scope `json:"remove"`
	}{input.CaseID, input.Reason, add, remove})
	return input, payload, err
}

func (m *CaseManager) existingScopeChange(ctx context.Context, actor string, input CaseScopeChangeRequest, payload []byte) (CaseScopeChangeStatus, bool, error) {
	var result CaseScopeChangeStatus
	var matches bool
	err := m.journal.coordinator.fence.db.QueryRowContext(ctx, `SELECT r.id,r.case_id,r.payload=$3::jsonb,
	 CASE WHEN EXISTS(SELECT 1 FROM workspace_legal_hold.case_scope_completions WHERE request_id=r.id) THEN 'complete' ELSE 'preparing' END
	 FROM workspace_legal_hold.case_scope_requests r WHERE actor_id=$1 AND request_key=$2`, actor, input.RequestKey, string(payload)).Scan(&result.ID, &result.CaseID, &matches, &result.State)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err == nil && !matches {
		err = ErrCaseScopeKeyConflict
	}
	return result, err == nil, err
}

// ResumeScopeChange lets another currently authorized administrator finish a
// durable request if its creator is unavailable. It never changes the original
// payload or key owner; the completion records the actual resuming actor.
func (m *CaseManager) ResumeScopeChange(rctx request.CTX, requestID string) (CaseScopeChangeStatus, error) {
	actor, err := m.authorize(rctx)
	if err != nil {
		return CaseScopeChangeStatus{}, err
	}
	if !model.IsValidId(requestID) {
		return CaseScopeChangeStatus{}, errors.New("invalid legal hold scope change ID")
	}
	ctx, cancel := context.WithTimeout(rctx.Context(), 2*time.Minute)
	defer cancel()
	c := m.journal.coordinator
	if err = validateCaseScopeJournal(ctx, c.fence.db); err != nil {
		return CaseScopeChangeStatus{}, err
	}
	lease, err := c.vault.mutationLease(ctx)
	if err != nil {
		return CaseScopeChangeStatus{}, err
	}
	defer lease.Close()
	if err = c.recoverLocked(ctx); err != nil {
		return CaseScopeChangeStatus{}, err
	}
	status := CaseScopeChangeStatus{ID: requestID}
	err = c.fence.db.QueryRowContext(ctx, `SELECT r.case_id,CASE WHEN EXISTS(SELECT 1 FROM workspace_legal_hold.case_scope_completions WHERE request_id=r.id) THEN 'complete' ELSE 'preparing' END FROM workspace_legal_hold.case_scope_requests r WHERE id=$1`, requestID).Scan(&status.CaseID, &status.State)
	if err != nil {
		return CaseScopeChangeStatus{}, err
	}
	if err = m.reauthorize(rctx, actor); err != nil {
		return status, err
	}
	if status.State == "complete" {
		return status, nil
	}
	return m.completeScopeChangeLocked(ctx, rctx, actor, status)
}

func (m *CaseManager) prepareScopeChangeTx(ctx context.Context, tx *sql.Tx, actor string, input CaseScopeChangeRequest, payload []byte) (CaseScopeChangeStatus, error) {
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT workspace_legal_hold.case_scope_pending($1)`, input.CaseID).Scan(&pending); err != nil {
		return CaseScopeChangeStatus{}, err
	}
	if pending {
		return CaseScopeChangeStatus{}, ErrCaseScopePending
	}
	for _, scope := range input.Add {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+m.journal.sources[scope.Kind]+" WHERE id=$1)", scope.TargetID).Scan(&exists); err != nil {
			return CaseScopeChangeStatus{}, err
		}
		if !exists {
			return CaseScopeChangeStatus{}, ErrCaseScopeNotFound
		}
	}
	status := CaseScopeChangeStatus{ID: model.NewId(), CaseID: input.CaseID, State: "preparing"}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_scope_requests(id,case_id,actor_id,request_key,payload) VALUES($1,$2,$3,$4,$5::jsonb)`, status.ID, status.CaseID, actor, input.RequestKey, string(payload)); err != nil {
		return CaseScopeChangeStatus{}, err
	}
	for _, scope := range input.Add {
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_scope_actions(request_id,case_id,kind,target_id,action) VALUES($1,$2,$3,$4,'add')`, status.ID, status.CaseID, scope.Kind, scope.TargetID); err != nil {
			return CaseScopeChangeStatus{}, err
		}
		if _, err := tx.ExecContext(ctx, "SELECT workspace_legal_hold.capture_case_object($1,$2,id,to_jsonb(src),'snapshot') FROM "+m.journal.sources[scope.Kind]+" src WHERE id=$3", status.CaseID, scope.Kind, scope.TargetID); err != nil {
			return CaseScopeChangeStatus{}, err
		}
	}
	if err := m.snapshotScopePostsTx(ctx, tx, status.CaseID); err != nil {
		return CaseScopeChangeStatus{}, err
	}
	return status, nil
}

func (m *CaseManager) snapshotScopePostsTx(ctx context.Context, tx *sql.Tx, caseID string) error {
	if _, err := tx.ExecContext(ctx, "SELECT workspace_legal_hold.capture_case_post($1,to_jsonb(p),'snapshot') FROM "+m.journal.sources["posts"]+` p WHERE EXISTS(
	 SELECT 1 FROM workspace_legal_hold.effective_case_scopes s WHERE s.case_id=$1 AND
	 ((s.kind='users' AND s.target_id=p.userid) OR (s.kind='channels' AND s.target_id=p.channelid)))`, caseID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "SELECT workspace_legal_hold.capture_case_file($1,to_jsonb(f),true) FROM "+m.journal.sources["fileinfo"]+` f
	 WHERE EXISTS(SELECT 1 FROM workspace_legal_hold.file_cases(to_jsonb(f)) c WHERE c.case_id=$1)`, caseID)
	return err
}

func (m *CaseManager) completeScopeChangeLocked(ctx context.Context, rctx request.CTX, actor string, status CaseScopeChangeStatus) (CaseScopeChangeStatus, error) {
	tx, err := m.journal.coordinator.fence.begin(ctx)
	if err != nil {
		return status, err
	}
	defer tx.Rollback()
	if err = m.completeScopeChangeTx(ctx, tx, actor, status); err != nil {
		return status, err
	}
	if err = m.reauthorize(rctx, actor); err != nil {
		return status, err
	}
	if err = tx.Commit(); err != nil {
		return status, err
	}
	status.State = "complete"
	return status, nil
}

func (m *CaseManager) completeScopeChangeTx(ctx context.Context, tx *sql.Tx, actor string, status CaseScopeChangeStatus) error {
	if err := m.sealCaseFilesTx(ctx, tx, status.CaseID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_scope_actions(request_id,case_id,kind,target_id,action)
	 SELECT r.id,r.case_id,s->>'kind',s->>'target_id','remove' FROM workspace_legal_hold.case_scope_requests r,
	 LATERAL jsonb_array_elements(r.payload->'remove') s WHERE r.id=$1`, status.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_scope_completions(request_id,actor_id) VALUES($1,$2)`, status.ID, actor)
	return err
}
