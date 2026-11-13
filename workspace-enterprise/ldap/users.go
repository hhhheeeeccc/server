package ldap

import (
	"bytes"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	ldapclient "github.com/mattermost/ldap"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/store"
)

func ldapError(id string, status int) *model.AppError {
	return model.NewAppError("LDAP", id, nil, "", status)
}

func (s *Service) userSettings(login bool) (model.LdapSettings, *model.AppError) {
	cfg := s.app.Config().LdapSettings
	cfg.SetDefaults()
	l := s.app.License()
	if l == nil || l.Features == nil || !model.SafeDereference(l.Features.LDAP) ||
		(login && !*cfg.Enable) || (!login && !*cfg.Enable && !*cfg.EnableSync) {
		return cfg, ldapError("ent.ldap.disabled.app_error", http.StatusForbidden)
	}
	for _, name := range []string{*cfg.IdAttribute, *cfg.UsernameAttribute, *cfg.EmailAttribute, *cfg.LoginIdAttribute} {
		if !attributeName.MatchString(name) {
			return cfg, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
		}
	}
	return cfg, nil
}

func userAttributes(cfg model.LdapSettings) []string {
	attrs := []string{*cfg.IdAttribute, *cfg.LoginIdAttribute, *cfg.UsernameAttribute, *cfg.EmailAttribute,
		"userAccountControl", "nsAccountLock"}
	for _, name := range []*string{cfg.FirstNameAttribute, cfg.LastNameAttribute, cfg.NicknameAttribute, cfg.PositionAttribute} {
		if *name != "" && !slices.Contains(attrs, *name) {
			attrs = append(attrs, *name)
		}
	}
	return attrs
}

func userFilter(cfg model.LdapSettings) string {
	if *cfg.UserFilter != "" {
		return *cfg.UserFilter
	}
	return "(" + *cfg.IdAttribute + "=*)"
}

func (s *Service) userEntries(rctx request.CTX, cfg model.LdapSettings, attribute, value string, attrs []string) (*directory, []*ldapclient.Entry, *model.AppError) {
	filter := userFilter(cfg)
	if attribute != "" {
		f, err := groupIDFilter(attribute, value)
		if err != nil {
			return nil, nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
		}
		filter = "(&" + filter + f + ")"
	}
	d, err := connect(rctx, cfg, s.app.GetConfigFile)
	if err != nil {
		return nil, nil, ldapError("ent.ldap.do_login.unable_to_connect.app_error", http.StatusBadGateway)
	}
	entries, err := d.search(cfg, *cfg.BaseDN, filter, attrs, ldapclient.ScopeWholeSubtree)
	if err != nil {
		d.close()
		return nil, nil, ldapError("ent.ldap.search_failed.app_error", http.StatusBadGateway)
	}
	return d, entries, nil
}

func singleEntry(entries []*ldapclient.Entry) (*ldapclient.Entry, *model.AppError) {
	if len(entries) != 1 || entries[0].DN == "" {
		return nil, ldapError("ent.ldap.do_login.user_not_registered.app_error", http.StatusUnauthorized)
	}
	return entries[0], nil
}

func directoryDisabled(e *ldapclient.Entry) bool {
	control, _ := strconv.ParseUint(attributeValue(e, "userAccountControl"), 10, 32)
	return control&2 != 0 || strings.EqualFold(attributeValue(e, "nsAccountLock"), "true")
}

func mappedUser(e *ldapclient.Entry, cfg model.LdapSettings) (*model.User, *model.AppError) {
	id := entryID(e, *cfg.IdAttribute)
	u := &model.User{AuthService: model.UserAuthServiceLdap, AuthData: &id,
		Username: strings.ToLower(attributeValue(e, *cfg.UsernameAttribute)),
		Email:    strings.ToLower(attributeValue(e, *cfg.EmailAttribute)), EmailVerified: true,
		FirstName: attributeValue(e, *cfg.FirstNameAttribute), LastName: attributeValue(e, *cfg.LastNameAttribute),
		Nickname: attributeValue(e, *cfg.NicknameAttribute), Position: attributeValue(e, *cfg.PositionAttribute),
		Roles: model.SystemUserRoleId}
	// Validate before any writes, including before a synchronization can deactivate users.
	check := u.DeepCopy()
	check.PreSave(nil) // No password is present in a provider profile.
	if id == "" || len(id) > 128 || !utf8.ValidString(id) || check.IsValid() != nil {
		return nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
	}
	return u, nil
}

// GetUser resolves a login name without provisioning an account or authenticating it.
func (s *Service) GetUser(rctx request.CTX, login string) (*model.User, *model.AppError) {
	cfg, appErr := s.userSettings(true)
	if appErr != nil {
		return nil, appErr
	}
	d, entries, appErr := s.userEntries(rctx, cfg, *cfg.LoginIdAttribute, login, userAttributes(cfg))
	if appErr != nil {
		return nil, appErr
	}
	defer d.close()
	e, appErr := singleEntry(entries)
	if appErr != nil {
		return nil, appErr
	}
	if directoryDisabled(e) {
		return nil, ldapError("ent.ldap.do_login.invalid_password.app_error", http.StatusUnauthorized)
	}
	return mappedUser(e, cfg)
}

func (s *Service) GetLDAPUserForMMUser(rctx request.CTX, user *model.User) (*model.User, string, *model.AppError) {
	cfg, appErr := s.userSettings(false)
	if appErr != nil {
		return nil, "", appErr
	}
	if user == nil || !user.IsLDAPUser() || user.AuthData == nil {
		return nil, "", ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
	}
	d, entries, appErr := s.userEntries(rctx, cfg, *cfg.IdAttribute, *user.AuthData, userAttributes(cfg))
	if appErr != nil {
		return nil, "", appErr
	}
	defer d.close()
	e, appErr := singleEntry(entries)
	if appErr != nil {
		return nil, "", appErr
	}
	u, appErr := mappedUser(e, cfg)
	return u, e.DN, appErr
}

// ResolveSAMLUser resolves a signed SAML identity against LDAP before SAML
// authentication data is overridden. The IdP owns authentication; this lookup
// only selects the matching active directory identity and profile.
func (s *Service) ResolveSAMLUser(rctx request.CTX, samlID, email string, useID bool) (*model.User, *model.AppError) {
	cfg, appErr := s.userSettings(false)
	if appErr != nil {
		return nil, appErr
	}
	if !model.SafeDereference(cfg.EnableSync) {
		return nil, ldapError("ent.ldap.disabled.app_error", http.StatusForbidden)
	}
	attribute, value := *cfg.EmailAttribute, email
	if useID {
		attribute, value = *cfg.IdAttribute, samlID
	}
	d, entries, appErr := s.userEntries(rctx, cfg, attribute, value, userAttributes(cfg))
	if appErr != nil {
		return nil, appErr
	}
	defer d.close()
	e, appErr := singleEntry(entries)
	if appErr != nil {
		return nil, appErr
	}
	if directoryDisabled(e) {
		return nil, ldapError("ent.ldap.do_login.user_not_registered.app_error", http.StatusUnauthorized)
	}
	user, appErr := mappedUser(e, cfg)
	if appErr != nil {
		return nil, appErr
	}
	if !reflect.DeepEqual(cfg, s.app.Config().LdapSettings) {
		return nil, ldapError("ent.ldap.sync_config_changed.app_error", http.StatusConflict)
	}
	return user, nil
}

func (s *Service) GetUserAttributes(rctx request.CTX, id string, attributes []string) (map[string]string, *model.AppError) {
	cfg, appErr := s.userSettings(false)
	if appErr != nil {
		return nil, appErr
	}
	if len(attributes) == 0 {
		return map[string]string{}, nil
	}
	lookupAttribute, lookupValue := *cfg.IdAttribute, id
	samlCfg := s.app.Config().SamlSettings
	if model.SafeDereference(samlCfg.EnableSyncWithLdap) &&
		(!model.SafeDereference(samlCfg.EnableSyncWithLdapIncludeAuth) || model.SafeDereference(samlCfg.IdAttribute) == "") {
		// The native plugin API passes SAML AuthData here. In the default sync
		// mode that value is not an LDAP ID; resolve its account, then use the
		// same email mapping as the synchronization job.
		samlUser, err := s.app.Srv().Store().User().GetByAuth(&id, model.UserAuthServiceSaml)
		if err == nil {
			lookupAttribute, lookupValue = *cfg.EmailAttribute, samlUser.Email
		} else {
			var missing *store.ErrNotFound
			if !errors.As(err, &missing) {
				return nil, ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
			}
		}
	}
	d, entries, appErr := s.userEntries(rctx, cfg, lookupAttribute, lookupValue, attributes)
	if appErr != nil {
		return nil, appErr
	}
	defer d.close()
	e, appErr := singleEntry(entries)
	if appErr != nil {
		return nil, appErr
	}
	if directoryDisabled(e) {
		return nil, ldapError("ent.ldap.do_login.user_not_registered.app_error", http.StatusUnauthorized)
	}
	result := make(map[string]string, len(attributes))
	for _, attr := range attributes {
		result[attr] = attributeValue(e, attr)
	}
	return result, nil
}

// The caller supplies the immutable IdAttribute value, never a username or email.
func (s *Service) DoLogin(rctx request.CTX, id, password string) (*model.User, *model.AppError) {
	cfg, appErr := s.userSettings(true)
	if appErr != nil {
		return nil, appErr
	}
	if password == "" {
		return nil, ldapError("ent.ldap.do_login.invalid_password.app_error", http.StatusUnauthorized)
	}
	textFields, appErr := s.ldapMappedFields(rctx)
	if appErr != nil {
		return nil, appErr
	}
	d, entries, appErr := s.userEntries(rctx, cfg, *cfg.IdAttribute, id, appendLDAPMappedAttributes(userAttributes(cfg), textFields))
	if appErr != nil {
		return nil, appErr
	}
	defer d.close()
	e, appErr := singleEntry(entries)
	if appErr != nil {
		return nil, appErr
	}
	if directoryDisabled(e) {
		return nil, ldapError("ent.ldap.do_login.invalid_password.app_error", http.StatusUnauthorized)
	}
	u, appErr := mappedUser(e, cfg)
	if appErr != nil {
		return nil, appErr
	}
	// Evaluate filters with the service bind, before the connection is rebound as the user.
	admin, appErr := matchesUserFilter(d, cfg, id, *cfg.AdminFilter, *cfg.EnableAdminFilter)
	if appErr != nil {
		return nil, appErr
	}
	guest, appErr := matchesUserFilter(d, cfg, id, *cfg.GuestFilter, *s.app.Config().GuestAccountsSettings.Enable)
	if appErr != nil {
		return nil, appErr
	}
	if err := d.conn.Bind(e.DN, password); err != nil {
		if ldapclient.IsErrorWithCode(err, ldapclient.LDAPResultInvalidCredentials) {
			return nil, ldapError("ent.ldap.do_login.invalid_password.app_error", http.StatusUnauthorized)
		}
		return nil, ldapError("ent.ldap.do_login.unable_to_connect.app_error", http.StatusBadGateway)
	}
	values := make(map[string][]string, len(textFields))
	for _, field := range textFields {
		values[field.attribute] = attributeValues(e, field.attribute)
	}
	if appErr = validateLDAPCustomAttributes(values, textFields); appErr != nil {
		return nil, appErr
	}
	stored, err := s.app.Srv().Store().User().GetByAuth(u.AuthData, model.UserAuthServiceLdap)
	var missing *store.ErrNotFound
	switch {
	case err == nil:
		if stored.DeleteAt != 0 {
			return nil, ldapError("api.user.check_user_deleted.app_error", http.StatusUnauthorized)
		}
		stored, appErr = s.updateMappedUser(rctx, cfg, stored, u)
	case errors.As(err, &missing):
		if guest && !admin {
			stored, appErr = s.app.CreateGuest(rctx, u)
		} else {
			stored, appErr = s.app.CreateUser(rctx, u)
		}
	default:
		return nil, ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
	}
	if appErr != nil {
		return nil, appErr
	}
	if *cfg.EnableAdminFilter && stored.IsSystemAdmin() != admin {
		roles := strings.Fields(stored.Roles)
		roles = slices.DeleteFunc(roles, func(role string) bool { return role == model.SystemAdminRoleId })
		if admin {
			roles = append(roles, model.SystemAdminRoleId)
		}
		stored, appErr = s.app.UpdateUserRoles(rctx, stored.Id, strings.Join(roles, " "), true)
		if appErr != nil {
			return nil, appErr
		}
	}
	if appErr = s.FirstLoginSync(rctx, stored); appErr != nil {
		return nil, appErr
	}
	if appErr = s.syncCustomAttributes(rctx, stored.Id, values, textFields); appErr != nil {
		return nil, appErr
	}
	return stored, nil
}

func matchesUserFilter(d *directory, cfg model.LdapSettings, id, filter string, enabled bool) (bool, *model.AppError) {
	if !enabled || filter == "" {
		return false, nil
	}
	idFilter, err := groupIDFilter(*cfg.IdAttribute, id)
	if err != nil {
		return false, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
	}
	entries, err := d.search(cfg, *cfg.BaseDN, "(&"+userFilter(cfg)+idFilter+filter+")", []string{*cfg.IdAttribute}, ldapclient.ScopeWholeSubtree)
	if err != nil || len(entries) > 1 {
		return false, ldapError("ent.ldap.search_failed.app_error", http.StatusBadGateway)
	}
	return len(entries) == 1, nil
}

func (s *Service) updateMappedUser(rctx request.CTX, cfg model.LdapSettings, old, mapped *model.User) (*model.User, *model.AppError) {
	u := old.DeepCopy()
	u.Username, u.Email = mapped.Username, mapped.Email
	if *cfg.FirstNameAttribute != "" {
		u.FirstName = mapped.FirstName
	}
	if *cfg.LastNameAttribute != "" {
		u.LastName = mapped.LastName
	}
	if *cfg.NicknameAttribute != "" {
		u.Nickname = mapped.Nickname
	}
	if *cfg.PositionAttribute != "" {
		u.Position = mapped.Position
	}
	if u.Username == old.Username && u.Email == old.Email && u.FirstName == old.FirstName &&
		u.LastName == old.LastName && u.Nickname == old.Nickname && u.Position == old.Position {
		return old, nil
	}
	// Provider-owned email changes must not run the local-account verification flow.
	updated, err := s.app.Srv().Store().User().Update(rctx, u, true)
	if err != nil {
		return nil, ldapError("ent.ldap.store.app_error", http.StatusConflict)
	}
	s.app.InvalidateCacheForUser(u.Id)
	return updated.New, nil
}

func (s *Service) CheckProviderAttributes(_ request.CTX, cfg *model.LdapSettings, old *model.User, patch *model.UserPatch) string {
	for _, field := range []struct {
		name    string
		mapping *string
		before  string
		after   *string
	}{
		{"username", cfg.UsernameAttribute, old.Username, patch.Username}, {"email", cfg.EmailAttribute, old.Email, patch.Email},
		{"first_name", cfg.FirstNameAttribute, old.FirstName, patch.FirstName}, {"last_name", cfg.LastNameAttribute, old.LastName, patch.LastName},
		{"nickname", cfg.NicknameAttribute, old.Nickname, patch.Nickname}, {"position", cfg.PositionAttribute, old.Position, patch.Position},
	} {
		if model.SafeDereference(field.mapping) != "" && field.after != nil && *field.after != field.before {
			return field.name
		}
	}
	return ""
}

func (s *Service) SwitchToLdap(rctx request.CTX, userID, login, password string) *model.AppError {
	cfg, appErr := s.userSettings(true)
	if appErr != nil {
		return appErr
	}
	if password == "" {
		return ldapError("ent.ldap.do_login.invalid_password.app_error", http.StatusUnauthorized)
	}
	u, appErr := s.app.GetUser(rctx, userID)
	if appErr != nil {
		return appErr
	}
	if u.AuthService != "" || u.DeleteAt != 0 || u.IsBot {
		return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict)
	}
	d, entries, appErr := s.userEntries(rctx, cfg, *cfg.LoginIdAttribute, login, userAttributes(cfg))
	if appErr != nil {
		return appErr
	}
	defer d.close()
	e, appErr := singleEntry(entries)
	if appErr != nil {
		return appErr
	}
	mapped, appErr := mappedUser(e, cfg)
	if appErr != nil {
		return appErr
	}
	if directoryDisabled(e) || d.conn.Bind(e.DN, password) != nil {
		return ldapError("ent.ldap.do_login.invalid_password.app_error", http.StatusUnauthorized)
	}
	_, err := s.app.Srv().Store().User().GetByAuth(mapped.AuthData, model.UserAuthServiceLdap)
	var missing *store.ErrNotFound
	if !errors.As(err, &missing) {
		return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict)
	}
	_, appErr = s.app.UpdateUserAuth(rctx, userID, &model.UserAuth{AuthService: model.UserAuthServiceLdap, AuthData: mapped.AuthData})
	return appErr
}

func (s *Service) UpdateProfilePictureIfNecessary(rctx request.CTX, user model.User, _ model.Session) {
	cfg, appErr := s.userSettings(false)
	if appErr != nil || !user.IsLDAPUser() || user.AuthData == nil || *cfg.PictureAttribute == "" {
		return
	}
	attrs, appErr := s.GetUserAttributes(rctx, *user.AuthData, []string{*cfg.PictureAttribute})
	if appErr == nil {
		data := []byte(attrs[*cfg.PictureAttribute])
		if len(data) == 0 {
			if user.LastPictureUpdate > 0 {
				appErr = s.app.SetDefaultProfileImage(rctx, &user)
			}
		} else if len(data) > 10*1024*1024 {
			appErr = ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
		} else {
			appErr = s.app.SetProfileImageFromFile(rctx, user.Id, bytes.NewReader(data))
		}
	}
	if appErr != nil {
		rctx.Logger().Warn("LDAP profile picture synchronization failed", mlog.String("user_id", user.Id), mlog.Err(appErr))
	}
}
