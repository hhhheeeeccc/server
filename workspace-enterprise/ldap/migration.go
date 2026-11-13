package ldap

import (
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"

	ldapclient "github.com/mattermost/ldap"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

type idMigration struct {
	userID string
	oldID  string
	newID  string
}

// MigrateIDAttribute changes the stored LDAP identities while the current
// IdAttribute still names the old attribute. The administrator changes the
// configuration only after this call succeeds.
func (s *Service) MigrateIDAttribute(rctx request.CTX, toAttribute string) error {
	cfg, appErr := s.userSettings(false)
	if appErr != nil {
		return appErr
	}
	if !attributeName.MatchString(toAttribute) {
		return ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
	}
	if toAttribute == *cfg.IdAttribute {
		return nil
	}

	// Wait outside the native pool: a synchronization owner needs that pool
	// to finish and release the shared coordination lock.
	release, err := s.acquireSyncLock(rctx.Context())
	if err != nil {
		return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
	}
	defer release()
	tx, err := s.app.Srv().Store().GetInternalMasterDB().BeginTx(rctx.Context(), nil)
	if err != nil {
		return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
	}
	defer tx.Rollback()
	// The table lock also excludes concurrent logins and account edits while
	// the directory snapshot is made; identity writes stay in this transaction.
	if _, err = tx.ExecContext(rctx.Context(), "LOCK TABLE Users IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
	}

	rows, err := tx.QueryContext(rctx.Context(), "SELECT Id, AuthData FROM Users WHERE AuthService=$1", model.UserAuthServiceLdap)
	if err != nil {
		return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
	}
	users := map[string]string{}
	for rows.Next() {
		var id string
		var authData sql.NullString
		if err = rows.Scan(&id, &authData); err != nil {
			break
		}
		if !authData.Valid || authData.String == "" {
			err = errors.New("LDAP account has no identity")
			break
		}
		users[authData.String] = id
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return ldapError("ent.ldap.invalid_mapping.app_error", http.StatusConflict)
	}

	d, entries, appErr := s.userEntries(rctx, cfg, "", "", []string{*cfg.IdAttribute, toAttribute})
	if appErr != nil {
		return appErr
	}
	d.close()
	plan, appErr := planIDMigration(users, entries, *cfg.IdAttribute, toAttribute)
	if appErr != nil {
		return appErr
	}
	if !reflect.DeepEqual(cfg, s.app.Config().LdapSettings) {
		return ldapError("ent.ldap.sync_config_changed.app_error", http.StatusConflict)
	}
	if _, appErr = s.userSettings(false); appErr != nil {
		return appErr
	}
	// AuthData is globally unique, including users of other auth providers.
	for _, change := range plan {
		var otherID string
		err = tx.QueryRowContext(rctx.Context(), "SELECT Id FROM Users WHERE AuthData=$1 AND AuthService<>$2 LIMIT 1", change.newID, model.UserAuthServiceLdap).Scan(&otherID)
		if err == nil {
			return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
		}
	}
	// A two-phase update permits old/new IDs to exchange values without
	// violating the immediate unique index on Users.AuthData.
	for _, change := range plan {
		temporary := "ldap-migrate-" + model.NewId()
		if err = updateLDAPID(rctx, tx, change.userID, change.oldID, temporary); err != nil {
			return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict).Wrap(err)
		}
		change.oldID = temporary
	}
	for _, change := range plan {
		if err = updateLDAPID(rctx, tx, change.userID, change.oldID, change.newID); err != nil {
			return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict).Wrap(err)
		}
	}
	if err = tx.Commit(); err != nil {
		return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError).Wrap(err)
	}
	release()
	for _, change := range plan {
		s.app.InvalidateCacheForUser(change.userID)
	}
	return nil
}

func updateLDAPID(rctx request.CTX, tx *sql.Tx, userID, oldID, newID string) error {
	result, err := tx.ExecContext(rctx.Context(), "UPDATE Users SET AuthData=$1, UpdateAt=$2 WHERE Id=$3 AND AuthService=$4 AND AuthData=$5", newID, model.GetMillis(), userID, model.UserAuthServiceLdap, oldID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("LDAP account changed during identity migration")
	}
	return nil
}

func planIDMigration(users map[string]string, entries []*ldapclient.Entry, from, to string) ([]*idMigration, *model.AppError) {
	seenOld := map[string]bool{}
	seenNew := map[string]bool{}
	plan := make([]*idMigration, 0, len(users))
	for _, entry := range entries {
		oldID, newID := entryID(entry, from), entryID(entry, to)
		if entry.DN == "" || !validMigrationID(oldID) || !validMigrationID(newID) || seenOld[oldID] || seenNew[newID] {
			return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusConflict)
		}
		seenOld[oldID], seenNew[newID] = true, true
		if userID := users[oldID]; userID != "" {
			plan = append(plan, &idMigration{userID: userID, oldID: oldID, newID: newID})
		}
	}
	if len(plan) != len(users) {
		return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusConflict)
	}
	return plan, nil
}

func validMigrationID(id string) bool {
	return id != "" && len(id) <= 128 && utf8.ValidString(id)
}
