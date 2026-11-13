package account_migration

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/mattermost/enterprise/internal/advisorylock"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

type Service struct {
	app        *app.App
	initErr    error
	syncLocker *advisorylock.Locker
}

var _ einterfaces.AccountMigrationInterface = (*Service)(nil)

func New(a *app.App) einterfaces.AccountMigrationInterface {
	installPrimarySessionLookups(a)
	s := &Service{app: a, syncLocker: advisorylock.New(model.SafeDereference(a.Config().SqlSettings.DataSource))}
	if err := s.initSessionCleanup(); err != nil {
		s.initErr = err
		a.Log().Error("Unable to initialize authentication migration session cleanup", mlog.Err(err))
	} else {
		a.Srv().Go(s.runSessionCleanup)
	}
	return s
}

type change struct{ before, after *model.User }

// SAMLIdentity binds an existing account to the exact identity returned by
// the IdP. UserID, rather than email, identifies the source account.
type SAMLIdentity struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	SAMLID   string `json:"saml_id"`
}

func migrationError(id string, status int) *model.AppError {
	return model.NewAppError("AccountMigration", id, nil, "", status)
}

func invalid(details string) *model.AppError {
	return model.NewAppError("AccountMigration", "api.user.update_user_auth.invalid_request", nil, details, http.StatusBadRequest)
}

func storeError() *model.AppError {
	return migrationError("app.user.update_auth_data.app_error", http.StatusInternalServerError)
}

func conflict() *model.AppError {
	return migrationError("app.user.update_auth_data.app_error", http.StatusConflict)
}

func (s *Service) settings(from, target string) (*model.Config, *model.AppError) {
	if !slices.Contains([]string{"", "gitlab", "google", "office365", "ldap", "saml"}, from) || from == target {
		return nil, invalid("Unsupported source authentication service")
	}
	cfg := s.app.Config().Clone()
	l := s.app.License()
	if l == nil || l.Features == nil ||
		(target == model.UserAuthServiceLdap && (!model.SafeDereference(l.Features.LDAP) || !model.SafeDereference(cfg.LdapSettings.Enable))) ||
		(target == model.UserAuthServiceSaml && (!model.SafeDereference(l.Features.SAML) || !model.SafeDereference(cfg.SamlSettings.Enable))) {
		return nil, migrationError("api.license.upgrade_needed.app_error", http.StatusForbidden)
	}
	return cfg, nil
}

func (s *Service) MigrateToLdap(rctx request.CTX, from, match string, force, dryRun bool) *model.AppError {
	if from == model.UserAuthServiceEmail {
		from = ""
	}
	cfg, appErr := s.settings(from, model.UserAuthServiceLdap)
	if appErr != nil {
		return appErr
	}
	if match != "email" && match != "username" {
		return invalid("LDAP match field must be email or username")
	}
	if s.app.Ldap() == nil {
		return migrationError("api.admin.ldap.not_available.app_error", http.StatusNotImplemented)
	}
	return s.migrate(rctx, from, model.UserAuthServiceLdap, cfg, dryRun, func(users []*model.User) ([]change, *model.AppError) {
		directory, err := s.app.Ldap().GetAllLdapUsers(rctx)
		if err != nil {
			return nil, err
		}
		key := func(u *model.User) string {
			if match == "email" {
				return strings.ToLower(u.Email)
			}
			return strings.ToLower(u.Username)
		}
		byMatch := map[string][]*model.User{}
		for _, u := range directory {
			if u == nil || u.AuthData == nil || *u.AuthData == "" || key(u) == "" {
				return nil, invalid("LDAP returned an invalid migration identity")
			}
			byMatch[key(u)] = append(byMatch[key(u)], u)
		}
		plan := make([]change, 0, len(users))
		for _, old := range users {
			matches := byMatch[key(old)]
			if len(matches) == 0 {
				return nil, migrationError("ent.migration.migratetoldap.user_not_found", http.StatusBadRequest)
			}
			if len(matches) != 1 {
				if !force {
					return nil, migrationError("ent.migration.migratetoldap.duplicate_field", http.StatusConflict)
				}
				rctx.Logger().Warn("Skipping ambiguous LDAP migration match", mlog.String("user_id", old.Id))
				continue
			}
			u := old.DeepCopy()
			u.AuthService, u.AuthData = model.UserAuthServiceLdap, new(*matches[0].AuthData)
			u.Username, u.Email = matches[0].Username, matches[0].Email
			plan = append(plan, change{old, u})
		}
		return plan, nil
	})
}

func (s *Service) MigrateToSaml(rctx request.CTX, from string, matches map[string]string, auto, dryRun bool) *model.AppError {
	if from == model.UserAuthServiceEmail {
		from = ""
	}
	cfg, appErr := s.settings(from, model.UserAuthServiceSaml)
	if appErr != nil {
		return appErr
	}
	if auto && len(matches) != 0 {
		return invalid("Choose automatic migration or an explicit email-to-username mapping")
	}
	ss := cfg.SamlSettings
	ss.SetDefaults()
	if *ss.EmailAttribute == "" || *ss.UsernameAttribute == "" {
		return invalid("SAML email and username attributes are required")
	}
	// The native contract carries email -> username, not arbitrary IdP IDs.
	// Guessing such an ID would lock out the migrated account on its next login.
	if *ss.IdAttribute != "" && *ss.IdAttribute != *ss.EmailAttribute && *ss.IdAttribute != *ss.UsernameAttribute {
		return invalid("This SAML ID attribute cannot be derived from the native email-to-username migration map")
	}
	normalized := make(map[string]string, len(matches))
	for email, username := range matches {
		key := strings.ToLower(email)
		if !model.IsValidEmail(key) || !model.IsValidUsername(username) || normalized[key] != "" {
			return invalid("Invalid or duplicate SAML email-to-username mapping")
		}
		normalized[key] = username
	}
	return s.migrate(rctx, from, model.UserAuthServiceSaml, cfg, dryRun, func(users []*model.User) ([]change, *model.AppError) {
		plan := make([]change, 0, len(users))
		for _, old := range users {
			username := old.Username
			if !auto {
				var ok bool
				username, ok = normalized[strings.ToLower(old.Email)]
				if !ok {
					return nil, migrationError("ent.migration.migratetosaml.user_not_found_in_users_mapping_file", http.StatusBadRequest)
				}
			}
			u := old.DeepCopy()
			u.AuthService, u.Username = model.UserAuthServiceSaml, username
			u.Email = model.NormalizeEmail(u.Email)
			u.AuthData = new(u.Email)
			if *ss.IdAttribute == *ss.UsernameAttribute {
				u.AuthData = new(username)
			}
			plan = append(plan, change{old, u})
		}
		return plan, nil
	})
}

// MigrateToSamlIdentities handles IdP IDs that cannot be derived from the
// native email-to-username map. The plan must cover every source account.
func (s *Service) MigrateToSamlIdentities(rctx request.CTX, from string, identities []SAMLIdentity, dryRun bool) *model.AppError {
	if from == model.UserAuthServiceEmail {
		from = ""
	}
	cfg, appErr := s.settings(from, model.UserAuthServiceSaml)
	if appErr != nil {
		return appErr
	}
	ss := cfg.SamlSettings
	ss.SetDefaults()
	if *ss.EmailAttribute == "" || *ss.UsernameAttribute == "" || *ss.IdAttribute == "" ||
		*ss.IdAttribute == *ss.EmailAttribute || *ss.IdAttribute == *ss.UsernameAttribute {
		return invalid("A distinct external SAML ID attribute is required")
	}
	byID := make(map[string]SAMLIdentity, len(identities))
	for _, identity := range identities {
		if !model.IsValidId(identity.UserID) || !model.IsValidUsername(identity.Username) ||
			!model.IsValidEmail(identity.Email) || identity.SAMLID == "" ||
			len(identity.SAMLID) > model.UserAuthDataMaxLength {
			return invalid("Invalid explicit SAML identity")
		}
		if _, exists := byID[identity.UserID]; exists {
			return invalid("Duplicate source user in SAML identity map")
		}
		byID[identity.UserID] = identity
	}
	return s.migrate(rctx, from, model.UserAuthServiceSaml, cfg, dryRun, func(users []*model.User) ([]change, *model.AppError) {
		if len(users) != len(byID) {
			return nil, invalid("SAML identity map must cover every source account")
		}
		plan := make([]change, 0, len(users))
		for _, old := range users {
			identity, ok := byID[old.Id]
			if !ok {
				return nil, invalid("SAML identity map omits a source account")
			}
			u := old.DeepCopy()
			u.AuthService = model.UserAuthServiceSaml
			u.AuthData = new(identity.SAMLID)
			u.Username = identity.Username
			u.Email = model.NormalizeEmail(identity.Email)
			plan = append(plan, change{old, u})
		}
		return plan, nil
	})
}

// Authorization/MFA/CSRF and audit records are provided by the native API. The
// interface is also callable internally, so entitlement and input validation
// are enforced here. No custom migration endpoint is registered.
func (s *Service) migrate(rctx request.CTX, from, target string, cfg *model.Config, dryRun bool, planFor func([]*model.User) ([]change, *model.AppError)) *model.AppError {
	if s.initErr != nil {
		return storeError()
	}
	// Synchronization needs the native pool while it owns this lock. Wait on
	// a separate bounded coordination connection before opening the write tx.
	release, err := s.syncLocker.Acquire(rctx.Context(), 6492261403)
	if err != nil {
		return storeError()
	}
	defer release()
	tx, err := s.app.Srv().Store().GetInternalMasterDB().BeginTx(rctx.Context(), nil)
	if err != nil {
		return storeError()
	}
	defer tx.Rollback()
	// Keep the migration-specific transaction lock and the original key order.
	if _, err = tx.ExecContext(rctx.Context(), "SELECT pg_advisory_xact_lock($1)", int64(6492261405)); err != nil {
		return storeError()
	}
	users, err := migrationSourceUsers(rctx.Context(), tx, from)
	if err != nil {
		return migrationError("ent.account_migration.get_all_failed", http.StatusInternalServerError)
	}
	plan, appErr := planFor(users)
	if appErr != nil {
		return appErr
	}
	if appErr = validatePlan(rctx, tx, plan); appErr != nil {
		return appErr
	}
	current, appErr := s.settings(from, target)
	if appErr != nil {
		return appErr
	}
	if !reflect.DeepEqual(cfg.LdapSettings, current.LdapSettings) || !reflect.DeepEqual(cfg.SamlSettings, current.SamlSettings) {
		return conflict()
	}
	if dryRun {
		rctx.Logger().Info("Authentication migration dry run validated", mlog.String("target", target), mlog.Int("users", len(plan)))
		return nil
	}
	// The public store has no atomic batch auth+username update. Change only
	// these fields in one transaction, using snapshots to reject concurrent
	// edits; preserve IDs, history, roles, MFA and activation state.
	now := model.GetMillis()
	for _, item := range plan {
		old, next := item.before, item.after
		result, err := tx.ExecContext(rctx.Context(), `UPDATE Users SET AuthService=$1, AuthData=$2, Username=$3, Email=$4,
			Password='', FailedAttempts=0, LastPasswordUpdate=$5, UpdateAt=$5
			WHERE Id=$6 AND UpdateAt=$7 AND AuthService=$8 AND AuthData IS NOT DISTINCT FROM $9
			AND Username=$10 AND Email=$11 AND DeleteAt=$12 AND Password=$13
			AND COALESCE(RemoteId, '')='' AND NOT EXISTS (SELECT 1 FROM Bots WHERE Bots.UserId=Users.Id)`,
			next.AuthService, *next.AuthData, next.Username, next.Email, now,
			old.Id, old.UpdateAt, old.AuthService, old.AuthData, old.Username, old.Email, old.DeleteAt, old.Password)
		if err != nil {
			var pgErr *pq.Error
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return conflict()
			}
			return storeError()
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != 1 {
			return conflict()
		}
	}
	// Persist cleanup intent in the same transaction as the identity change.
	// A failed post-commit revocation can then be retried after process restart.
	cleanupRetryAt := model.GetMillis() + sessionCleanupInterval.Milliseconds()
	for _, item := range plan {
		if _, err = tx.ExecContext(rctx.Context(), `INSERT INTO WorkspaceMigrationSessionCleanup (UserId,CutoffAt,RetryAt)
			VALUES ($1,$2,$3) ON CONFLICT (UserId) DO UPDATE SET CutoffAt=EXCLUDED.CutoffAt,RetryAt=EXCLUDED.RetryAt`,
			item.before.Id, int64(0), cleanupRetryAt); err != nil {
			return storeError()
		}
	}
	if err = tx.Commit(); err != nil {
		return storeError()
	}
	// Match the original transaction lifetime: postcommit session cleanup
	// must not keep directory synchronization or another migration waiting.
	release()
	// Revoke each session through the native app path. The list comes from the
	// master so a lagging replica cannot make cleanup appear complete.
	var cleanupErr *model.AppError
	for _, item := range plan {
		s.app.InvalidateCacheForUser(item.before.Id)
		// A client disconnect after commit must not cancel session cleanup.
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(rctx.Context()), 30*time.Second)
		cleanupCutoff, err := s.setSessionCleanupCutoff(cleanupContext, item.before.Id)
		if err != nil {
			cleanupErr = storeError()
			rctx.Logger().Error("Authentication migrated but session cleanup cutoff could not be saved", mlog.String("user_id", item.before.Id), mlog.Err(err))
			cancel()
			continue
		}
		if cleanupCutoff == 0 {
			cancel()
			continue
		}
		if err := s.revokeMigratedSessions(rctx.WithContext(cleanupContext), item.before.Id, cleanupCutoff); err != nil {
			cleanupErr = storeError()
			rctx.Logger().Error("Authentication migrated but session revocation failed", mlog.String("user_id", item.before.Id), mlog.Err(err))
		} else if err := s.deleteSessionCleanup(cleanupContext, item.before.Id, cleanupCutoff); err != nil {
			cleanupErr = storeError()
			rctx.Logger().Error("Authentication migrated but cleanup state could not be cleared", mlog.String("user_id", item.before.Id), mlog.Err(err))
		}
		cancel()
	}
	rctx.Logger().Info("Authentication migration committed", mlog.String("target", target), mlog.Int("users", len(plan)))
	return cleanupErr
}

// Read the complete source population through the same primary transaction
// used for validation and writes. A replica may omit accounts committed before
// migration starts and make an incomplete map appear to cover every account.
// Load the stored profile so the existing target-profile validation is retained;
// roles, MFA and other profile fields remain untouched by migration's UPDATE.
func migrationSourceUsers(ctx context.Context, tx *sql.Tx, from string) ([]*model.User, error) {
	rows, err := tx.QueryContext(ctx, `SELECT Id,CreateAt,UpdateAt,DeleteAt,Username,Password,AuthData,AuthService,
		Email,EmailVerified,Nickname,FirstName,LastName,Position,Roles,AllowMarketing,Props,NotifyProps,
		LastPasswordUpdate,LastPictureUpdate,FailedAttempts,Locale,Timezone,MfaActive,MfaSecret,
		MfaUsedTimestamps,RemoteId,LastLogin
		FROM Users WHERE AuthService=$1 AND COALESCE(RemoteId,'')=''
		AND NOT EXISTS (SELECT 1 FROM Bots WHERE Bots.UserId=Users.Id) ORDER BY Username ASC`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []*model.User{}
	err = sqlx.StructScan(rows, &users)
	return users, err
}

func validatePlan(rctx request.CTX, tx *sql.Tx, plan []change) *model.AppError {
	const batchSize = 500
	usernameOwner := make(map[string]string, len(plan))
	emailOwner := make(map[string]string, len(plan))
	authDataOwner := make(map[string]string, len(plan))
	usernames := make([]string, 0, len(plan))
	emails := make([]string, 0, len(plan))
	authData := make([]string, 0, len(plan))
	for _, item := range plan {
		u := item.after.DeepCopy()
		u.Password = ""
		if u.AuthData == nil || *u.AuthData == "" || !model.IsValidEmail(u.Email) || u.IsValid() != nil {
			return invalid("Invalid target migration profile")
		}
		username, email, auth := strings.ToLower(u.Username), strings.ToLower(u.Email), *u.AuthData
		if usernameOwner[username] != "" || emailOwner[email] != "" || authDataOwner[auth] != "" {
			return conflict()
		}
		usernameOwner[username], emailOwner[email], authDataOwner[auth] = u.Id, u.Id, u.Id
		usernames, emails, authData = append(usernames, username), append(emails, email), append(authData, auth)
	}
	for offset := 0; offset < len(plan); offset += batchSize {
		end := min(offset+batchSize, len(plan))
		rows, err := tx.QueryContext(rctx.Context(), `SELECT Id, Username, Email, AuthData FROM Users
			WHERE LOWER(Username) = ANY($1::text[]) OR LOWER(Email) = ANY($2::text[])
			OR AuthData = ANY($3::text[])`,
			pq.Array(usernames[offset:end]), pq.Array(emails[offset:end]), pq.Array(authData[offset:end]))
		if err != nil {
			return storeError()
		}
		for rows.Next() {
			var id, username, email string
			var auth sql.NullString
			if err = rows.Scan(&id, &username, &email, &auth); err != nil {
				rows.Close()
				return storeError()
			}
			if owner := usernameOwner[strings.ToLower(username)]; owner != "" && owner != id {
				rows.Close()
				return conflict()
			}
			if owner := emailOwner[strings.ToLower(email)]; owner != "" && owner != id {
				rows.Close()
				return conflict()
			}
			if auth.Valid {
				if owner := authDataOwner[auth.String]; owner != "" && owner != id {
					rows.Close()
					return conflict()
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return storeError()
		}
	}
	return nil
}
