package intune

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mattermost/enterprise/internal/advisorylock"
	"github.com/mattermost/enterprise/oauth/profile"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

type Service struct {
	app           *app.App
	client        *http.Client
	db            *sql.DB
	indexInitErr  error
	mu            sync.Mutex
	keys          map[string]*keyCache
	identityLocks *advisorylock.Locker
}

var _ einterfaces.IntuneInterface = (*Service)(nil)
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const maxIdentityLockConnections = 16

func New(a *app.App) einterfaces.IntuneInterface {
	s := &Service{app: a, keys: make(map[string]*keyCache), db: a.Srv().Store().GetInternalMasterDB(),
		identityLocks: advisorylock.NewWithCapacity(model.SafeDereference(a.Config().SqlSettings.DataSource), maxIdentityLockConnections), client: &http.Client{
			Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}}
	indexCtx, indexCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	s.indexInitErr = s.ensureLookupIndex(indexCtx)
	indexCancel()
	if s.indexInitErr != nil {
		a.Log().Error("Unable to initialize Intune identity index", mlog.Err(s.indexInitErr))
	}
	return s
}

func intuneError(id string, status int) *model.AppError {
	return model.NewAppError("Intune", id, nil, "", status)
}

func (s *Service) IsConfigured() bool {
	cfg := s.app.Config().IntuneSettings
	l := s.app.License()
	if !model.MinimumEnterpriseAdvancedLicense(l) || l.Features == nil || !model.SafeDereference(cfg.Enable) || cfg.IsValid() != nil {
		return false
	}
	if *cfg.AuthService == model.UserAuthServiceSaml {
		return model.SafeDereference(l.Features.SAML) && model.SafeDereference(s.app.Config().SamlSettings.Enable)
	}
	return model.SafeDereference(l.Features.Office365OAuth) && model.SafeDereference(s.app.Config().Office365Settings.Enable)
}

type tokenClaims struct {
	jwt.RegisteredClaims
	Version           string `json:"ver"`
	Tenant            string `json:"tid"`
	ObjectID          string `json:"oid"`
	Scope             string `json:"scp"`
	IdentityType      string `json:"idtyp"`
	Email             string `json:"email"`
	UPN               string `json:"upn"`
	PreferredUsername string `json:"preferred_username"`
	GivenName         string `json:"given_name"`
	FamilyName        string `json:"family_name"`
}

func (s *Service) Login(rctx request.CTX, accessToken string) (*model.User, *model.AppError) {
	if !s.IsConfigured() {
		return nil, intuneError("ent.intune.login.not_configured.app_error", http.StatusForbidden)
	}
	cfg := s.app.Config().IntuneSettings
	if s.indexInitErr != nil {
		return nil, intuneError("ent.intune.login.extract_auth_data.app_error", http.StatusServiceUnavailable)
	}
	if len(accessToken) == 0 || len(accessToken) > 64*1024 {
		return nil, intuneError("ent.intune.validate_token.invalid_token.app_error", http.StatusBadRequest)
	}
	// This parse only selects one of two fixed Microsoft discovery endpoints.
	// No claim is used as identity until the signed token passes validation.
	preview := &tokenClaims{}
	_, _, err := jwt.NewParser().ParseUnverified(accessToken, preview)
	if err != nil || (preview.Version != "1.0" && preview.Version != "2.0") {
		return nil, intuneError("ent.intune.validate_token.invalid_token.app_error", http.StatusBadRequest)
	}
	tenant, clientID := strings.ToLower(*cfg.TenantId), *cfg.ClientId
	issuer := issuerFor(tenant, preview.Version)
	audience := clientID
	if preview.Version == "1.0" {
		audience = "api://" + clientID
	}
	claims := &tokenClaims{}
	var keyErr error
	parsed, err := jwt.ParseWithClaims(accessToken, claims, func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" || len(kid) > 256 {
			return nil, errors.New("invalid key identifier")
		}
		key, err := s.signingKey(rctx.Context(), tenant, preview.Version, kid)
		if err != nil {
			keyErr = err
		}
		return key, err
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(issuer), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(30*time.Second))
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, intuneError("ent.intune.validate_token.token_expired.app_error", http.StatusUnauthorized)
		}
		if errors.Is(keyErr, errKeyService) {
			return nil, intuneError("ent.intune.validate_token.jwks_init.app_error", http.StatusInternalServerError)
		}
		return nil, intuneError("ent.intune.validate_token.invalid_token.app_error", http.StatusBadRequest)
	}
	// ClientId is a UUID, whose case is insignificant. The public client
	// config exposes it in the administrator's original case as IntuneScope.
	if !parsed.Valid || !strings.EqualFold(claims.Tenant, tenant) || !uuidPattern.MatchString(claims.ObjectID) || claims.Subject == "" ||
		claims.IssuedAt == nil || len(claims.Audience) != 1 || !strings.EqualFold(claims.Audience[0], audience) ||
		claims.IdentityType == "app" || !slices.Contains(strings.Fields(claims.Scope), "login.mattermost") {
		return nil, intuneError("ent.intune.validate_token.missing_claims.app_error", http.StatusBadRequest)
	}
	claims.ObjectID = strings.ToLower(claims.ObjectID)
	current := s.app.Config().IntuneSettings
	if !s.IsConfigured() || *current.TenantId != *cfg.TenantId || *current.ClientId != *cfg.ClientId || *current.AuthService != *cfg.AuthService {
		return nil, intuneError("ent.intune.login.not_configured.app_error", http.StatusForbidden)
	}
	// Do not use email/UPN to authorize or merge an existing account.
	lockKey := sha256.Sum256([]byte("workspace-intune:" + *cfg.AuthService + ":" + claims.ObjectID))
	lease, err := s.identityLocks.AcquireLease(rctx.Context(), int64(binary.BigEndian.Uint64(lockKey[:8])))
	if err != nil {
		return nil, intuneError("ent.intune.login.extract_auth_data.app_error", http.StatusInternalServerError)
	}
	defer lease.Release()
	current = s.app.Config().IntuneSettings
	if !s.IsConfigured() || *current.TenantId != *cfg.TenantId || *current.ClientId != *cfg.ClientId || *current.AuthService != *cfg.AuthService {
		return nil, intuneError("ent.intune.login.not_configured.app_error", http.StatusForbidden)
	}
	// Older accounts may have stored the same UUID with uppercase letters.
	// Resolve against the primary while holding the normalized identity lock;
	// never select one account arbitrarily if both spellings exist.
	var userIDs []string
	err = lease.Query(rctx.Context(), `
		SELECT id FROM users
		WHERE authservice = $1 AND lower(authdata) = $2
		LIMIT 2 FOR UPDATE`, []any{*cfg.AuthService, claims.ObjectID}, func(rows *sql.Rows) error {
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			userIDs = append(userIDs, id)
		}
		return nil
	})
	if err != nil {
		return nil, intuneError("ent.intune.login.extract_auth_data.app_error", http.StatusInternalServerError)
	}
	if len(userIDs) > 1 {
		return nil, intuneError("ent.intune.login.extract_auth_data.app_error", http.StatusConflict)
	}
	var u *model.User
	if len(userIDs) == 1 {
		var storeErr error
		u, storeErr = s.app.Srv().Store().User().Get(rctx.With(app.RequestContextWithMaster), userIDs[0])
		if storeErr != nil {
			return nil, intuneError("ent.intune.login.extract_auth_data.app_error", http.StatusInternalServerError)
		}
	}
	if u == nil {
		if *cfg.AuthService == model.UserAuthServiceSaml {
			return nil, intuneError("ent.intune.login.account_not_found.app_error", http.StatusPreconditionRequired)
		}
		if !*s.app.Config().TeamSettings.EnableUserCreation {
			return nil, intuneError("ent.intune.login.account_not_found.app_error", http.StatusForbidden)
		}
		email := claims.Email
		if email == "" {
			email = claims.UPN
		}
		if email == "" {
			email = claims.PreferredUsername
		}
		username := strings.SplitN(claims.PreferredUsername, "@", 2)[0]
		var err error
		u, err = profile.User(rctx, model.ServiceOffice365, claims.ObjectID, email, username, claims.GivenName, claims.FamilyName, true)
		if err != nil {
			return nil, intuneError("ent.intune.validate_token.missing_claims.app_error", http.StatusBadRequest)
		}
		var appErr *model.AppError
		u, appErr = s.app.CreateUser(rctx, u)
		if appErr != nil {
			return nil, appErr
		}
	}
	if u.IsBot {
		return nil, intuneError("api.user.login_by_intune.bot_login_forbidden.app_error", http.StatusForbidden)
	}
	if u.DeleteAt != 0 {
		return nil, intuneError("api.user.login_by_intune.account_locked.app_error", http.StatusConflict)
	}
	if appErr := s.app.CheckUserAllAuthenticationCriteria(rctx, u, ""); appErr != nil {
		return nil, appErr
	}
	return u, nil
}
