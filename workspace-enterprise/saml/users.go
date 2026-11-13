package saml

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattermost/enterprise/internal/advisorylock"
	saml2 "github.com/mattermost/gosaml2"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/store"
)

func mapUser(info *saml2.AssertionInfo, cfg model.SamlSettings) (*model.User, *model.AppError) {
	for _, attr := range []*string{cfg.IdAttribute, cfg.EmailAttribute, cfg.UsernameAttribute, cfg.FirstNameAttribute,
		cfg.LastNameAttribute, cfg.NicknameAttribute, cfg.PositionAttribute, cfg.LocaleAttribute} {
		if *attr != "" && info.Values.GetSize(*attr) > 1 {
			return nil, samlError("ent.saml.invalid_attributes.app_error", http.StatusBadRequest)
		}
	}
	email := strings.ToLower(strings.TrimSpace(info.Values.Get(*cfg.EmailAttribute)))
	id := email
	if *cfg.IdAttribute != "" {
		id = info.Values.Get(*cfg.IdAttribute)
	}
	username := strings.ToLower(info.Values.Get(*cfg.UsernameAttribute))
	u := &model.User{AuthService: model.UserAuthServiceSaml, AuthData: &id, Email: email, EmailVerified: true,
		Username: username, FirstName: info.Values.Get(*cfg.FirstNameAttribute), LastName: info.Values.Get(*cfg.LastNameAttribute),
		Nickname: info.Values.Get(*cfg.NicknameAttribute), Position: info.Values.Get(*cfg.PositionAttribute),
		Locale: info.Values.Get(*cfg.LocaleAttribute), Roles: model.SystemUserRoleId}
	check := u.DeepCopy()
	check.PreSave(nil)
	if id == "" || len(id) > 128 || !utf8.ValidString(id) || email == "" || check.IsValid() != nil {
		return nil, samlError("ent.saml.invalid_attributes.app_error", http.StatusBadRequest)
	}
	return u, nil
}

// Role settings use field=value filters, including multi-valued group claims.
func matchesRole(info *saml2.AssertionInfo, filter string) bool {
	name, expected, ok := strings.Cut(filter, "=")
	if !ok || name == "" || expected == "" {
		return false
	}
	return slices.Contains(info.Values.GetAll(name), expected)
}

func samlIdentityLockKey(key string) int64 {
	hash := sha256.Sum256([]byte("workspace-saml:" + key))
	return int64(binary.BigEndian.Uint64(hash[:8]))
}

func (s *Service) identityLock(rctx request.CTX, key string) (*advisorylock.Lease, *model.AppError) {
	lease, err := s.identityLocks.AcquireLease(rctx.Context(), samlIdentityLockKey(key))
	if err != nil {
		return nil, samlError("ent.saml.store.app_error", http.StatusInternalServerError)
	}
	return lease, nil
}

func (s *Service) persistUser(rctx request.CTX, cfg model.SamlSettings, mapped *model.User, info *saml2.AssertionInfo, props map[string]string) (*model.User, bool, *model.AppError) {
	lease, appErr := s.identityLock(rctx, "identity:"+*mapped.AuthData)
	if appErr != nil {
		return nil, false, appErr
	}
	defer lease.Release()
	guest := matchesRole(info, *cfg.GuestAttribute)
	admin := *cfg.EnableAdminAttribute && matchesRole(info, *cfg.AdminAttribute)
	l := s.app.License()
	if guest && (!*s.app.Config().GuestAccountsSettings.Enable || l == nil || l.Features == nil || !model.SafeDereference(l.Features.GuestAccounts) || admin) {
		return nil, false, samlError("ent.saml.invalid_attributes.app_error", http.StatusForbidden)
	}
	u, err := s.app.Srv().Store().User().GetByAuth(mapped.AuthData, model.UserAuthServiceSaml)
	var missing *store.ErrNotFound
	if err != nil && !errors.As(err, &missing) {
		return nil, false, samlError("ent.saml.store.app_error", http.StatusInternalServerError)
	}
	if props["action"] == model.OAuthActionEmailToSSO {
		u, appErr = s.linkLocalUser(rctx, mapped, u, props["email_token"], lease)
		if appErr != nil {
			return nil, false, appErr
		}
	}
	created := false
	if u == nil {
		if guest {
			u, appErr = s.app.CreateGuest(rctx, mapped)
		} else {
			u, appErr = s.app.CreateUser(rctx, mapped)
		}
		created = appErr == nil
	} else if u.DeleteAt != 0 || u.IsBot {
		return nil, false, samlError("api.user.check_user_deleted.app_error", http.StatusUnauthorized)
	} else {
		u, appErr = s.updateMappedUser(rctx, cfg, u, mapped)
	}
	if appErr != nil {
		return nil, false, appErr
	}
	// Disabling EnableAdminAttribute preserves manually managed roles. When
	// enabled, a missing or non-matching field=value filter removes admin.
	manageAdmin := *cfg.EnableAdminAttribute
	if guest {
		manageAdmin, admin = true, false
	}
	if manageAdmin && u.IsSystemAdmin() != admin {
		if admin && u.IsGuest() {
			return nil, false, samlError("ent.saml.invalid_attributes.app_error", http.StatusForbidden)
		}
		roles := slices.DeleteFunc(strings.Fields(u.Roles), func(role string) bool { return role == model.SystemAdminRoleId })
		if admin {
			roles = append(roles, model.SystemAdminRoleId)
		}
		u, appErr = s.app.UpdateUserRoles(rctx, u.Id, strings.Join(roles, " "), true)
		if appErr != nil {
			return nil, false, appErr
		}
	}
	if guest && !u.IsGuest() {
		if appErr = s.app.DemoteUserToGuest(rctx, u); appErr != nil {
			return nil, false, appErr
		}
		u, appErr = s.app.GetUser(rctx, u.Id)
	}
	return u, created, appErr
}

func (s *Service) linkLocalUser(rctx request.CTX, mapped, linked *model.User, tokenValue string, lease *advisorylock.Lease) (*model.User, *model.AppError) {
	invalid := func() (*model.User, *model.AppError) {
		return nil, samlError("api.saml.invalid_email_token.app_error", http.StatusBadRequest)
	}
	if linked != nil || tokenValue == "" {
		return invalid()
	}
	token, appErr := s.app.GetSamlEmailToken(tokenValue)
	if appErr != nil || token.CreateAt > model.GetMillis() || token.CreateAt < time.Now().Add(-requestLifetime).UnixMilli() {
		return invalid()
	}
	u, appErr := s.app.GetUserByEmail(token.Extra)
	if appErr != nil {
		return invalid()
	}
	// The identity and local-account keys must share the same coordination
	// connection. Nested leases could consume all slots and wait on themselves.
	if err := lease.Lock(rctx.Context(), samlIdentityLockKey("local:"+u.Id)); err != nil {
		return nil, samlError("ent.saml.store.app_error", http.StatusInternalServerError)
	}
	u, appErr = s.app.GetUser(rctx, u.Id)
	if appErr != nil || u.DeleteAt != 0 || u.IsBot || u.AuthService != "" || u.Email != token.Extra {
		return invalid()
	}
	// Never transfer an address or username owned by another account.
	for _, lookup := range []func() (*model.User, error){
		func() (*model.User, error) { return s.app.Srv().Store().User().GetByEmail(mapped.Email) },
		func() (*model.User, error) { return s.app.Srv().Store().User().GetByUsername(mapped.Username) },
	} {
		other, err := lookup()
		var missing *store.ErrNotFound
		if (err != nil && !errors.As(err, &missing)) || (other != nil && other.Id != u.Id) {
			return invalid()
		}
	}
	consumed, err := s.app.Srv().Store().Token().ConsumeOnce(model.TokenTypeSaml, tokenValue)
	if err != nil || consumed.Extra != token.Extra || consumed.CreateAt != token.CreateAt {
		return invalid()
	}
	_, appErr = s.app.UpdateUserAuth(rctx, u.Id, &model.UserAuth{AuthService: model.UserAuthServiceSaml, AuthData: mapped.AuthData})
	if appErr != nil {
		return nil, appErr
	}
	return s.app.GetUser(rctx, u.Id)
}

func (s *Service) updateMappedUser(rctx request.CTX, cfg model.SamlSettings, old, mapped *model.User) (*model.User, *model.AppError) {
	// With LDAP synchronization enabled, the scheduled directory snapshot owns
	// these profile fields. SAML still authenticates and manages SAML roles.
	if *cfg.EnableSyncWithLdap {
		return old, nil
	}
	u := old.DeepCopy()
	u.Email, u.EmailVerified = mapped.Email, true
	for _, field := range []struct {
		mapping string
		to      *string
		value   string
	}{
		{*cfg.UsernameAttribute, &u.Username, mapped.Username}, {*cfg.FirstNameAttribute, &u.FirstName, mapped.FirstName},
		{*cfg.LastNameAttribute, &u.LastName, mapped.LastName}, {*cfg.NicknameAttribute, &u.Nickname, mapped.Nickname},
		{*cfg.PositionAttribute, &u.Position, mapped.Position}, {*cfg.LocaleAttribute, &u.Locale, mapped.Locale},
	} {
		if field.mapping != "" {
			*field.to = field.value
		}
	}
	if digest(u) == digest(old) {
		return old, nil
	}
	updated, err := s.app.Srv().Store().User().Update(rctx, u, true)
	if err != nil {
		return nil, samlError("ent.saml.profile_conflict.app_error", http.StatusConflict)
	}
	s.app.InvalidateCacheForUser(u.Id)
	return updated.New, nil
}

func (s *Service) CheckProviderAttributes(_ request.CTX, cfg *model.SamlSettings, old *model.User, patch *model.UserPatch) string {
	for _, field := range []struct {
		name    string
		mapping *string
		before  string
		after   *string
	}{
		{"username", cfg.UsernameAttribute, old.Username, patch.Username}, {"email", cfg.EmailAttribute, old.Email, patch.Email},
		{"first_name", cfg.FirstNameAttribute, old.FirstName, patch.FirstName}, {"last_name", cfg.LastNameAttribute, old.LastName, patch.LastName},
		{"nickname", cfg.NicknameAttribute, old.Nickname, patch.Nickname}, {"position", cfg.PositionAttribute, old.Position, patch.Position},
		{"locale", cfg.LocaleAttribute, old.Locale, patch.Locale},
	} {
		if model.SafeDereference(field.mapping) != "" && field.after != nil && *field.after != field.before {
			return field.name
		}
	}
	return ""
}
