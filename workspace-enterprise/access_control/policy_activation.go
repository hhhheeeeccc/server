package access_control

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/store"
)

var errPolicyActivationChanged = errors.New("policy changed concurrently; retry saving")

// policyRevisionData is the versioned payload used by the native policy store.
// Keep the same nil-import normalization and JSON field order when comparing:
// PostgreSQL jsonb may reorder keys, while the native store compares serialized
// model values rather than the raw database representation.
type policyRevisionData struct {
	Imports []string                        `json:"imports"`
	Rules   []model.AccessControlPolicyRule `json:"rules"`
	Roles   []string                        `json:"roles,omitempty"`
	Scope   string                          `json:"scope,omitempty"`
	ScopeID string                          `json:"scope_id,omitempty"`
}

func (d policyRevisionData) encode() ([]byte, error) {
	if d.Imports == nil {
		d.Imports = []string{}
	}
	return json.Marshal(d)
}

// savePolicyActivation handles the one native Save no-op case that discards an
// activation change. All other saves still use the native store. Writers claim
// the unique history revision before touching the current row, matching the
// native store's lock order. Retry a superseded snapshot in a fresh transaction;
// bound retries so a busy policy cannot retain a request indefinitely.
func (s *Service) savePolicyActivation(rctx request.CTX, policy *model.AccessControlPolicy) (*model.AccessControlPolicy, bool, error) {
	for range 4 {
		saved, handled, err := s.trySavePolicyActivation(rctx, policy)
		if !errors.Is(err, errPolicyActivationChanged) {
			return saved, handled, err
		}
	}
	return nil, true, errPolicyActivationChanged
}

func (s *Service) trySavePolicyActivation(rctx request.CTX, policy *model.AccessControlPolicy) (*model.AccessControlPolicy, bool, error) {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return nil, false, nil
	}
	tx, err := db.BeginTx(rctx.Context(), nil)
	if err != nil {
		return nil, true, fmt.Errorf("begin policy activation: %w", err)
	}
	defer tx.Rollback()

	var policyType, version string
	var active bool
	var revision int
	var rawData []byte
	err = tx.QueryRowContext(rctx.Context(), `
		SELECT Type, Active, Version, Data, Revision FROM AccessControlPolicies
		WHERE ID=$1`, policy.ID).Scan(&policyType, &active, &version, &rawData, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("read policy activation: %w", err)
	}
	if policyType != policy.Type || version != policy.Version || active == policy.Active {
		return nil, false, nil
	}

	var existing policyRevisionData
	if len(rawData) > 0 {
		if err = json.Unmarshal(rawData, &existing); err != nil {
			return nil, true, fmt.Errorf("decode policy activation: %w", err)
		}
	}
	existingData, err := existing.encode()
	if err != nil {
		return nil, true, err
	}
	requestedData, err := (policyRevisionData{
		Imports: policy.Imports, Rules: policy.Rules, Roles: policy.Roles,
		Scope: policy.Scope, ScopeID: policy.ScopeID,
	}).encode()
	if err != nil {
		return nil, true, err
	}
	if !bytes.Equal(existingData, requestedData) {
		return nil, false, nil
	}
	props, err := json.Marshal(policy.Props)
	if err != nil {
		return nil, true, err
	}

	// The native history table intentionally has no Active column. Claim its
	// revision key first, without holding the current row while another native
	// writer owns that key and still needs to update/delete the current row.
	result, err := tx.ExecContext(rctx.Context(), `
		INSERT INTO AccessControlPolicyHistory (ID, Name, Type, CreateAt, Revision, Version, Data, Props)
		SELECT ID, Name, Type, CreateAt, Revision, Version, Data, Props
		FROM AccessControlPolicies WHERE ID=$1 AND Revision=$2
		ON CONFLICT (ID, Revision) DO NOTHING`, policy.ID, revision)
	if err != nil {
		return nil, true, fmt.Errorf("record policy activation history: %w", err)
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return nil, true, err
	}
	if claimed != 1 {
		return nil, true, errPolicyActivationChanged
	}
	saved := clonePolicy(policy)
	err = tx.QueryRowContext(rctx.Context(), `
		UPDATE AccessControlPolicies
		SET Active=$2, Name=$3, CreateAt=$4, Revision=Revision+1, Props=$5::jsonb
		WHERE ID=$1 AND Revision=$6 RETURNING CreateAt, Revision`, policy.ID, policy.Active, policy.Name,
		model.GetMillis(), string(props), revision).Scan(&saved.CreateAt, &saved.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, true, errPolicyActivationChanged
	}
	if err != nil {
		var pgErr *pq.Error
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.Constraint == "idx_accesscontrolpolicies_name_type" {
			return nil, true, store.NewErrConflict("AccessControlPolicy", err, "name="+policy.Name)
		}
		return nil, true, fmt.Errorf("update policy activation: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, true, fmt.Errorf("commit policy activation: %w", err)
	}
	return saved, true, nil
}
