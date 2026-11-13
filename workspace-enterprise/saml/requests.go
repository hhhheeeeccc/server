package saml

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

const requestLifetime = 10 * time.Minute

func digest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func relayDigest(props map[string]string) string {
	if props == nil {
		props = map[string]string{}
	}
	return digest(props)
}

// Request correlation and assertion replay protection must survive a restart and
// must be shared by every node. The tables contain hashes, never SAML payloads.
func (s *Service) ensureSchema(rctx request.CTX) error {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if s.schemaReady {
		return nil
	}
	tx, err := s.app.Srv().Store().GetInternalMasterDB().BeginTx(rctx.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`SELECT pg_advisory_xact_lock(6492261404)`,
		`CREATE TABLE IF NOT EXISTS WorkspaceSAMLRequests (Id VARCHAR(128) PRIMARY KEY, RelayHash VARCHAR(64) NOT NULL, ConfigHash VARCHAR(64) NOT NULL, ExpiresAt BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS workspace_saml_requests_expiry ON WorkspaceSAMLRequests (ExpiresAt)`,
		`CREATE TABLE IF NOT EXISTS WorkspaceSAMLAssertions (Id VARCHAR(64) PRIMARY KEY, ExpiresAt BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS workspace_saml_assertions_expiry ON WorkspaceSAMLAssertions (ExpiresAt)`,
	} {
		if _, err = tx.ExecContext(rctx.Context(), statement); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.schemaReady = true
	return nil
}

func (s *Service) BuildRequest(rctx request.CTX, relayState string) (result *model.SamlAuthRequest, resultErr *model.AppError) {
	defer func() { localizeError(rctx, resultErr) }()
	sp, cfg, appErr := s.provider(true)
	if appErr != nil {
		return nil, appErr
	}
	props := map[string]string{}
	if relayState != "" {
		var err error
		props, err = model.VerifySamlRelayState(s.app.SamlRelayStateSigningKey(), relayState)
		if err != nil {
			return nil, samlError("ent.saml.invalid_response.app_error", http.StatusBadRequest)
		}
	}
	doc, err := sp.BuildAuthRequestDocumentNoSig()
	if err != nil {
		return nil, samlError("ent.saml.configuration.app_error", http.StatusBadRequest)
	}
	authURL, err := sp.BuildAuthURLRedirect(relayState, doc)
	if err != nil {
		return nil, samlError("ent.saml.configuration.app_error", http.StatusBadRequest)
	}
	raw, err := doc.WriteToBytes()
	if err != nil {
		return nil, samlError("ent.saml.configuration.app_error", http.StatusInternalServerError)
	}
	if err = s.ensureSchema(rctx); err != nil {
		return nil, samlError("ent.saml.store.app_error", http.StatusInternalServerError)
	}
	_, err = s.app.Srv().Store().GetInternalMasterDB().ExecContext(rctx.Context(),
		`DELETE FROM WorkspaceSAMLRequests WHERE Id IN (SELECT Id FROM WorkspaceSAMLRequests WHERE ExpiresAt <= $1 LIMIT 1000)`, time.Now().UnixMilli())
	if err != nil {
		return nil, samlError("ent.saml.store.app_error", http.StatusInternalServerError)
	}
	_, err = s.app.Srv().Store().GetInternalMasterDB().ExecContext(rctx.Context(),
		`INSERT INTO WorkspaceSAMLRequests (Id, RelayHash, ConfigHash, ExpiresAt) VALUES ($1,$2,$3,$4)`,
		doc.Root().SelectAttrValue("ID", ""), relayDigest(props), digest(cfg), time.Now().Add(requestLifetime).UnixMilli())
	if err != nil {
		return nil, samlError("ent.saml.store.app_error", http.StatusInternalServerError)
	}
	return &model.SamlAuthRequest{URL: authURL, RelayState: relayState, Base64AuthRequest: base64.StdEncoding.EncodeToString(raw)}, nil
}

func (s *Service) consumeAssertion(rctx request.CTX, assertionID, issuer, requestID string, expires time.Time, cfg model.SamlSettings, props map[string]string) error {
	if err := s.ensureSchema(rctx); err != nil {
		return err
	}
	tx, err := s.app.Srv().Store().GetInternalMasterDB().BeginTx(rctx.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	// Bounded garbage collection avoids an unbounded pause on the login path.
	for _, table := range []string{"WorkspaceSAMLRequests", "WorkspaceSAMLAssertions"} {
		_, err = tx.ExecContext(rctx.Context(), `DELETE FROM `+table+` WHERE Id IN (SELECT Id FROM `+table+` WHERE ExpiresAt <= $1 LIMIT 1000)`, now)
		if err != nil {
			return err
		}
	}
	if requestID == "" {
		// An unsolicited IdP login cannot carry an account-conversion token or
		// redirect/mobile properties taken from an unrelated SP login request.
		if len(props) != 0 {
			return errors.New("unsolicited response contains relay properties")
		}
	} else {
		result, err := tx.ExecContext(rctx.Context(), `DELETE FROM WorkspaceSAMLRequests WHERE Id=$1 AND RelayHash=$2 AND ConfigHash=$3 AND ExpiresAt>$4`, requestID, relayDigest(props), digest(cfg), now)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count != 1 {
			return errors.New("SAML request is missing, expired, or mismatched")
		}
	}
	if _, err = tx.ExecContext(rctx.Context(), `INSERT INTO WorkspaceSAMLAssertions (Id, ExpiresAt) VALUES ($1,$2)`, digest([]string{issuer, assertionID}), expires.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}
