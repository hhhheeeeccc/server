package user_no_email

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/mattermost/enterprise/database_migration"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const (
	schemaTimeout              = 30 * time.Second
	openIDEmailCleanupTimeout  = 10 * time.Second
	openIDEmailCleanupInterval = 20 * time.Millisecond
	openIDEmailCleanupDelay    = 500 * time.Millisecond
	openIDPlaceholderPrefix    = "no-email-openid-"
)

type Service struct {
	app *app.App

	schemaMu    sync.Mutex
	schemaReady bool

	pendingOpenID sync.Map
}

// Register installs the licensed Enterprise user routes before api4 registers
// the core routes with the same paths. The license matcher declines each route
// while no license is loaded, allowing Gorilla Mux to use the core route.
func Register(a *app.App) *Service {
	service := &Service{app: a}
	ctx, cancel := context.WithTimeout(context.Background(), schemaTimeout)
	if err := service.ensureEmailSchema(ctx); err != nil {
		a.Log().Error("Unable to migrate user email storage", mlog.Err(err))
	}
	cancel()
	createHandler := service.createUser

	a.Srv().Router.Handle(model.APIURLSuffix+"/users", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     createHandler,
		HandlerName:    web.GetHandlerName(createHandler),
		RequireSession: false,
		TrustRequester: false,
		RequireMfa:     false,
		IsStatic:       false,
		IsLocal:        false,
	}).MatcherFunc(licensedRoute(a)).Methods(http.MethodPost)

	registerSessionRoute(a, "/users/{user_id:[A-Za-z0-9]+}/roles", http.MethodPut, service.updateUserRoles)
	registerSessionRoute(a, "/users/{user_id:[A-Za-z0-9]+}/active", http.MethodPut, service.updateUserActive)
	registerSessionRoute(a, "/users/{user_id:[A-Za-z0-9]+}", http.MethodPut, service.updateUser)
	registerSessionRoute(a, "/users/{user_id:[A-Za-z0-9]+}/patch", http.MethodPut, service.patchUser)
	registerSessionRoute(a, "/users/{user_id:[A-Za-z0-9]+}", http.MethodDelete, service.deleteUser)
	registerSessionRoute(a, "/users/{user_id:[A-Za-z0-9]+}/status/custom", http.MethodPut, service.updateCustomStatus)
	registerSessionRoute(a, "/users/{user_id:[A-Za-z0-9]+}/status/custom", http.MethodDelete, service.removeCustomStatus)

	a.Srv().Go(service.clearStaleOpenIDPlaceholderEmails)
	a.Srv().Go(service.clearStaleCustomStatusPlaceholderEmails)
	return service
}

func registerSessionRoute(a *app.App, path, method string, handler func(*web.Context, http.ResponseWriter, *http.Request)) {
	a.Srv().Router.Handle(model.APIURLSuffix+path, &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).MatcherFunc(licensedRoute(a)).Methods(method)

	// Register the same isolated implementation for mmctl --local. Local mode
	// creates its unrestricted administrative session inside web.Handler, so it
	// must not require a pre-existing HTTP session.
	a.Srv().LocalRouter.Handle(model.APIURLSuffix+path, &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: false,
		IsLocal:        true,
	}).MatcherFunc(licensedRoute(a)).Methods(method)
}

func licensedRoute(a *app.App) mux.MatcherFunc {
	return func(_ *http.Request, _ *mux.RouteMatch) bool {
		return a != nil && a.License() != nil
	}
}

func (s *Service) createUser(c *web.Context, w http.ResponseWriter, r *http.Request) {
	var user model.User
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	err := decoder.Decode(&user)
	if err == nil {
		var trailing any
		if trailingErr := decoder.Decode(&trailing); trailingErr != io.EOF {
			err = trailingErr
			if err == nil {
				err = errors.New("unexpected data after user")
			}
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("CreateUserWithoutEmail", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("user", err)
		return
	}

	user.SanitizeInput(c.IsSystemAdmin())
	withoutEmail := strings.TrimSpace(user.Email) == ""

	tokenID := r.URL.Query().Get("t")
	inviteID := r.URL.Query().Get("iid")
	redirect := r.URL.Query().Get("r")

	auditRec := c.MakeAuditRecord(model.AuditEventCreateUser, model.AuditStatusFail)
	defer c.LogAuditRec(auditRec)
	model.AddEventParameterToAuditRec(auditRec, "invite_id", inviteID)
	model.AddEventParameterToAuditRec(auditRec, "redirect", redirect)
	model.AddEventParameterAuditableToAuditRec(auditRec, "user", &user)

	if withoutEmail {
		if c.App.License() == nil {
			c.Err = model.NewAppError("CreateUserWithoutEmail", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
			return
		}

		// An account with no email address cannot receive a welcome or verification
		// message. This flag is consumed by the normal Mattermost creation flow and
		// prevents it from trying to send to the temporary validation address.
		user.DisableWelcomeEmail = true

		if err := s.ensureEmailSchema(r.Context()); err != nil {
			s.setStorageError(c, "CreateUserWithoutEmail.prepareSchema", err)
			return
		}

		// Core validation still requires a syntactically valid email. The reserved
		// .invalid TLD makes the temporary value undeliverable. It is replaced by
		// an actual empty string in the database before the response is returned.
		user.Email = "no-email-" + model.NewId() + "@users.invalid"
		user.EmailVerified = false
	}

	var createdUser *model.User
	var appErr *model.AppError

	switch {
	case tokenID != "":
		var token *model.Token
		token, appErr = c.App.GetTokenById(tokenID)
		if appErr != nil {
			c.Err = appErr
			return
		}
		auditRec.AddMeta("token_type", token.Type)

		if token.Type == model.TokenTypeGuestInvitation {
			if c.App.Channels().License() == nil {
				c.Err = model.NewAppError("CreateUserWithToken", "api.user.create_user.guest_accounts.license.app_error", nil, "", http.StatusBadRequest)
				return
			}
			if !*c.App.Config().GuestAccountsSettings.Enable {
				c.Err = model.NewAppError("CreateUserWithToken", "api.user.create_user.guest_accounts.disabled.app_error", nil, "", http.StatusBadRequest)
				return
			}
		}

		createdUser, appErr = c.App.CreateUserWithToken(c.AppContext, &user, token)
	case inviteID != "":
		createdUser, appErr = c.App.CreateUserWithInviteId(c.AppContext, &user, inviteID, redirect)
	case c.IsSystemAdmin():
		createdUser, appErr = c.App.CreateUserAsAdmin(c.AppContext, &user, redirect)
		auditRec.AddMeta("admin", true)
	default:
		createdUser, appErr = c.App.CreateUserFromSignup(c.AppContext, &user, redirect)
	}

	if appErr != nil {
		c.Err = appErr
		return
	}

	responseUser := createdUser
	if withoutEmail {
		emailState, err := s.clearStoredEmail(createdUser.Id, createdUser.Email)
		if err != nil {
			c.Logger.Error("Unable to clear the temporary email after creating a user",
				mlog.String("user_id", createdUser.Id),
				mlog.Err(err),
			)

			if cleanupErr := c.App.PermanentDeleteUser(c.AppContext, createdUser); cleanupErr != nil {
				c.Logger.Error("Unable to remove a user after clearing its temporary email failed",
					mlog.String("user_id", createdUser.Id),
					mlog.Err(cleanupErr),
				)
			}

			c.Err = model.NewAppError("CreateUserWithoutEmail.clearStoredEmail", "app.user.save.app_error", nil, "", http.StatusInternalServerError)
			return
		}

		c.App.InvalidateCacheForUser(createdUser.Id)
		responseUser = emailState.apply(createdUser)
	}

	auditRec.Success()
	auditRec.AddEventResultState(responseUser)
	auditRec.AddEventObjectType("user")

	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(responseUser); err != nil {
		c.Logger.Warn("Error while writing response", mlog.Err(err))
	}
}

func (s *Service) ensureEmailSchema(parent context.Context) error {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()

	if s.schemaReady {
		return nil
	}

	config := s.app.Config()
	if config == nil || config.SqlSettings.DriverName == nil {
		return errors.New("SQL driver is not configured")
	}
	if *config.SqlSettings.DriverName != model.DatabaseDriverPostgres {
		return fmt.Errorf("unsupported SQL driver %q", *config.SqlSettings.DriverName)
	}

	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return errors.New("master database connection is unavailable")
	}

	ctx, cancel := context.WithTimeout(parent, schemaTimeout)
	defer cancel()

	if err := database_migration.RepairUserEmail(ctx, db); err != nil {
		return fmt.Errorf("migrate user email storage: %w", err)
	}
	s.schemaReady = true
	return nil
}

// PrepareOpenIDUser gives Mattermost a unique, non-deliverable address for the
// duration of its normal OAuth validation and persistence flow. Once the user
// has been created or linked by OpenID subject, the address is cleared from the
// database so installations whose identity provider has no email claim retain
// a genuinely empty email field.
func (s *Service) PrepareOpenIDUser(parent context.Context, user *model.User) error {
	if user == nil || strings.TrimSpace(user.Email) != "" {
		return nil
	}
	if s.app.License() == nil {
		return errors.New("an active license is required for OpenID users without email")
	}
	if user.AuthService != model.ServiceOpenid || user.AuthData == nil || strings.TrimSpace(*user.AuthData) == "" {
		return errors.New("OpenID subject is required for a user without email")
	}
	if parent == nil {
		parent = context.Background()
	}
	if err := s.ensureEmailSchema(parent); err != nil {
		return err
	}

	subject := *user.AuthData
	digest := sha256.Sum256([]byte(subject))
	placeholder := fmt.Sprintf("%s%x@users.invalid", openIDPlaceholderPrefix, digest[:16])
	user.Email = placeholder
	user.EmailVerified = false
	user.DisableWelcomeEmail = true

	if _, loaded := s.pendingOpenID.LoadOrStore(subject, placeholder); loaded {
		return nil
	}
	s.app.Srv().Go(func() {
		defer s.pendingOpenID.Delete(subject)
		if err := s.waitAndClearOpenIDEmail(subject, placeholder); err != nil {
			s.app.Log().Warn("Unable to clear an OpenID placeholder email", mlog.Err(err))
		}
	})

	return nil
}

func (s *Service) waitAndClearOpenIDEmail(subject, placeholder string) error {
	ctx, cancel := context.WithTimeout(context.Background(), openIDEmailCleanupTimeout)
	defer cancel()

	// GetUserFromJSON runs before the core OAuth flow updates or creates the
	// database row. Waiting briefly avoids treating an already-empty stored
	// account as finished just before core writes the temporary address.
	initialDelay := time.NewTimer(openIDEmailCleanupDelay)
	defer initialDelay.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-initialDelay.C:
	}

	ticker := time.NewTicker(openIDEmailCleanupInterval)
	defer ticker.Stop()

	for {
		userID, done, err := s.clearOpenIDEmail(ctx, subject, placeholder)
		if err != nil {
			return err
		}
		if done {
			if userID != "" {
				s.app.InvalidateCacheForUser(userID)
			}
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for OpenID user persistence: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Service) clearOpenIDEmail(ctx context.Context, subject, placeholder string) (string, bool, error) {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return "", false, errors.New("master database connection is unavailable")
	}

	var userID string
	err := db.QueryRowContext(ctx, `
		UPDATE users
		SET email = '', emailverified = FALSE
		WHERE authservice = $1
		  AND authdata = $2
		  AND email = $3
		RETURNING id
	`, model.ServiceOpenid, subject, placeholder).Scan(&userID)
	if err == nil {
		return userID, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("clear OpenID placeholder email: %w", err)
	}

	var storedEmail string
	err = db.QueryRowContext(ctx, `
		SELECT id, email
		FROM users
		WHERE authservice = $1 AND authdata = $2
	`, model.ServiceOpenid, subject).Scan(&userID, &storedEmail)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspect OpenID user email: %w", err)
	}
	if storedEmail == "" {
		return userID, true, nil
	}
	return "", false, nil
}

func (s *Service) clearStaleOpenIDPlaceholderEmails() {
	ctx, cancel := context.WithTimeout(context.Background(), schemaTimeout)
	defer cancel()

	if err := s.ensureEmailSchema(ctx); err != nil {
		s.app.Log().Warn("Unable to prepare storage for OpenID users without email", mlog.Err(err))
		return
	}
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return
	}

	rows, err := db.QueryContext(ctx, `
		UPDATE users
		SET email = '', emailverified = FALSE
		WHERE authservice = $1
		  AND email LIKE $2
		RETURNING id
	`, model.ServiceOpenid, openIDPlaceholderPrefix+"%@users.invalid")
	if err != nil {
		s.app.Log().Warn("Unable to recover OpenID placeholder emails", mlog.Err(err))
		return
	}
	defer rows.Close()
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			s.app.Log().Warn("Unable to read a recovered OpenID user", mlog.Err(err))
			return
		}
		s.app.InvalidateCacheForUser(userID)
	}
	if err := rows.Err(); err != nil {
		s.app.Log().Warn("Unable to finish recovering OpenID placeholder emails", mlog.Err(err))
	}
}

func (s *Service) setStorageError(c *web.Context, where string, err error) {
	c.Logger.Error("Unable to prepare storage for users without email", mlog.Err(err))
	status := http.StatusInternalServerError
	if errors.Is(err, errStoredEmailChanged) {
		status = http.StatusConflict
	}
	c.Err = model.NewAppError(where, "app.user.save.app_error", nil, "", status)
}

func (s *Service) updateUserRoles(c *web.Context, w http.ResponseWriter, r *http.Request) {
	c.RequireUserId()
	if c.Err != nil {
		return
	}

	var input struct {
		Roles string `json:"roles"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&input)
	if err == nil {
		var trailing any
		if trailingErr := decoder.Decode(&trailing); trailingErr != io.EOF {
			err = trailingErr
			if err == nil {
				err = errors.New("unexpected data after roles request")
			}
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("UpdateUserRoles", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("roles", err)
		return
	}
	newRoles := input.Roles
	if !model.IsValidUserRoles(newRoles) {
		c.SetInvalidParam("roles")
		return
	}

	for _, roleName := range strings.Fields(newRoles) {
		for _, roleID := range model.NewSystemRoleIDs {
			license := c.App.Channels().License()
			if roleName == roleID && (license == nil || license.Features == nil || !model.SafeDereference(license.Features.CustomPermissionsSchemes)) {
				c.Err = model.NewAppError("UpdateUserRoles", "api.user.update_user_roles.license.app_error", nil, "", http.StatusBadRequest)
				return
			}
		}
	}

	auditRec := c.MakeAuditRecord(model.AuditEventUpdateUserRoles, model.AuditStatusFail)
	model.AddEventParameterToAuditRec(auditRec, "roles", newRoles)
	defer c.LogAuditRec(auditRec)

	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionManageRoles) {
		c.SetPermissionError(model.PermissionManageRoles)
		return
	}

	user, appErr := c.App.GetUser(c.AppContext, c.Params.UserId)
	if appErr != nil {
		appErr.StatusCode = http.StatusBadRequest
		c.Err = appErr
		return
	}
	mutableUser, clearEmail := s.mutableUser(c, user)
	if c.Err != nil {
		return
	}

	updatedUser, appErr := c.App.UpdateUserRolesWithUser(c.AppContext, mutableUser, newRoles, true)
	if clearEmail {
		emailState, err := s.clearStoredEmail(user.Id, mutableUser.Email)
		if err != nil {
			s.setStorageError(c, "UpdateUserRolesWithoutEmail.clearStoredEmail", err)
			return
		}
		c.App.InvalidateCacheForUser(user.Id)
		if updatedUser != nil {
			updatedUser = emailState.apply(updatedUser)
		}
	}
	if appErr != nil {
		c.Err = appErr
		return
	}

	auditRec.Success()
	auditRec.AddEventResultState(updatedUser)
	auditRec.AddEventObjectType("user")
	c.LogAudit(fmt.Sprintf("user=%s roles=%s", c.Params.UserId, newRoles))
	web.ReturnStatusOK(w)
}

func (s *Service) updateUser(c *web.Context, w http.ResponseWriter, r *http.Request) {
	c.RequireUserId()
	if c.Err != nil {
		return
	}

	auditRec := c.MakeAuditRecord(model.AuditEventUpdateUser, model.AuditStatusFail)
	defer c.LogAuditRec(auditRec)

	var user model.User
	if err := json.NewDecoder(r.Body).Decode(&user); err != nil {
		c.SetInvalidParamWithErr("user", err)
		return
	}
	model.AddEventParameterAuditableToAuditRec(auditRec, "user", &user)

	if user.Id != c.Params.UserId {
		c.SetInvalidParam("user_id")
		return
	}
	if !c.App.SessionHasPermissionToUserOrBot(c.AppContext, *c.AppContext.Session(), user.Id) {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}

	oldUser, appErr := c.App.GetUser(c.AppContext, user.Id)
	if appErr != nil {
		c.Err = appErr
		return
	}
	if !c.IsSystemAdmin() && lastActiveVisibilityChanged(oldUser.Props, user.Props) {
		c.Err = model.NewAppError("UpdateUser", "api.user.update_user.personal_data_locked.app_error", nil, "", http.StatusForbidden)
		return
	}
	if oldUser.IsSystemAdmin() && !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionManageSystem) {
		c.SetPermissionError(model.PermissionManageSystem)
		return
	}
	auditRec.AddEventPriorState(oldUser)
	auditRec.AddEventObjectType("user")

	if c.AppContext.Session().IsOAuth && oldUser.Email != user.Email {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		c.Err.DetailedError += ", attempted email update by oauth app"
		return
	}
	if field := c.App.CheckProviderAttributes(c.AppContext, oldUser, user.ToPatch()); field != "" {
		c.Err = model.NewAppError("UpdateUserWithoutEmail", "api.user.update_user.login_provider_attribute_set.app_error", map[string]any{"Field": field}, "", http.StatusConflict)
		return
	}
	if field := c.App.CheckLockedProfileFields(*c.AppContext.Session(), oldUser, user.ToPatch()); field != "" {
		c.Err = model.NewAppError("UpdateUserWithoutEmail", "api.user.update_user.profile_field_locked.app_error", map[string]any{"Field": field}, "", http.StatusConflict)
		return
	}

	clearingEmail := strings.TrimSpace(user.Email) == "" && strings.TrimSpace(oldUser.Email) != ""
	if user.Email != "" && oldUser.Email != user.Email && c.AppContext.Session().UserId == c.Params.UserId {
		if appErr = c.App.DoubleCheckPassword(c.AppContext, oldUser, user.Password); appErr != nil {
			c.SetInvalidParam("password")
			return
		}
	}

	keepEmailEmpty := strings.TrimSpace(oldUser.Email) == "" && strings.TrimSpace(user.Email) == ""
	useEmptyEmailFlow := keepEmailEmpty || clearingEmail
	var emailState storedEmailState
	var temporaryEmail string
	if useEmptyEmailFlow {
		if s.app.License() == nil {
			c.Err = model.NewAppError("UpdateUserWithoutEmail", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
			return
		}
		if err := s.ensureEmailSchema(r.Context()); err != nil {
			s.setStorageError(c, "UpdateUserWithoutEmail.prepareSchema", err)
			return
		}

		temporaryEmail = "no-email-" + model.NewId() + "@users.invalid"
		if keepEmailEmpty {
			if err := s.prepareStoredEmail(r.Context(), oldUser.Id, temporaryEmail); err != nil {
				s.setStorageError(c, "UpdateUserWithoutEmail.prepare", err)
				return
			}
			c.App.InvalidateCacheForUser(oldUser.Id)
		}
		user.Email = temporaryEmail
		user.EmailVerified = false
	}

	updatedUser, appErr := c.App.UpdateUserAsUser(c.AppContext, &user, c.IsSystemAdmin())
	if useEmptyEmailFlow {
		var err error
		emailState, err = s.clearStoredEmail(oldUser.Id, temporaryEmail)
		if err != nil {
			s.setStorageError(c, "UpdateUserWithoutEmail.restore", err)
			return
		}
		c.App.InvalidateCacheForUser(oldUser.Id)
	}
	if appErr != nil {
		c.Err = appErr
		return
	}
	if useEmptyEmailFlow {
		updatedUser = emailState.apply(updatedUser)
	}

	auditRec.Success()
	auditRec.AddEventResultState(updatedUser)
	c.LogAudit("")
	if err := json.NewEncoder(w).Encode(updatedUser); err != nil {
		c.Logger.Warn("Error while writing response", mlog.Err(err))
	}
}

func (s *Service) patchUser(c *web.Context, w http.ResponseWriter, r *http.Request) {
	c.RequireUserId()
	if c.Err != nil {
		return
	}

	var patch model.UserPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		c.SetInvalidParamWithErr("user", err)
		return
	}
	patch.RemoteId = nil

	auditRec := c.MakeAuditRecord(model.AuditEventPatchUser, model.AuditStatusFail)
	model.AddEventParameterAuditableToAuditRec(auditRec, "user_patch", &patch)
	defer c.LogAuditRec(auditRec)

	if !c.App.SessionHasPermissionToUserOrBot(c.AppContext, *c.AppContext.Session(), c.Params.UserId) {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}

	oldUser, appErr := c.App.GetUser(c.AppContext, c.Params.UserId)
	if appErr != nil {
		c.SetInvalidParam("user_id")
		return
	}
	if !c.IsSystemAdmin() && patch.Props != nil && lastActiveVisibilityChanged(oldUser.Props, patch.Props) {
		c.Err = model.NewAppError("PatchUser", "api.user.update_user.personal_data_locked.app_error", nil, "", http.StatusForbidden)
		return
	}
	auditRec.AddEventPriorState(oldUser)
	auditRec.AddEventObjectType("user")

	if oldUser.IsSystemAdmin() && !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionManageSystem) {
		c.SetPermissionError(model.PermissionManageSystem)
		return
	}
	if c.AppContext.Session().IsOAuth && patch.Email != nil && oldUser.Email != *patch.Email {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		c.Err.DetailedError += ", attempted email update by oauth app"
		return
	}
	// Workspace administrators manage profile fields from the system console,
	// including accounts whose login identity comes from OpenID. Keep the
	// provider-owned field restriction for ordinary users.
	if field := c.App.CheckProviderAttributes(c.AppContext, oldUser, &patch); field != "" && !c.IsSystemAdmin() {
		c.Err = model.NewAppError("PatchUserWithoutEmail", "api.user.patch_user.login_provider_attribute_set.app_error", map[string]any{"Field": field}, "", http.StatusConflict)
		return
	}
	if field := c.App.CheckLockedProfileFields(*c.AppContext.Session(), oldUser, &patch); field != "" {
		c.Err = model.NewAppError("PatchUserWithoutEmail", "api.user.patch_user.profile_field_locked.app_error", map[string]any{"Field": field}, "", http.StatusConflict)
		return
	}
	clearingEmail := patch.Email != nil && strings.TrimSpace(*patch.Email) == "" && strings.TrimSpace(oldUser.Email) != ""
	if patch.Email != nil && oldUser.Email != *patch.Email && c.AppContext.Session().UserId == c.Params.UserId && !clearingEmail {
		if patch.Password == nil {
			c.SetInvalidParam("password")
			return
		}
		if appErr = c.App.DoubleCheckPassword(c.AppContext, oldUser, *patch.Password); appErr != nil {
			c.Err = appErr
			return
		}
	}

	keepEmailEmpty := strings.TrimSpace(oldUser.Email) == "" && (patch.Email == nil || strings.TrimSpace(*patch.Email) == "")
	useEmptyEmailFlow := keepEmailEmpty || clearingEmail
	var emailState storedEmailState
	var temporaryEmail string
	if useEmptyEmailFlow {
		if s.app.License() == nil {
			c.Err = model.NewAppError("PatchUserWithoutEmail", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
			return
		}
		if err := s.ensureEmailSchema(r.Context()); err != nil {
			s.setStorageError(c, "PatchUserWithoutEmail.prepareSchema", err)
			return
		}
		temporaryEmail = "no-email-" + model.NewId() + "@users.invalid"
		if keepEmailEmpty {
			if err := s.prepareStoredEmail(r.Context(), oldUser.Id, temporaryEmail); err != nil {
				s.setStorageError(c, "PatchUserWithoutEmail.prepare", err)
				return
			}
			c.App.InvalidateCacheForUser(oldUser.Id)

			// An explicit empty email is already the stored state. Leaving it in the
			// patch would replace the temporary validation value before core validates
			// the complete user record.
			patch.Email = nil
		} else {
			// Core validates the complete record before updating it. Use the same
			// temporary address during that validation, then clear it below.
			patch.Email = &temporaryEmail
		}
	}

	updatedUser, appErr := c.App.PatchUser(c.AppContext, c.Params.UserId, &patch, c.IsSystemAdmin())
	if useEmptyEmailFlow {
		var err error
		emailState, err = s.clearStoredEmail(oldUser.Id, temporaryEmail)
		if err != nil {
			s.setStorageError(c, "PatchUserWithoutEmail.restore", err)
			return
		}
		c.App.InvalidateCacheForUser(oldUser.Id)
	}
	if appErr != nil {
		c.Err = appErr
		return
	}
	if useEmptyEmailFlow {
		updatedUser = emailState.apply(updatedUser)
	}

	c.App.SetAutoResponderStatus(c.AppContext, updatedUser, oldUser.NotifyProps)
	auditRec.Success()
	auditRec.AddEventResultState(updatedUser)
	c.LogAudit("")

	if err := json.NewEncoder(w).Encode(updatedUser); err != nil {
		c.Logger.Warn("Error while writing response", mlog.Err(err))
	}
}

func (s *Service) updateUserActive(c *web.Context, w http.ResponseWriter, r *http.Request) {
	c.RequireUserId()
	if c.Err != nil {
		return
	}

	var props map[string]any
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	err := decoder.Decode(&props)
	if err == nil {
		var trailing any
		if trailingErr := decoder.Decode(&trailing); trailingErr != io.EOF {
			err = trailingErr
			if err == nil {
				err = errors.New("unexpected data after activation request")
			}
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("UpdateUserActive", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("active", err)
		return
	}
	active, ok := props["active"].(bool)
	if !ok {
		c.SetInvalidParam("active")
		return
	}

	auditRec := c.MakeAuditRecord(model.AuditEventUpdateUserActive, model.AuditStatusFail)
	defer c.LogAuditRec(auditRec)
	model.AddEventParameterToAuditRec(auditRec, "active", active)

	isSelfDeactivate := !active && c.Params.UserId == c.AppContext.Session().UserId
	if !isSelfDeactivate && !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionSysconsoleWriteUserManagementUsers) {
		c.Err = model.NewAppError("UpdateUserActive", "api.user.update_active.permissions.app_error", nil, "userId="+c.Params.UserId, http.StatusForbidden)
		return
	}
	if isSelfDeactivate && !model.SafeDereference(c.App.Config().TeamSettings.EnableUserDeactivation) {
		c.Err = model.NewAppError("UpdateUserActive", "api.user.update_active.not_enable.app_error", nil, "userId="+c.Params.UserId, http.StatusUnauthorized)
		return
	}

	user, appErr := c.App.GetUser(c.AppContext, c.Params.UserId)
	if appErr != nil {
		c.Err = appErr
		return
	}
	auditRec.AddEventPriorState(user)
	auditRec.AddEventObjectType("user")

	if user.IsSystemAdmin() && !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionManageSystem) {
		c.SetPermissionError(model.PermissionManageSystem)
		return
	}
	if user.IsBot {
		if permissionErr := c.App.SessionHasPermissionToManageBot(c.AppContext, *c.AppContext.Session(), c.Params.UserId); permissionErr != nil {
			c.Err = permissionErr
			return
		}
	}
	if active && user.IsGuest() && !model.SafeDereference(c.App.Config().GuestAccountsSettings.Enable) {
		c.Err = model.NewAppError("UpdateUserActive", "api.user.update_active.cannot_enable_guest_when_guest_feature_is_disabled.app_error", nil, "userId="+c.Params.UserId, http.StatusUnauthorized)
		return
	}
	if user.AuthService == model.UserAuthServiceLdap {
		c.Err = model.NewAppError("UpdateUserActive", "api.user.update_active.cannot_modify_status_when_user_is_managed_by_ldap.app_error", nil, "userId="+c.Params.UserId, http.StatusForbidden)
		return
	}

	mutableUser, clearEmail := s.mutableUser(c, user)
	if c.Err != nil {
		return
	}
	_, appErr = c.App.UpdateActive(c.AppContext, mutableUser, active)
	if clearEmail {
		if _, err := s.clearStoredEmail(user.Id, mutableUser.Email); err != nil {
			s.setStorageError(c, "UpdateUserActiveWithoutEmail.clearStoredEmail", err)
			return
		}
		c.App.InvalidateCacheForUser(user.Id)
	}
	if appErr != nil {
		c.Err = appErr
		return
	}

	auditRec.Success()
	c.LogAudit(fmt.Sprintf("user_id=%s active=%v", user.Id, active))
	message := model.NewWebSocketEvent(model.WebsocketEventUserActivationStatusChange, "", "", "", nil, "")
	c.App.Publish(message)
	web.ReturnStatusOK(w)
}

func (s *Service) deleteUser(c *web.Context, w http.ResponseWriter, r *http.Request) {
	c.RequireUserId()
	if c.Err != nil {
		return
	}

	userID := c.Params.UserId
	permanent := c.Params.Permanent
	auditRec := c.MakeAuditRecord(model.AuditEventDeleteUser, model.AuditStatusFail)
	model.AddEventParameterToAuditRec(auditRec, "user_id", userID)
	model.AddEventParameterToAuditRec(auditRec, "permanent", permanent)
	defer c.LogAuditRec(auditRec)

	if !c.App.SessionHasPermissionToUserOrBot(c.AppContext, *c.AppContext.Session(), userID) {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}
	if userID == c.AppContext.Session().UserId && !model.SafeDereference(c.App.Config().TeamSettings.EnableUserDeactivation) && !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionManageSystem) {
		c.Err = model.NewAppError("DeleteUser", "api.user.update_active.not_enable.app_error", nil, "userId="+userID, http.StatusUnauthorized)
		return
	}

	user, appErr := c.App.GetUser(c.AppContext, userID)
	if appErr != nil {
		c.Err = appErr
		return
	}
	auditRec.AddEventPriorState(user)
	auditRec.AddEventObjectType("user")

	if user.IsSystemAdmin() && !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionManageSystem) {
		c.SetPermissionError(model.PermissionManageSystem)
		return
	}

	if permanent {
		if !model.SafeDereference(c.App.Config().ServiceSettings.EnableAPIUserDeletion) {
			loggedUser, loggedUserErr := c.App.GetUser(c.AppContext, c.AppContext.Session().UserId)
			if loggedUserErr == nil && loggedUser != nil && loggedUser.IsSystemAdmin() {
				c.Err = model.NewAppError("DeleteUser", "api.user.delete_user.not_enabled.for_admin.app_error", nil, "userId="+userID, http.StatusUnauthorized)
			} else {
				c.Err = model.NewAppError("DeleteUser", "api.user.delete_user.not_enabled.app_error", nil, "userId="+userID, http.StatusUnauthorized)
			}
			return
		}

		permanentUser, restoreEmptyEmail := s.mutableUser(c, user)
		if c.Err != nil {
			return
		}
		appErr = c.App.PermanentDeleteUser(c.AppContext, permanentUser)
		if appErr != nil && restoreEmptyEmail {
			if _, err := s.clearStoredEmail(user.Id, permanentUser.Email); err != nil {
				s.setStorageError(c, "DeleteUserWithoutEmail.restore", err)
				return
			}
			c.App.InvalidateCacheForUser(user.Id)
		}
	} else {
		mutableUser, clearEmail := s.mutableUser(c, user)
		if c.Err != nil {
			return
		}
		_, appErr = c.App.UpdateActive(c.AppContext, mutableUser, false)
		if clearEmail {
			if _, err := s.clearStoredEmail(user.Id, mutableUser.Email); err != nil {
				s.setStorageError(c, "DeleteUserWithoutEmail.clearStoredEmail", err)
				return
			}
			c.App.InvalidateCacheForUser(user.Id)
		}
	}
	if appErr != nil {
		c.Err = appErr
		return
	}

	auditRec.Success()
	web.ReturnStatusOK(w)
}

func (s *Service) mutableUser(c *web.Context, user *model.User) (*model.User, bool) {
	if strings.TrimSpace(user.Email) != "" {
		return user, false
	}
	if s.app.License() == nil {
		c.Err = model.NewAppError("MutateUserWithoutEmail", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return nil, false
	}

	mutable := user.DeepCopy()
	mutable.Email = "no-email-" + model.NewId() + "@users.invalid"
	mutable.EmailVerified = false
	return mutable, true
}
