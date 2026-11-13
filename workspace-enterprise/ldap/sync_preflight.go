package ldap

import (
	"net/http"
	"strings"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

const identityPreflightBatchSize = 500

// Check the master database before profile writes. The advisory sync lock
// serializes jobs, but regular account updates can still race this check;
// the store's unique constraints remain the final guard.
func (s *Service) preflightIdentityOwnership(rctx request.CTX, ldapUsers, samlUsers []*model.User, byID, byEmail map[string]*model.User, samlCfg model.SamlSettings) *model.AppError {
	usernameOwner := map[string]string{}
	emailOwner := map[string]string{}
	add := func(local, mapped *model.User) *model.AppError {
		if mapped == nil {
			return nil
		}
		username := strings.ToLower(mapped.Username)
		email := strings.ToLower(mapped.Email)
		if owner := usernameOwner[username]; owner != "" && owner != local.Id {
			return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict)
		}
		if owner := emailOwner[email]; owner != "" && owner != local.Id {
			return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict)
		}
		usernameOwner[username] = local.Id
		emailOwner[email] = local.Id
		return nil
	}
	for _, user := range ldapUsers {
		if appErr := add(user, byID[model.SafeDereference(user.AuthData)]); appErr != nil {
			return appErr
		}
	}
	for _, user := range samlUsers {
		if model.SafeDereference(samlCfg.IgnoreGuestsLdapSync) && user.IsGuest() {
			continue
		}
		if appErr := add(user, mappedSAMLUser(user, samlCfg, byID, byEmail)); appErr != nil {
			return appErr
		}
	}

	usernames := make([]string, 0, len(usernameOwner))
	for name := range usernameOwner {
		usernames = append(usernames, name)
	}
	emails := make([]string, 0, len(emailOwner))
	for email := range emailOwner {
		emails = append(emails, email)
	}
	for offset := 0; offset < len(usernames) || offset < len(emails); offset += identityPreflightBatchSize {
		if err := rctx.Context().Err(); err != nil {
			return ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
		}
		nameEnd := min(offset+identityPreflightBatchSize, len(usernames))
		emailEnd := min(offset+identityPreflightBatchSize, len(emails))
		var nameBatch, emailBatch []string
		if offset < len(usernames) {
			nameBatch = usernames[offset:nameEnd]
		}
		if offset < len(emails) {
			emailBatch = emails[offset:emailEnd]
		}
		rows, err := s.app.Srv().Store().GetInternalMasterDB().QueryContext(rctx.Context(),
			`SELECT id, username, email FROM users WHERE lower(username) = ANY($1::text[]) OR lower(email) = ANY($2::text[])`,
			pq.Array(nameBatch), pq.Array(emailBatch))
		if err != nil {
			return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
		}
		for rows.Next() {
			var id, username, email string
			if err = rows.Scan(&id, &username, &email); err != nil {
				rows.Close()
				return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
			}
			if owner := usernameOwner[strings.ToLower(username)]; owner != "" && owner != id {
				rows.Close()
				return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict)
			}
			if owner := emailOwner[strings.ToLower(email)]; owner != "" && owner != id {
				rows.Close()
				return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
		}
	}
	return nil
}
