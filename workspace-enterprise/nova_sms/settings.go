package nova_sms

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode"
)

const settingsTimeout = 5 * time.Second

type Settings struct {
	Endpoint              string `json:"endpoint"`
	ApplicationIdentifier string `json:"application_identifier"`
	WorkflowIdentifier    string `json:"workflow_identifier"`
	Enabled               bool   `json:"enabled"`
	HasSecretKey          bool   `json:"has_secret_key"`
	Version               int64  `json:"version"`
}

type settingsUpdate struct {
	Endpoint              string `json:"endpoint"`
	ApplicationIdentifier string `json:"application_identifier"`
	WorkflowIdentifier    string `json:"workflow_identifier"`
	Enabled               bool   `json:"enabled"`
	SecretKey             string `json:"secret_key"`
	Version               int64  `json:"version"`
}

var errConflict = errors.New("Nova settings changed since they were loaded")

func validEndpoint(raw string) bool {
	if raw == "" || len(raw) > 2048 || strings.TrimSpace(raw) != raw {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Hostname() != "" && parsed.User == nil && parsed.Fragment == "" && parsed.RawQuery == ""
}

func validUpdate(input settingsUpdate) bool {
	return validEndpoint(input.Endpoint) &&
		input.ApplicationIdentifier != "" && len(input.ApplicationIdentifier) <= 128 &&
		strings.TrimSpace(input.ApplicationIdentifier) == input.ApplicationIdentifier &&
		strings.IndexFunc(input.ApplicationIdentifier, unicode.IsSpace) == -1 &&
		len(input.WorkflowIdentifier) <= 128 && strings.TrimSpace(input.WorkflowIdentifier) == input.WorkflowIdentifier &&
		strings.IndexFunc(input.WorkflowIdentifier, unicode.IsSpace) == -1 &&
		(!input.Enabled || input.WorkflowIdentifier != "") &&
		len(input.SecretKey) <= 1024 && input.Version >= 0
}

func (s *Service) initSettings(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, settingsTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS WorkspaceNovaSettings (
		Id integer PRIMARY KEY CHECK (Id=1), Endpoint text NOT NULL DEFAULT '',
		ApplicationIdentifier text NOT NULL DEFAULT '', SecretKey text NOT NULL DEFAULT '',
		Version bigint NOT NULL DEFAULT 0,
		WorkflowIdentifier text NOT NULL DEFAULT '', Enabled boolean NOT NULL DEFAULT false,
		EnabledAt bigint NOT NULL DEFAULT 0)`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE WorkspaceNovaSettings
		ADD COLUMN IF NOT EXISTS WorkflowIdentifier text NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS Enabled boolean NOT NULL DEFAULT false,
		ADD COLUMN IF NOT EXISTS EnabledAt bigint NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO WorkspaceNovaSettings (Id) VALUES (1) ON CONFLICT (Id) DO NOTHING`)
	return err
}

func (s *Service) readSettings(ctx context.Context) (Settings, error) {
	ctx, cancel := context.WithTimeout(ctx, settingsTimeout)
	defer cancel()
	var result Settings
	err := s.db.QueryRowContext(ctx, `SELECT Endpoint, ApplicationIdentifier, WorkflowIdentifier, Enabled, SecretKey <> '', Version FROM WorkspaceNovaSettings WHERE Id=1`).Scan(
		&result.Endpoint, &result.ApplicationIdentifier, &result.WorkflowIdentifier, &result.Enabled, &result.HasSecretKey, &result.Version)
	return result, err
}

func (s *Service) saveSettings(ctx context.Context, input settingsUpdate) (Settings, error) {
	ctx, cancel := context.WithTimeout(ctx, settingsTimeout)
	defer cancel()
	var result Settings
	err := s.db.QueryRowContext(ctx, `UPDATE WorkspaceNovaSettings
		SET Endpoint=$1, ApplicationIdentifier=$2, WorkflowIdentifier=$3, Enabled=$4,
		EnabledAt=CASE WHEN $4 AND (NOT Enabled OR Endpoint<>$1 OR
			ApplicationIdentifier<>$2 OR WorkflowIdentifier<>$3 OR
			($5<>'' AND SecretKey<>$5)) THEN $7 ELSE EnabledAt END,
		SecretKey=CASE WHEN $5 <> '' THEN $5 ELSE SecretKey END,
		Version=Version+1 WHERE Id=1 AND Version=$6
		RETURNING Endpoint, ApplicationIdentifier, WorkflowIdentifier, Enabled, SecretKey <> '', Version`,
		input.Endpoint, input.ApplicationIdentifier, input.WorkflowIdentifier, input.Enabled, input.SecretKey, input.Version, time.Now().UnixMilli()).Scan(
		&result.Endpoint, &result.ApplicationIdentifier, &result.WorkflowIdentifier, &result.Enabled, &result.HasSecretKey, &result.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{}, errConflict
	}
	return result, err
}
