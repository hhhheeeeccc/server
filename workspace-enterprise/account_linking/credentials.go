package account_linking

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app/password/hashers"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const (
	credentialTable       = "workspace_linked_credentials"
	credentialInitTimeout = 10 * time.Second
	loginMatchBodyLimit   = 64 * 1024
)

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type loginRequest struct {
	ID             string `json:"id"`
	LoginID        string `json:"login_id"`
	Password       string `json:"password"`
	MFAToken       string `json:"token"`
	DeviceID       string `json:"device_id"`
	VoIPDeviceID   string `json:"voip_device_id"`
	LDAPOnly       string `json:"ldap_only"`
	MagicLinkToken string `json:"magic_link_token"`
}

func (s *Service) initCredentials() {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		s.app.Log().Error("Unable to initialize linked OpenID credentials: database is unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), credentialInitTimeout)
	defer cancel()
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS workspace_linked_credentials (
			userid VARCHAR(26) PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			password TEXT NOT NULL,
			createat BIGINT NOT NULL,
			updateat BIGINT NOT NULL
		)
	`)
	if err != nil {
		s.app.Log().Error("Unable to initialize linked OpenID credentials", mlog.Err(err))
	}
}

func (s *Service) registerCredentialRoutes(local bool) {
	router := s.app.Srv().Router
	if local {
		router = s.app.Srv().LocalRouter
	}

	loginHandler := &web.Handler{
		Srv:         s.app.Srv(),
		HandleFunc:  s.loginWithLinkedPassword,
		HandlerName: web.GetHandlerName(s.loginWithLinkedPassword),
		IsLocal:     local,
	}
	router.Handle(model.APIURLSuffix+"/users/login", loginHandler).
		MatcherFunc(s.matchLinkedLogin).
		Methods(http.MethodPost)

	passwordHandler := &web.Handler{
		Srv:            s.app.Srv(),
		HandleFunc:     s.updateLinkedPassword,
		HandlerName:    web.GetHandlerName(s.updateLinkedPassword),
		RequireSession: !local,
		RequireMfa:     !local,
		IsLocal:        local,
	}
	router.Handle(model.APIURLSuffix+"/users/{user_id:[A-Za-z0-9]+}/password", passwordHandler).
		MatcherFunc(s.matchOpenIDPasswordRoute).
		Methods(http.MethodPut)
}

func (s *Service) matchLinkedLogin(r *http.Request, _ *mux.RouteMatch) bool {
	if s.app.License() == nil {
		return false
	}

	payload, ok := readLoginRequestForMatch(r)
	if !ok || payload.LDAPOnly == "true" || payload.MagicLinkToken != "" || (payload.ID == "" && payload.LoginID == "") {
		return false
	}

	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return false
	}
	var linked bool
	err := db.QueryRowContext(r.Context(), `
		SELECT EXISTS (
			SELECT 1
			FROM workspace_linked_credentials credentials
			JOIN users ON users.id = credentials.userid
			WHERE users.deleteat = 0
			  AND users.authservice = $3
			  AND (($1 <> '' AND users.id = $1)
			       OR ($1 = '' AND (lower(users.username) = lower($2) OR lower(users.email) = lower($2))))
		)
	`, payload.ID, payload.LoginID, model.ServiceOpenid).Scan(&linked)
	return err == nil && linked
}

func readLoginRequestForMatch(r *http.Request) (loginRequest, bool) {
	if r.Body == nil {
		return loginRequest{}, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, loginMatchBodyLimit+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.MultiReader(bytes.NewReader(data), r.Body), Closer: r.Body}
	if err != nil || len(data) > loginMatchBodyLimit {
		return loginRequest{}, false
	}
	var payload loginRequest
	if err := json.Unmarshal(data, &payload); err != nil {
		return loginRequest{}, false
	}
	return payload, true
}

func (s *Service) matchOpenIDPasswordRoute(r *http.Request, _ *mux.RouteMatch) bool {
	if s.app.License() == nil {
		return false
	}
	userID, ok := passwordRouteUserID(r.URL.Path)
	if !ok {
		return false
	}
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return false
	}
	var linked bool
	err := db.QueryRowContext(r.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM users
			WHERE id = $1 AND deleteat = 0 AND authservice = $2
		)
	`, userID, model.ServiceOpenid).Scan(&linked)
	return err == nil && linked
}

func passwordRouteUserID(path string) (string, bool) {
	prefix := model.APIURLSuffix + "/users/"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, "/password") {
		return "", false
	}
	userID := strings.TrimSuffix(strings.TrimPrefix(path, prefix), "/password")
	return userID, model.IsValidId(userID)
}

func (s *Service) loginWithLinkedPassword(c *web.Context, w http.ResponseWriter, r *http.Request) {
	props := model.MapFromJSON(r.Body)
	id := props["id"]
	loginID := props["login_id"]
	password := props["password"]
	mfaToken := props["token"]
	deviceID := props["device_id"]
	voIPDeviceID := props["voip_device_id"]

	auditRec := c.MakeAuditRecord(model.AuditEventLogin, model.AuditStatusFail)
	defer c.LogAuditRec(auditRec)
	model.AddEventParameterToAuditRec(auditRec, "login_id", loginID)
	model.AddEventParameterToAuditRec(auditRec, "device_id", model.RedactDeviceId(deviceID))
	model.AddEventParameterToAuditRec(auditRec, "voip_device_id", model.RedactDeviceId(voIPDeviceID))
	c.LogAuditWithUserId(id, "attempt - linked OpenID local password")

	if password == "" {
		c.Err = model.NewAppError("AuthenticateLinkedOpenID", "api.user.login.blank_pwd.app_error", nil, "", http.StatusBadRequest)
		return
	}
	user, appErr := c.App.GetUserForLogin(c.AppContext, id, loginID)
	if appErr != nil {
		s.setMaskedLoginError(c, appErr)
		return
	}
	if user.AuthService != model.ServiceOpenid {
		s.setMaskedLoginError(c, model.NewAppError("AuthenticateLinkedOpenID", "api.user.login.use_auth_service.app_error", nil, "", http.StatusUnauthorized))
		return
	}
	hash, err := s.getCredential(r.Context(), user.Id)
	if err != nil {
		s.setMaskedLoginError(c, model.NewAppError("AuthenticateLinkedOpenID", "api.user.check_user_password.invalid.app_error", nil, "", http.StatusUnauthorized).Wrap(err))
		return
	}
	if appErr = s.authenticateLinkedPassword(c, user, hash, password, mfaToken); appErr != nil {
		c.LogAuditWithUserId(user.Id, "failure - linked OpenID local password")
		s.setMaskedLoginError(c, appErr)
		return
	}
	auditRec.AddEventResultState(user)

	if user.IsMagicLinkEnabled() && !*c.App.Config().GuestAccountsSettings.EnableGuestMagicLink {
		c.Err = model.NewAppError("login", "api.user.login.guest_magic_link.disabled.error", nil, "", http.StatusUnauthorized)
		return
	}
	if user.IsGuest() {
		if c.App.Channels().License() == nil {
			c.Err = model.NewAppError("login", "api.user.login.guest_accounts.license.error", nil, "", http.StatusUnauthorized)
			return
		}
		if !*c.App.Config().GuestAccountsSettings.Enable {
			c.Err = model.NewAppError("login", "api.user.login.guest_accounts.disabled.error", nil, "", http.StatusUnauthorized)
			return
		}
	}
	if user.IsRemote() {
		c.Err = model.NewAppError("login", "api.user.login.remote_users.login.error", nil, "", http.StatusUnauthorized)
		return
	}

	session, appErr := c.App.DoLogin(c.AppContext, w, r, user, model.LoginOptions{
		DeviceId:     deviceID,
		VoIPDeviceId: voIPDeviceID,
		IsMobile:     utils.IsMobileRequest(r),
	})
	if appErr != nil {
		c.Err = appErr
		return
	}
	c.AppContext = c.AppContext.WithSession(session)
	if r.Header.Get(model.HeaderRequestedWith) == model.HeaderRequestedWithXML {
		c.App.AttachSessionCookies(c.AppContext, w, r)
	}

	userTerms, appErr := c.App.GetUserTermsOfService(user.Id)
	if appErr != nil && appErr.StatusCode != http.StatusNotFound {
		c.Err = appErr
		return
	}
	if userTerms != nil {
		user.TermsOfServiceId = userTerms.TermsOfServiceId
		user.TermsOfServiceCreateAt = userTerms.CreateAt
	}
	user.Sanitize(map[string]bool{})
	auditRec.Success()
	c.LogAuditWithUserId(user.Id, "success - linked OpenID local password")
	if err := json.NewEncoder(w).Encode(user); err != nil {
		c.Logger.Warn("Error while writing linked OpenID login response", mlog.Err(err))
	}
}

func (s *Service) authenticateLinkedPassword(c *web.Context, user *model.User, storedHash, password, mfaToken string) *model.AppError {
	if appErr := c.App.CheckUserPreflightAuthenticationCriteria(c.AppContext, user, mfaToken); appErr != nil {
		return appErr
	}
	if appErr := s.verifyCredential(c, user, storedHash, password, false); appErr != nil {
		return appErr
	}
	if appErr := c.App.CheckUserMfa(c.AppContext, user, mfaToken); appErr != nil {
		if mfaToken == "" {
			s.refundLoginAttempt(c, user.Id)
		}
		return appErr
	}
	if err := c.App.Srv().Store().User().UpdateFailedPasswordAttempts(user.Id, 0); err != nil {
		return model.NewAppError("AuthenticateLinkedOpenID", "app.user.update_failed_pwd_attempts.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	if appErr := c.App.CheckUserPostflightAuthenticationCriteria(c.AppContext, user); appErr != nil {
		return appErr
	}
	return nil
}

func (s *Service) verifyCredential(c *web.Context, user *model.User, storedHash, password string, resetAttempts bool) *model.AppError {
	maxAttempts := *c.App.Config().ServiceSettings.MaximumLoginAttempts
	claimed, err := c.App.Srv().Store().User().TryIncrementFailedPasswordAttempts(user.Id, maxAttempts)
	if err != nil {
		return model.NewAppError("AuthenticateLinkedOpenID", "app.user.update_failed_pwd_attempts.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	if !claimed {
		return model.NewAppError("AuthenticateLinkedOpenID", "api.user.check_user_login_attempts.too_many.app_error", nil, "user_id="+user.Id, http.StatusUnauthorized)
	}

	hasher, phc, err := hashers.GetHasherFromPHCString(storedHash)
	if err != nil {
		s.refundLoginAttempt(c, user.Id)
		return model.NewAppError("AuthenticateLinkedOpenID", "api.user.check_user_password.invalid_hash.app_error", nil, "user_id="+user.Id, http.StatusUnauthorized).Wrap(err)
	}
	if err = hasher.CompareHashAndPassword(phc, password); err != nil {
		if errors.Is(err, hashers.ErrMismatchedHashAndPassword) {
			return model.NewAppError("AuthenticateLinkedOpenID", "api.user.check_user_password.invalid.app_error", nil, "user_id="+user.Id, http.StatusUnauthorized).Wrap(err)
		}
		s.refundLoginAttempt(c, user.Id)
		return model.NewAppError("AuthenticateLinkedOpenID", "app.valid_password_generic.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}

	if !hashers.IsLatestHasher(hasher) {
		newHash, hashErr := hashers.Hash(password)
		if hashErr != nil {
			s.refundLoginAttempt(c, user.Id)
			return model.NewAppError("AuthenticateLinkedOpenID", "app.user.check_user_password.failed_migration", nil, "", http.StatusInternalServerError).Wrap(hashErr)
		}
		migrated, appErr := s.migrateCredential(c.AppContext.Context(), user.Id, storedHash, newHash)
		if appErr == nil && !migrated {
			// Another login may have upgraded the same password. Verify its
			// committed credential once; never overwrite a reset or recreate a
			// credential removed by unlinking while this login was hashing.
			appErr = s.verifyReplacementCredential(c.AppContext.Context(), user.Id, password)
		}
		if appErr != nil {
			s.refundLoginAttempt(c, user.Id)
			return appErr
		}
	}
	if resetAttempts {
		if err = c.App.Srv().Store().User().UpdateFailedPasswordAttempts(user.Id, 0); err != nil {
			return model.NewAppError("AuthenticateLinkedOpenID", "app.user.update_failed_pwd_attempts.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		}
	}
	return nil
}

func (s *Service) refundLoginAttempt(c *web.Context, userID string) {
	if err := c.App.Srv().Store().User().DecrementFailedPasswordAttempts(userID); err != nil {
		c.Logger.Warn("Unable to refund linked OpenID login attempt", mlog.String("user_id", userID), mlog.Err(err))
	}
}

func (s *Service) setMaskedLoginError(c *web.Context, appErr *model.AppError) {
	switch appErr.Id {
	case "mfa.validate_token.authenticate.app_error",
		"api.user.check_user_mfa.bad_code.app_error",
		"api.user.login.blank_pwd.app_error",
		"api.user.login.bot_login_forbidden.app_error",
		"api.user.login.remote_users.login.error",
		"api.user.login.inactive.app_error",
		"api.user.login.not_verified.app_error",
		"api.user.check_user_login_attempts.too_many.app_error":
		c.Err = appErr
	default:
		c.Err = model.NewAppError("login", "api.user.login.invalid_credentials_sso", nil, "", http.StatusUnauthorized)
	}
}

func (s *Service) updateLinkedPassword(c *web.Context, w http.ResponseWriter, r *http.Request) {
	c.RequireUserId()
	if c.Err != nil {
		return
	}
	props := model.MapFromJSON(r.Body)
	newPassword := props["new_password"]

	auditRec := c.MakeAuditRecord(model.AuditEventUpdatePassword, model.AuditStatusFail)
	defer c.LogAuditRec(auditRec)
	user, appErr := c.App.GetUser(c.AppContext, c.Params.UserId)
	if appErr != nil {
		c.Err = appErr
		return
	}
	auditRec.AddEventPriorState(user)
	model.AddEventParameterAuditableToAuditRec(auditRec, "user", user)

	canUpdatePassword := false
	if user.IsSystemAdmin() {
		canUpdatePassword = c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionManageSystem)
	} else if user.IsBot {
		canUpdatePassword = c.App.SessionHasPermissionToManageBot(c.AppContext, *c.AppContext.Session(), user.Id) == nil
	} else {
		canUpdatePassword = c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionSysconsoleWriteUserManagementUsers)
	}
	isSelf := user.Id == c.AppContext.Session().UserId

	if props["already_hashed"] == "true" {
		if !canUpdatePassword {
			if isSelf {
				c.Err = model.NewAppError("updatePassword", "api.user.update_password.user_and_hashed.app_error", nil, "", http.StatusUnauthorized)
			} else {
				c.Err = model.NewAppError("updatePassword", "api.user.update_password.context.app_error", nil, "", http.StatusForbidden)
			}
			return
		}
	} else {
		if isSelf {
			storedHash, err := s.getCredential(r.Context(), user.Id)
			switch {
			case err == nil:
				currentPassword := props["current_password"]
				if currentPassword == "" {
					c.SetInvalidParam("current_password")
					return
				}
				if appErr = s.verifyCredential(c, user, storedHash, currentPassword, true); appErr != nil {
					if appErr.Id == "api.user.check_user_password.invalid.app_error" {
						appErr = model.NewAppError("updatePassword", "api.user.update_password.incorrect.app_error", nil, "", http.StatusBadRequest)
					}
					c.Err = appErr
					return
				}
			case errors.Is(err, sql.ErrNoRows):
				// An SSO session must not create a new local login method on an
				// account that was never linked to a local credential.
				c.Err = model.NewAppError("updatePassword", "api.user.update_password.context.app_error", nil, "linked credential is missing", http.StatusForbidden)
				return
			default:
				c.Err = model.NewAppError("updatePassword", "api.user.update_password.failed.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
				return
			}
		} else if !canUpdatePassword {
			c.Err = model.NewAppError("updatePassword", "api.user.update_password.context.app_error", nil, "", http.StatusForbidden)
			return
		}
		if appErr = c.App.IsPasswordValid(c.AppContext, newPassword); appErr != nil {
			c.Err = appErr
			return
		}
		hashedPassword, err := hashers.Hash(newPassword)
		if err != nil {
			c.Err = model.NewAppError("updatePassword", "api.user.update_password.password_hash.app_error", nil, "user_id="+user.Id, http.StatusInternalServerError).Wrap(err)
			return
		}
		newPassword = hashedPassword
	}

	if newPassword == "" {
		c.SetInvalidParam("new_password")
		return
	}
	if appErr = s.updateCredentialPassword(c.AppContext.Context(), user.Id, newPassword); appErr != nil {
		c.Err = appErr
		return
	}
	c.App.InvalidateCacheForUser(user.Id)
	if appErr = s.terminatePasswordSessions(c, user.Id); appErr != nil {
		c.Err = appErr
		return
	}

	auditRec.Success()
	auditRec.AddEventResultState(user)
	c.LogAudit("completed linked OpenID password update")
	web.ReturnStatusOK(w)
}

func (s *Service) terminatePasswordSessions(c *web.Context, userID string) *model.AppError {
	if !*c.App.Config().ServiceSettings.TerminateSessionsOnPasswordChange {
		return nil
	}
	currentSessionID := ""
	if c.AppContext.Session().UserId == userID {
		currentSessionID = c.AppContext.Session().Id
	}
	sessions, appErr := c.App.GetSessions(c.AppContext, userID)
	if appErr != nil {
		return model.NewAppError("UpdateLinkedOpenIDPassword", "api.user.update_password.failed.app_error", nil, "", http.StatusInternalServerError).Wrap(appErr)
	}
	for _, session := range sessions {
		if session.Id == currentSessionID {
			continue
		}
		if appErr = c.App.RevokeSessionById(c.AppContext, session.Id); appErr != nil {
			return model.NewAppError("UpdateLinkedOpenIDPassword", "api.user.update_password.failed.app_error", nil, "", http.StatusInternalServerError).Wrap(appErr)
		}
	}
	return nil
}

// Keep the local password and the new OpenID identity in the same commit.
// The ordinary UpdateUserAuth store call clears users.password separately.
func (s *Service) updateOpenIDAuthWithCredential(ctx context.Context, user *model.User, subject string) *model.AppError {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return model.NewAppError("LinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "database is unavailable", http.StatusInternalServerError)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return model.NewAppError("LinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	defer func() { _ = tx.Rollback() }()
	var currentPassword, currentService string
	var currentAuth sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT password,authservice,authdata FROM users WHERE id=$1 FOR UPDATE`, user.Id).
		Scan(&currentPassword, &currentService, &currentAuth)
	if err != nil {
		return model.NewAppError("LinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	if currentPassword != user.Password || currentService != user.AuthService ||
		currentAuth.Valid != (user.AuthData != nil) || (currentAuth.Valid && currentAuth.String != *user.AuthData) {
		return model.NewAppError("LinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "user authentication changed during linking", http.StatusConflict)
	}
	now := model.GetMillis()
	if err = upsertCredential(ctx, tx, user.Id, currentPassword, now); err != nil {
		return model.NewAppError("LinkOpenIDAccount", "api.user.update_password.failed.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE users SET password='',lastpasswordupdate=$2,updateat=$2,failedattempts=0,
		authservice=$3,authdata=$4 WHERE id=$1`, user.Id, now, model.ServiceOpenid, subject)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return model.NewAppError("LinkOpenIDAccount", "app.user.update_auth_data.email_exists.app_error", nil, "", http.StatusConflict).Wrap(err)
		}
		return model.NewAppError("LinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	if err = tx.Commit(); err != nil {
		return model.NewAppError("LinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	return nil
}

// Restore the password saved when an otherwise unused OpenID identifier was
// attached to a local account. Linked pairs use the dedicated unmerge route.
func (s *Service) restoreLocalAuthWithCredential(ctx context.Context, userID string) (bool, *model.AppError) {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return false, model.NewAppError("UnlinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "database is unavailable", http.StatusInternalServerError)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, model.NewAppError("UnlinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	defer func() { _ = tx.Rollback() }()
	var currentService, currentPassword string
	var currentAuth sql.NullString
	var deletedAt int64
	if err = tx.QueryRowContext(ctx, `SELECT authservice,authdata,password,deleteat FROM users WHERE id=$1 FOR UPDATE`, userID).
		Scan(&currentService, &currentAuth, &currentPassword, &deletedAt); err != nil {
		return false, model.NewAppError("UnlinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	if currentService != model.ServiceOpenid || !currentAuth.Valid || currentPassword != "" || deletedAt != 0 {
		return false, model.NewAppError("UnlinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "account authentication changed during unlink", http.StatusConflict)
	}
	var linked bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_openid_account_merges
		WHERE targetid=$1 OR sourceid=$1)`, userID).Scan(&linked); err != nil {
		return false, model.NewAppError("UnlinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	if linked {
		return false, model.NewAppError("UnlinkOpenIDAccount", "app.user.update_auth_data.email_exists.app_error", nil,
			"unlink the OpenID accounts with the dedicated route", http.StatusConflict)
	}
	var savedPassword string
	if err = tx.QueryRowContext(ctx, `SELECT password FROM workspace_linked_credentials WHERE userid=$1 FOR UPDATE`, userID).Scan(&savedPassword); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, model.NewAppError("UnlinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	if savedPassword == "" {
		return false, model.NewAppError("UnlinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "saved password is empty", http.StatusConflict)
	}
	now := model.GetMillis()
	if _, err = tx.ExecContext(ctx, `UPDATE users SET authservice='',authdata=NULL,password=$2,
		lastpasswordupdate=$3,updateat=$3,failedattempts=0 WHERE id=$1`, userID, savedPassword, now); err != nil {
		return false, model.NewAppError("UnlinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM workspace_linked_credentials WHERE userid=$1`, userID); err != nil {
		return false, model.NewAppError("UnlinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	if err = tx.Commit(); err != nil {
		return false, model.NewAppError("UnlinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	return true, nil
}

func (s *Service) migrateCredential(ctx context.Context, userID, previousHash, passwordHash string) (bool, *model.AppError) {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return false, model.NewAppError("MigrateLinkedOpenIDCredential", "api.user.update_password.failed.app_error", nil, "database is unavailable", http.StatusInternalServerError)
	}
	result, err := db.ExecContext(ctx, `UPDATE workspace_linked_credentials AS credentials
		SET password=$3,updateat=$4 WHERE userid=$1 AND password=$2
		AND EXISTS (SELECT 1 FROM users WHERE id=credentials.userid AND deleteat=0 AND authservice=$5)`,
		userID, previousHash, passwordHash, model.GetMillis(), model.ServiceOpenid)
	if err != nil {
		return false, model.NewAppError("MigrateLinkedOpenIDCredential", "api.user.update_password.failed.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, model.NewAppError("MigrateLinkedOpenIDCredential", "api.user.update_password.failed.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	return count == 1, nil
}

func (s *Service) verifyReplacementCredential(ctx context.Context, userID, password string) *model.AppError {
	var currentHash string
	err := s.app.Srv().Store().GetInternalMasterDB().QueryRowContext(ctx, `SELECT credentials.password
		FROM workspace_linked_credentials credentials JOIN users ON users.id=credentials.userid
		WHERE credentials.userid=$1 AND users.deleteat=0 AND users.authservice=$2`, userID, model.ServiceOpenid).Scan(&currentHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return model.NewAppError("MigrateLinkedOpenIDCredential", "api.user.update_password.failed.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	if err == nil {
		hasher, phc, parseErr := hashers.GetHasherFromPHCString(currentHash)
		if parseErr == nil && hasher.CompareHashAndPassword(phc, password) == nil {
			return nil
		}
	}
	return model.NewAppError("MigrateLinkedOpenIDCredential", "api.user.update_password.failed.app_error", nil,
		"linked credential changed during password hash migration", http.StatusConflict)
}

func (s *Service) updateCredentialPassword(ctx context.Context, userID, passwordHash string) *model.AppError {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return model.NewAppError("UpdateLinkedOpenIDCredential", "api.user.update_password.failed.app_error", nil, "database is unavailable", http.StatusInternalServerError)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return model.NewAppError("UpdateLinkedOpenIDCredential", "api.user.update_password.failed.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	defer func() { _ = tx.Rollback() }()

	now := model.GetMillis()
	if err = upsertCredential(ctx, tx, userID, passwordHash, now); err != nil {
		return model.NewAppError("UpdateLinkedOpenIDCredential", "api.user.update_password.failed.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE users
		SET lastpasswordupdate = $2,
			updateat = $2,
			failedattempts = 0
		WHERE id = $1 AND deleteat = 0 AND authservice = $3
	`, userID, now, model.ServiceOpenid)
	if err != nil {
		return model.NewAppError("UpdateLinkedOpenIDCredential", "api.user.update_password.failed.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		if err == nil {
			err = fmt.Errorf("expected one active OpenID user, updated %d", rows)
		}
		return model.NewAppError("UpdateLinkedOpenIDCredential", "api.user.update_password.failed.app_error", nil, "", http.StatusConflict).Wrap(err)
	}
	if err = tx.Commit(); err != nil {
		return model.NewAppError("UpdateLinkedOpenIDCredential", "api.user.update_password.failed.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	return nil
}

func upsertCredential(ctx context.Context, executor sqlExecutor, userID, passwordHash string, now int64) error {
	_, err := executor.ExecContext(ctx, `
		INSERT INTO workspace_linked_credentials (userid, password, createat, updateat)
		VALUES ($1, $2, $3, $3)
		ON CONFLICT (userid) DO UPDATE
		SET password = EXCLUDED.password, updateat = EXCLUDED.updateat
	`, userID, passwordHash, now)
	return err
}

func (s *Service) getCredential(ctx context.Context, userID string) (string, error) {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return "", errors.New("database is unavailable")
	}
	var passwordHash string
	err := db.QueryRowContext(ctx, `
		SELECT password FROM workspace_linked_credentials WHERE userid = $1
	`, userID).Scan(&passwordHash)
	return passwordHash, err
}
