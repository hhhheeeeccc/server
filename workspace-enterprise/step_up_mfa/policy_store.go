package step_up_mfa

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

const policyTimeout = 5 * time.Second

type PolicySettings struct {
	TeamDeletion       bool `json:"team_deletion"`
	UserDeletion       bool `json:"user_deletion"`
	UserPasswordChange bool `json:"user_password_change"`
	UserMfaRemoval     bool `json:"user_mfa_removal"`
	ConfigDisable      bool `json:"config_disable"`
	GraceMinutes       int  `json:"grace_minutes"`
}

type PolicyResponse struct {
	PolicySettings
	Version int64 `json:"version"`
	// Kept in responses for older webapp bundles; this is the regular MFA
	// setting, not an independent step-up activation gate.
	Enabled bool `json:"enabled"`
}

type storedPolicy struct {
	TeamDeletion       *bool `json:"team_deletion"`
	UserDeletion       *bool `json:"user_deletion"`
	UserPasswordChange *bool `json:"user_password_change"`
	UserMfaRemoval     *bool `json:"user_mfa_removal"`
	ConfigDisable      *bool `json:"config_disable"`
	GraceMinutes       *int  `json:"grace_minutes"`
}

func (s *Service) initPolicy(ctx context.Context, defaults PolicySettings) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(6492261408)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS WorkspaceStepUpMFAPolicy (
		Id integer PRIMARY KEY CHECK (Id=1), Policy jsonb NOT NULL, Version bigint NOT NULL DEFAULT 0,
		MigrationVersion integer NOT NULL DEFAULT 0)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `ALTER TABLE WorkspaceStepUpMFAPolicy
		ADD COLUMN IF NOT EXISTS MigrationVersion integer NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	raw, err := json.Marshal(defaults)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO WorkspaceStepUpMFAPolicy (Id, Policy, MigrationVersion) VALUES (1, $1, 1)
		ON CONFLICT (Id) DO NOTHING`, string(raw)); err != nil {
		return err
	}
	// Existing installations used a process-local gate. If it was off, keep
	// every protected operation off until an administrator enables it in the
	// shared policy. Fresh installs use the normal per-operation defaults.
	if !strings.EqualFold(os.Getenv("WORKSPACE_STEP_UP_MFA_ENABLED"), "true") {
		if _, err = tx.ExecContext(ctx, `UPDATE WorkspaceStepUpMFAPolicy
			SET Policy=Policy || '{"team_deletion":false,"user_deletion":false,"user_password_change":false,"user_mfa_removal":false,"config_disable":false}'::jsonb,
			Version=Version+1, MigrationVersion=1 WHERE Id=1 AND MigrationVersion=0`); err != nil {
			return err
		}
	} else if _, err = tx.ExecContext(ctx, `UPDATE WorkspaceStepUpMFAPolicy
		SET MigrationVersion=1 WHERE Id=1 AND MigrationVersion=0`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) readPolicy(ctx context.Context) (PolicyResponse, error) {
	if s.initErr != nil {
		return PolicyResponse{}, s.initErr
	}
	ctx, cancel := context.WithTimeout(ctx, policyTimeout)
	defer cancel()
	var raw []byte
	var result PolicyResponse
	if err := s.db.QueryRowContext(ctx, `SELECT Policy, Version FROM WorkspaceStepUpMFAPolicy WHERE Id=1`).Scan(&raw, &result.Version); err != nil {
		return PolicyResponse{}, err
	}
	var stored storedPolicy
	if err := json.Unmarshal(raw, &stored); err != nil {
		return PolicyResponse{}, err
	}
	if stored.TeamDeletion == nil || stored.UserDeletion == nil || stored.UserPasswordChange == nil ||
		stored.UserMfaRemoval == nil || stored.ConfigDisable == nil || stored.GraceMinutes == nil ||
		*stored.GraceMinutes < 0 || *stored.GraceMinutes > 1440 {
		return PolicyResponse{}, errors.New("stored step-up MFA policy is incomplete or invalid")
	}
	result.PolicySettings = PolicySettings{
		TeamDeletion: *stored.TeamDeletion, UserDeletion: *stored.UserDeletion,
		UserPasswordChange: *stored.UserPasswordChange, UserMfaRemoval: *stored.UserMfaRemoval,
		ConfigDisable: *stored.ConfigDisable, GraceMinutes: *stored.GraceMinutes,
	}
	result.Enabled = model.SafeDereference(s.app.Config().ServiceSettings.EnableMultifactorAuthentication)
	return result, nil
}

var errPolicyConflict = errors.New("step-up MFA policy changed since it was loaded")

func (s *Service) savePolicy(ctx context.Context, requested PolicyResponse) (PolicyResponse, error) {
	if requested.GraceMinutes < 0 || requested.GraceMinutes > 1440 || requested.Version < 0 {
		return PolicyResponse{}, errors.New("invalid step-up MFA policy")
	}
	if s.initErr != nil {
		return PolicyResponse{}, s.initErr
	}
	ctx, cancel := context.WithTimeout(ctx, policyTimeout)
	defer cancel()
	raw, err := json.Marshal(requested.PolicySettings)
	if err != nil {
		return PolicyResponse{}, err
	}
	var version int64
	err = s.db.QueryRowContext(ctx, `UPDATE WorkspaceStepUpMFAPolicy
		SET Policy=$1, Version=Version+1 WHERE Id=1 AND Version=$2 RETURNING Version`,
		string(raw), requested.Version).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return PolicyResponse{}, errPolicyConflict
	}
	if err != nil {
		return PolicyResponse{}, err
	}
	requested.Version = version
	requested.Enabled = model.SafeDereference(s.app.Config().ServiceSettings.EnableMultifactorAuthentication)
	return requested, nil
}
