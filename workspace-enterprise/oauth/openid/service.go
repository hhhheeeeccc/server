package openid

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

const (
	discoveryCacheTTL = time.Hour
	maxDiscoveryBytes = 1024 * 1024
)

type discoveryFields uint8

const (
	discoveryAuthorization discoveryFields = 1 << iota
	discoveryToken
	discoveryUserInfo
	discoveryAll = discoveryAuthorization | discoveryToken | discoveryUserInfo
)

type DiscoveryResponse struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserInfoEndpoint      string `json:"userinfo_endpoint"`
}

type User struct {
	Subject           string `json:"sub"`
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	Nickname          string `json:"nickname"`
	Name              string `json:"name"`
	GivenName         string `json:"given_name"`
	FamilyName        string `json:"family_name"`
}

type Provider struct {
	appMu             sync.RWMutex
	app               *app.App
	licenseCheck      func() bool
	discoveryMu       sync.Mutex
	discoveryURL      string
	discovery         DiscoveryResponse
	discoveryExpires  time.Time
	discoveryRequests singleflight.Group
	emailProvisioner  EmailProvisioner
}

type EmailProvisioner interface {
	PrepareOpenIDUser(context.Context, *model.User) error
}

func New() *Provider {
	return &Provider{}
}

func (p *Provider) Register() {
	einterfaces.RegisterOAuthProvider(model.ServiceOpenid, p)
}

func (p *Provider) Bind(a *app.App) {
	p.appMu.Lock()
	p.app = a
	p.appMu.Unlock()
}

func (p *Provider) BindEmailProvisioner(provisioner EmailProvisioner) {
	p.appMu.Lock()
	p.emailProvisioner = provisioner
	p.appMu.Unlock()
}

func (p *Provider) GetUserFromJSON(rctx request.CTX, data io.Reader, _ *model.User, settings *model.SSOSettings) (*model.User, error) {
	var openIDUser User
	if err := json.NewDecoder(data).Decode(&openIDUser); err != nil {
		return nil, err
	}
	if openIDUser.Subject == "" {
		return nil, errors.New("OpenID subject is empty")
	}

	storedUser := p.getLinkedUser(openIDUser.Subject)
	incomingEmail := strings.ToLower(strings.TrimSpace(openIDUser.Email))
	if incomingEmail != "" && !model.IsValidEmail(incomingEmail) {
		incomingEmail = ""
	}

	username := openIDUser.Nickname
	if settings != nil && model.SafeDereference(settings.UsePreferredUsername) {
		username = openIDUser.PreferredUsername
	}
	if username == "" {
		username = openIDUser.Nickname
	}
	if username == "" {
		username = openIDUser.Name
	}
	if username == "" {
		username = openIDUser.Email
	}
	if storedUser != nil {
		// Once an administrator has linked this subject, the Mattermost account
		// remains authoritative for its username and profile. This prevents a
		// provider that only sends `sub` from renaming the local user on login.
		username = storedUser.Username
	} else if username == "" {
		digest := sha256.Sum256([]byte(openIDUser.Subject))
		username = fmt.Sprintf("openid-%x", digest[:8])
	}

	user := &model.User{
		Username:    model.CleanUsername(rctx.Logger(), username),
		Email:       incomingEmail,
		AuthData:    model.NewPointer(openIDUser.Subject),
		AuthService: model.ServiceOpenid,
	}
	if openIDUser.GivenName != "" {
		user.FirstName = openIDUser.GivenName
		user.LastName = openIDUser.FamilyName
	} else if openIDUser.Name != "" {
		parts := strings.Fields(openIDUser.Name)
		if len(parts) > 0 {
			user.FirstName = parts[0]
		}
		if len(parts) > 1 {
			user.LastName = strings.Join(parts[1:], " ")
		}
	}
	if storedUser != nil {
		// The linked Mattermost account owns its profile. Returning its existing
		// values keeps the core OAuth refresh from replacing an empty email or a
		// locally selected name with claims supplied by the identity provider.
		user.Username = storedUser.Username
		user.Email = storedUser.Email
		user.FirstName = storedUser.FirstName
		user.LastName = storedUser.LastName
	}
	if user.Email == "" {
		if storedUser != nil {
			return user, nil
		}
		p.appMu.RLock()
		provisioner := p.emailProvisioner
		p.appMu.RUnlock()
		if provisioner == nil {
			return nil, errors.New("OpenID user has no email and email-less provisioning is unavailable")
		}
		ctx := context.Background()
		if rctx != nil {
			ctx = rctx.Context()
		}
		if err := provisioner.PrepareOpenIDUser(ctx, user); err != nil {
			return nil, fmt.Errorf("prepare OpenID user without email: %w", err)
		}
	}
	return user, nil
}

func (p *Provider) getLinkedUser(subject string) *model.User {
	p.appMu.RLock()
	a := p.app
	p.appMu.RUnlock()
	if a == nil {
		return nil
	}
	user, err := a.Srv().Store().User().GetByAuth(model.NewPointer(subject), model.ServiceOpenid)
	if err != nil {
		return nil
	}
	return user
}

func (p *Provider) GetSSOSettings(rctx request.CTX, config *model.Config, _ string) (*model.SSOSettings, error) {
	if !p.isLicensed() {
		return nil, errors.New("OpenID requires an active licensed feature")
	}
	if config == nil {
		return nil, errors.New("OpenID configuration is unavailable")
	}
	settings := config.OpenIdSettings
	settings.Id = trimmedStringPointer(settings.Id)
	settings.Secret = trimmedStringPointer(settings.Secret)

	var required discoveryFields
	if model.SafeDereference(settings.AuthEndpoint) == "" {
		required |= discoveryAuthorization
	}
	if model.SafeDereference(settings.TokenEndpoint) == "" {
		required |= discoveryToken
	}
	if model.SafeDereference(settings.UserAPIEndpoint) == "" {
		required |= discoveryUserInfo
	}
	if model.SafeDereference(settings.Enable) && model.SafeDereference(settings.DiscoveryEndpoint) != "" && required != 0 {
		discovery, err := p.getDiscovery(rctx, *settings.DiscoveryEndpoint, required)
		if err != nil {
			return nil, err
		}
		if required&discoveryAuthorization != 0 {
			settings.AuthEndpoint = model.NewPointer(discovery.AuthorizationEndpoint)
		}
		if required&discoveryToken != 0 {
			settings.TokenEndpoint = model.NewPointer(discovery.TokenEndpoint)
		}
		if required&discoveryUserInfo != 0 {
			settings.UserAPIEndpoint = model.NewPointer(discovery.UserInfoEndpoint)
		}
	}

	if model.SafeDereference(settings.Enable) &&
		(!validDiscoveryURL(model.SafeDereference(settings.AuthEndpoint)) ||
			!validDiscoveryURL(model.SafeDereference(settings.TokenEndpoint)) ||
			!validDiscoveryURL(model.SafeDereference(settings.UserAPIEndpoint))) {
		return nil, errors.New("OpenID configuration is missing a valid endpoint")
	}
	return &settings, nil
}

func (*Provider) GetUserFromIdToken(request.CTX, string) (*model.User, error) {
	return nil, nil
}

func (*Provider) IsSameUser(_ request.CTX, stored, incoming *model.User) bool {
	if stored == nil || incoming == nil || stored.AuthService != model.ServiceOpenid || incoming.AuthService != model.ServiceOpenid {
		return false
	}
	if stored.AuthData == nil || incoming.AuthData == nil || *stored.AuthData == "" {
		return false
	}
	return *stored.AuthData == *incoming.AuthData
}

func (p *Provider) getDiscovery(rctx request.CTX, endpoint string, required discoveryFields) (DiscoveryResponse, error) {
	ctx := context.Background()
	if rctx != nil {
		ctx = rctx.Context()
	}
	if err := ctx.Err(); err != nil {
		return DiscoveryResponse{}, err
	}
	if cached, ok := p.cachedDiscovery(endpoint); ok && hasDiscoveryFields(cached, required) {
		return cached, nil
	}

	key := endpoint + "#" + strconv.FormatUint(uint64(required), 10)
	result := p.discoveryRequests.DoChan(key, func() (any, error) {
		if cached, ok := p.cachedDiscovery(endpoint); ok && hasDiscoveryFields(cached, required) {
			return cached, nil
		}
		fetchContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		discovery, err := fetchDiscovery(fetchContext, endpoint)
		if err != nil {
			return nil, err
		}
		if !hasDiscoveryFields(discovery, required) {
			return nil, errors.New("OpenID discovery is missing a required endpoint")
		}
		p.discoveryMu.Lock()
		p.discoveryURL = endpoint
		p.discovery = discovery
		p.discoveryExpires = time.Now().Add(discoveryCacheTTL)
		p.discoveryMu.Unlock()
		return discovery, nil
	})

	select {
	case <-ctx.Done():
		return DiscoveryResponse{}, ctx.Err()
	case response := <-result:
		if response.Err != nil {
			return DiscoveryResponse{}, response.Err
		}
		return response.Val.(DiscoveryResponse), nil
	}
}

func (p *Provider) cachedDiscovery(endpoint string) (DiscoveryResponse, bool) {
	p.discoveryMu.Lock()
	defer p.discoveryMu.Unlock()
	return p.discovery, p.discoveryURL == endpoint && time.Now().Before(p.discoveryExpires)
}

func (p *Provider) isLicensed() bool {
	if p.licenseCheck != nil {
		return p.licenseCheck()
	}
	p.appMu.RLock()
	a := p.app
	p.appMu.RUnlock()
	if a == nil {
		return false
	}
	activeLicense := a.License()
	return activeLicense != nil && activeLicense.Features != nil && model.SafeDereference(activeLicense.Features.OpenId)
}

func hasDiscoveryFields(discovery DiscoveryResponse, required discoveryFields) bool {
	return (required&discoveryAuthorization == 0 || discovery.AuthorizationEndpoint != "") &&
		(required&discoveryToken == 0 || discovery.TokenEndpoint != "") &&
		(required&discoveryUserInfo == 0 || discovery.UserInfoEndpoint != "")
}

func validDiscoveryURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Host != "" && parsed.User == nil && parsed.Fragment == "" &&
		(parsed.Scheme == "https" || parsed.Scheme == "http")
}

func fetchDiscovery(ctx context.Context, endpoint string) (DiscoveryResponse, error) {
	if !validDiscoveryURL(endpoint) {
		return DiscoveryResponse{}, errors.New("invalid OpenID discovery URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return DiscoveryResponse{}, errors.New("invalid OpenID discovery request")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return DiscoveryResponse{}, errors.New("unable to retrieve OpenID discovery document")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return DiscoveryResponse{}, errors.New("OpenID discovery request failed")
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxDiscoveryBytes+1))
	if err != nil || len(body) > maxDiscoveryBytes {
		return DiscoveryResponse{}, errors.New("invalid OpenID discovery response")
	}
	var discovery DiscoveryResponse
	if err := json.Unmarshal(body, &discovery); err != nil || discovery == (DiscoveryResponse{}) {
		return DiscoveryResponse{}, errors.New("invalid OpenID discovery response")
	}
	for _, endpoint := range []string{discovery.AuthorizationEndpoint, discovery.TokenEndpoint, discovery.UserInfoEndpoint} {
		if endpoint != "" && !validDiscoveryURL(endpoint) {
			return DiscoveryResponse{}, errors.New("invalid OpenID discovery response")
		}
	}
	return discovery, nil
}

func trimmedStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	return model.NewPointer(strings.TrimSpace(*value))
}
