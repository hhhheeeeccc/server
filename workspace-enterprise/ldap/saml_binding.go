package ldap

import (
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

func mappedSAMLUser(user *model.User, cfg model.SamlSettings, byID, byEmail map[string]*model.User) *model.User {
	if model.SafeDereference(cfg.EnableSyncWithLdapIncludeAuth) && model.SafeDereference(cfg.IdAttribute) != "" {
		if mapped := byID[model.SafeDereference(user.AuthData)]; mapped != nil {
			return mapped
		}
	}
	return byEmail[strings.ToLower(user.Email)]
}

// migrateSAMLBindings runs before profile and membership writes. It changes
// AuthData for all matched SAML accounts together, preserving account IDs,
// sessions, roles, and MFA. A missing directory account is handled by the
// normal deactivation pass rather than assigned another person's identity.
func (s *Service) migrateSAMLBindings(rctx request.CTX, ldapCfg model.LdapSettings, cfg model.SamlSettings, users []*model.User, byID, byEmail map[string]*model.User) *model.AppError {
	plan := make([]*idMigration, 0)
	planIDs := map[string]bool{}
	for _, user := range users {
		if model.SafeDereference(cfg.IgnoreGuestsLdapSync) && user.IsGuest() {
			continue
		}
		mapped := mappedSAMLUser(user, cfg, byID, byEmail)
		if mapped == nil {
			continue
		}
		target := mapped.Email
		if model.SafeDereference(cfg.IdAttribute) != "" {
			target = model.SafeDereference(mapped.AuthData)
		}
		if !validMigrationID(target) || user.AuthData == nil || !validMigrationID(*user.AuthData) {
			return ldapError("ent.ldap.invalid_mapping.app_error", http.StatusConflict)
		}
		if target != *user.AuthData {
			plan = append(plan, &idMigration{userID: user.Id, oldID: *user.AuthData, newID: target})
			planIDs[user.Id] = true
		}
	}
	if len(plan) == 0 {
		return nil
	}
	tx, err := s.app.Srv().Store().GetInternalMasterDB().BeginTx(rctx.Context(), nil)
	if err != nil {
		return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(rctx.Context(), "LOCK TABLE Users IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
	}
	if !reflect.DeepEqual(cfg, s.app.Config().SamlSettings) || !reflect.DeepEqual(ldapCfg, s.app.Config().LdapSettings) {
		return ldapError("ent.ldap.sync_config_changed.app_error", http.StatusConflict)
	}
	if _, appErr := s.userSettings(false); appErr != nil {
		return appErr
	}
	license := s.app.License()
	if license == nil || license.Features == nil || !model.SafeDereference(license.Features.SAML) {
		return ldapError("ent.ldap.disabled.app_error", http.StatusForbidden)
	}
	byUserID := map[string]*model.User{}
	for _, user := range users {
		byUserID[user.Id] = user
	}
	for _, change := range plan {
		var currentEmail string
		if err = tx.QueryRowContext(rctx.Context(), "SELECT Email FROM Users WHERE Id=$1 AND AuthService=$2 AND AuthData=$3", change.userID, model.UserAuthServiceSaml, change.oldID).Scan(&currentEmail); err != nil {
			return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict)
		}
		if currentEmail != byUserID[change.userID].Email {
			return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict)
		}
		var owner string
		err = tx.QueryRowContext(rctx.Context(), "SELECT Id FROM Users WHERE AuthData=$1 AND Id<>$2 LIMIT 1", change.newID, change.userID).Scan(&owner)
		if err == nil && !planIDs[owner] {
			return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
		}
	}
	now := model.GetMillis()
	for _, change := range plan {
		temporary := "saml-migrate-" + model.NewId()
		if err = updateSAMLID(rctx, tx, change.userID, change.oldID, temporary, now); err != nil {
			return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict).Wrap(err)
		}
		change.oldID = temporary
	}
	for _, change := range plan {
		if err = updateSAMLID(rctx, tx, change.userID, change.oldID, change.newID, now); err != nil {
			return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict).Wrap(err)
		}
	}
	if err = tx.Commit(); err != nil {
		return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError).Wrap(err)
	}
	for _, change := range plan {
		user := byUserID[change.userID]
		user.AuthData = new(change.newID)
		user.UpdateAt = now
		s.app.InvalidateCacheForUser(user.Id)
	}
	return nil
}

func updateSAMLID(rctx request.CTX, tx *sql.Tx, userID, oldID, newID string, updateAt int64) error {
	result, err := tx.ExecContext(rctx.Context(),
		"UPDATE Users SET AuthData=$1, UpdateAt=$2 WHERE Id=$3 AND AuthService=$4 AND AuthData=$5",
		newID, updateAt, userID, model.UserAuthServiceSaml, oldID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("SAML account changed during identity migration")
	}
	return nil
}
