package legal_hold

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

var ErrCaseStartKeyConflict = errors.New("legal hold start request key belongs to a different title or scope")

// StartCaptureWithKey recovers the committed case after a lost response. Keys
// are scoped to the current administrator and bind the normalized title and
// unordered set of scopes. A retry resumes preparation or returns the current
// active/released state; it never reopens a released case or snapshots anew.
func (m *CaseManager) StartCaptureWithKey(rctx request.CTX, input CaseRequest, key string) (CaseStatus, error) {
	// Authentication precedes validation, including on replay.
	if _, err := m.authorize(rctx); err != nil {
		return CaseStatus{}, err
	}
	if !model.IsValidId(key) {
		return CaseStatus{}, errors.New("legal hold start request key must be a valid ID")
	}
	return m.startCapture(rctx, input, key)
}

func (m *CaseManager) reauthorize(rctx request.CTX, actor string) error {
	current, err := m.authorize(rctx)
	if err != nil {
		return err
	}
	if current != actor {
		return ErrCaseForbidden
	}
	return nil
}

// JSONB equality also rejects extra payload fields. Copy the scope slice so
// canonicalization never mutates a caller's request while another call uses it.
func caseStartPayload(input CaseRequest) ([]byte, error) {
	type scope struct {
		Kind     string `json:"kind"`
		TargetID string `json:"target_id"`
	}
	scopes := make([]scope, len(input.Scopes))
	for i, item := range input.Scopes {
		scopes[i] = scope{Kind: item.Kind, TargetID: item.TargetID}
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].Kind == scopes[j].Kind {
			return scopes[i].TargetID < scopes[j].TargetID
		}
		return scopes[i].Kind < scopes[j].Kind
	})
	return json.Marshal(struct {
		Title  string  `json:"title"`
		Scopes []scope `json:"scopes"`
	}{Title: input.Title, Scopes: scopes})
}

func (m *CaseManager) existingCaseStart(ctx context.Context, actor, key string, input CaseRequest) (CaseStatus, bool, error) {
	payload, err := caseStartPayload(input)
	if err != nil {
		return CaseStatus{}, false, err
	}
	var id string
	var matches bool
	err = m.journal.coordinator.fence.db.QueryRowContext(ctx, `SELECT case_id,request_payload=$3::jsonb FROM workspace_legal_hold.case_start_keys WHERE actor_id=$1 AND request_key=$2`, actor, key, string(payload)).Scan(&id, &matches)
	if errors.Is(err, sql.ErrNoRows) {
		return CaseStatus{}, false, nil
	}
	if err != nil {
		return CaseStatus{}, false, err
	}
	if !matches {
		return CaseStatus{}, true, ErrCaseStartKeyConflict
	}
	status, err := m.status(ctx, id)
	return status, true, err
}
