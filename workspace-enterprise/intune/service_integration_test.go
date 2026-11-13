package intune

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

const (
	fixtureTenant = "11111111-2222-3333-4444-555555555555"
	fixtureClient = "66666666-7777-8888-9999-aaaaaaaaaaaa"
	fixtureUser   = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
)

func TestMain(m *testing.M) {
	app.RegisterIntuneInterface(New)
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

type fixture struct {
	th       *api4.TestHelper
	s        *Service
	server   *httptest.Server
	mu       sync.Mutex
	private  *rsa.PrivateKey
	kid      string
	keysURL  string
	fail     bool
	requests atomic.Int32
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func setupIntune(t *testing.T) *fixture {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	f := &fixture{private: private, kid: "key-1", keysURL: "https://login.microsoftonline.com/keys"}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if f.fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration") {
			version := "1.0"
			if strings.Contains(r.URL.Path, "/v2.0/") {
				version = "2.0"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuerFor(fixtureTenant, version), "jwks_uri": f.keysURL})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kid": f.kid, "kty": "RSA", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(f.private.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.private.E)).Bytes())}}})
	}))
	t.Cleanup(f.server.Close)
	f.th = api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	}).InitBasic(t)
	require.True(t, f.th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	f.th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.Office365Settings.Enable = new(true)
		cfg.Office365Settings.Id = new(fixtureClient)
		cfg.Office365Settings.Secret = new("synthetic-test-client-secret")
		cfg.IntuneSettings = model.IntuneSettings{Enable: new(true), TenantId: new(fixtureTenant), ClientId: new(fixtureClient), AuthService: new(model.ServiceOffice365)}
	})
	f.s = f.th.App.Intune().(*Service)
	endpoint, err := url.Parse(f.server.URL)
	require.NoError(t, err)
	transport := f.server.Client().Transport
	f.s.client.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if !trustedEndpoint(r.URL.String()) {
			t.Errorf("untrusted outbound request host")
			return nil, errKeyService
		}
		copy := r.Clone(r.Context())
		copy.URL.Scheme, copy.URL.Host, copy.Host = endpoint.Scheme, endpoint.Host, endpoint.Host
		return transport.RoundTrip(copy)
	})
	require.True(t, f.s.IsConfigured())
	return f
}

func validClaims() tokenClaims {
	return tokenClaims{Version: "2.0", Tenant: fixtureTenant, ObjectID: fixtureUser, Scope: "login.mattermost", IdentityType: "user",
		Email: "intune@example.test", PreferredUsername: "intune_user@example.test", GivenName: "First", FamilyName: "Last",
		RegisteredClaims: jwt.RegisteredClaims{Subject: "pairwise-subject", Issuer: issuerFor(fixtureTenant, "2.0"), Audience: jwt.ClaimStrings{fixtureClient},
			IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)), NotBefore: jwt.NewNumericDate(time.Now().Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(time.Now().Add(10 * time.Minute))}}
}

func (f *fixture) token(t *testing.T, claims tokenClaims) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = f.kid
	value, err := token.SignedString(f.private)
	require.NoError(t, err)
	return value
}

func TestIntuneHTTPLoginProvisionsAndReusesStableAccount(t *testing.T) {
	f := setupIntune(t)
	claims := validClaims()
	raw, err := json.Marshal(model.IntuneLoginRequest{AccessToken: f.token(t, claims)})
	require.NoError(t, err)
	response, err := http.Post(f.th.Client.URL+"/oauth/intune", "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var u model.User
	require.NoError(t, json.NewDecoder(response.Body).Decode(&u))
	require.Equal(t, "intune_user", u.Username)
	var sessionToken string
	for _, c := range response.Cookies() {
		if c.Name == model.SessionCookieToken {
			sessionToken = c.Value
		}
	}
	require.NotEmpty(t, sessionToken)
	client := f.th.CreateClient()
	client.SetToken(sessionToken)
	me, _, err := client.GetMe(f.th.Context.Context(), "")
	require.NoError(t, err)
	require.Equal(t, u.Id, me.Id)
	claims.Email, claims.PreferredUsername = "changed@example.test", "changed"
	again, appErr := f.s.Login(f.th.Context, f.token(t, claims))
	require.Nil(t, appErr)
	require.Equal(t, u.Id, again.Id)
	require.Equal(t, "intune_user", again.Username, "mobile sign-in preserves provider-owned profile fields")
	require.Equal(t, int32(2), f.requests.Load(), "discovery and keys are cached")
	f.th.Server.SetLicense(nil)
	_, appErr = f.s.Login(f.th.Context, f.token(t, claims))
	require.Equal(t, http.StatusForbidden, appErr.StatusCode)
	require.Equal(t, int32(2), f.requests.Load())
}

func TestIntuneObjectIDCaseDoesNotCreateAnotherAccount(t *testing.T) {
	f := setupIntune(t)
	claims := validClaims()
	first, appErr := f.s.Login(f.th.Context, f.token(t, claims))
	require.Nil(t, appErr)
	claims.ObjectID = strings.ToUpper(claims.ObjectID)
	claims.Email = "alternate-intune@example.test"
	claims.PreferredUsername = "alternate-intune@example.test"
	again, appErr := f.s.Login(f.th.Context, f.token(t, claims))
	require.Nil(t, appErr)
	require.Equal(t, first.Id, again.Id)
	require.Equal(t, fixtureUser, *again.AuthData)
}

func TestIntuneReusesLegacyUppercaseObjectID(t *testing.T) {
	f := setupIntune(t)
	legacyObjectID := strings.ToUpper(fixtureUser)
	u, appErr := f.th.App.CreateUser(f.th.Context, &model.User{
		AuthService: model.ServiceOffice365,
		AuthData:    &legacyObjectID,
		Email:       "legacy-intune@example.test",
		Username:    "legacy_intune",
	})
	require.Nil(t, appErr)
	claims := validClaims()
	got, appErr := f.s.Login(f.th.Context, f.token(t, claims))
	require.Nil(t, appErr)
	require.Equal(t, u.Id, got.Id)
}

func TestIntuneRejectsAmbiguousLegacyObjectID(t *testing.T) {
	f := setupIntune(t)
	for _, candidate := range []struct {
		authData string
		name     string
	}{
		{authData: fixtureUser, name: "lowercase_intune"},
		{authData: strings.ToUpper(fixtureUser), name: "uppercase_intune"},
	} {
		_, appErr := f.th.App.CreateUser(f.th.Context, &model.User{
			AuthService: model.ServiceOffice365,
			AuthData:    &candidate.authData,
			Email:       candidate.name + "@example.test",
			Username:    candidate.name,
		})
		require.Nil(t, appErr)
	}
	_, appErr := f.s.Login(f.th.Context, f.token(t, validClaims()))
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusConflict, appErr.StatusCode)
}

func TestIntuneRepairsInterruptedLookupIndex(t *testing.T) {
	f := setupIntune(t)
	for _, candidate := range []struct {
		authData string
		name     string
	}{
		{authData: fixtureUser, name: "index_lower"},
		{authData: strings.ToUpper(fixtureUser), name: "index_upper"},
	} {
		_, appErr := f.th.App.CreateUser(f.th.Context, &model.User{
			AuthService: model.ServiceOffice365,
			AuthData:    &candidate.authData,
			Email:       candidate.name + "@example.test",
			Username:    candidate.name,
		})
		require.Nil(t, appErr)
	}
	db := f.th.Store.GetInternalMasterDB()
	_, err := db.Exec(`DROP INDEX CONCURRENTLY idx_workspace_intune_users_auth_lower`)
	require.NoError(t, err)
	// PostgreSQL leaves the index name behind with indisvalid=false after
	// a failed concurrent build, which IF NOT EXISTS alone would skip.
	writer, err := db.Begin()
	require.NoError(t, err)
	defer writer.Rollback()
	_, err = writer.Exec(`UPDATE users SET updateat=updateat WHERE id=$1`, f.th.BasicUser.Id)
	require.NoError(t, err)
	buildCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = db.ExecContext(buildCtx, `CREATE INDEX CONCURRENTLY idx_workspace_intune_users_auth_lower
		ON users (authservice, lower(authdata)) WHERE authdata IS NOT NULL`)
	require.Error(t, err)
	require.NoError(t, writer.Rollback())
	var valid bool
	require.NoError(t, db.QueryRow(`SELECT indisvalid FROM pg_index WHERE indexrelid='idx_workspace_intune_users_auth_lower'::regclass`).Scan(&valid))
	require.False(t, valid)
	require.NoError(t, f.s.ensureLookupIndex(context.Background()))
	require.NoError(t, db.QueryRow(`SELECT indisvalid FROM pg_index WHERE indexrelid='idx_workspace_intune_users_auth_lower'::regclass`).Scan(&valid))
	require.True(t, valid)
}

func TestIntuneRejectsInvalidTokenClaimsAndAlgorithms(t *testing.T) {
	f := setupIntune(t)
	for name, change := range map[string]func(*tokenClaims){
		"tenant":           func(c *tokenClaims) { c.Tenant = fixtureClient },
		"issuer":           func(c *tokenClaims) { c.Issuer = issuerFor(fixtureClient, "2.0") },
		"audience":         func(c *tokenClaims) { c.Audience = jwt.ClaimStrings{fixtureTenant} },
		"id token":         func(c *tokenClaims) { c.Scope = "" },
		"app token":        func(c *tokenClaims) { c.IdentityType = "app" },
		"scope":            func(c *tokenClaims) { c.Scope = "login.mattermost.evil" },
		"object id":        func(c *tokenClaims) { c.ObjectID = "" },
		"issued in future": func(c *tokenClaims) { c.IssuedAt = jwt.NewNumericDate(time.Now().Add(time.Hour)) },
		"no expiry":        func(c *tokenClaims) { c.ExpiresAt = nil },
		"not yet valid":    func(c *tokenClaims) { c.NotBefore = jwt.NewNumericDate(time.Now().Add(time.Hour)) },
	} {
		t.Run(name, func(t *testing.T) {
			claims := validClaims()
			change(&claims)
			_, appErr := f.s.Login(f.th.Context, f.token(t, claims))
			require.NotNil(t, appErr)
			require.Equal(t, http.StatusBadRequest, appErr.StatusCode)
		})
	}
	claims := validClaims()
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	_, appErr := f.s.Login(f.th.Context, f.token(t, claims))
	require.Equal(t, http.StatusUnauthorized, appErr.StatusCode)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
	token.Header["kid"] = "key-1"
	encoded, err := token.SignedString([]byte("untrusted-hmac-key"))
	require.NoError(t, err)
	_, appErr = f.s.Login(f.th.Context, encoded)
	require.NotNil(t, appErr)
	_, appErr = f.th.App.GetUserByAuth(new(fixtureUser), model.ServiceOffice365)
	require.NotNil(t, appErr, "invalid tokens never provision accounts")
}

func TestIntuneSAMLRequiresExistingOIDAndRejectsDeactivatedUsers(t *testing.T) {
	f := setupIntune(t)
	f.th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.IntuneSettings.AuthService = new(model.UserAuthServiceSaml)
		cfg.SamlSettings.Enable = new(true)
		cfg.SamlSettings.Encrypt = new(false)
		cfg.SamlSettings.IdpURL = new("https://idp.example.test/sso")
		cfg.SamlSettings.IdpDescriptorURL = new("https://idp.example.test/entity")
		cfg.SamlSettings.ServiceProviderIdentifier = new("https://workspace.example.test/entity")
		cfg.SamlSettings.AssertionConsumerServiceURL = new("https://workspace.example.test/login/sso/saml")
		cfg.SamlSettings.IdpCertificateFile = new("fixture-saml.crt")
		cfg.SamlSettings.EmailAttribute = new("mail")
		cfg.SamlSettings.UsernameAttribute = new("username")
	})
	require.Equal(t, model.UserAuthServiceSaml, *f.th.App.Config().IntuneSettings.AuthService)
	_, appErr := f.s.Login(f.th.Context, f.token(t, validClaims()))
	require.Equal(t, http.StatusPreconditionRequired, appErr.StatusCode)
	u, appErr := f.th.App.CreateUser(f.th.Context, &model.User{AuthService: model.UserAuthServiceSaml, AuthData: new(fixtureUser), Email: "saml-mobile@example.test", Username: "saml_mobile", EmailVerified: true})
	require.Nil(t, appErr)
	got, appErr := f.s.Login(f.th.Context, f.token(t, validClaims()))
	require.Nil(t, appErr)
	require.Equal(t, u.Id, got.Id)
	require.Equal(t, u.Email, got.Email)
	require.Nil(t, f.th.App.UpdateUserActive(f.th.Context, u.Id, false))
	_, appErr = f.s.Login(f.th.Context, f.token(t, validClaims()))
	require.Equal(t, http.StatusConflict, appErr.StatusCode)
}

func TestIntuneDoesNotLinkByEmailAndRespectsAccountCreationSetting(t *testing.T) {
	f := setupIntune(t)
	claims := validClaims()
	claims.Email = f.th.BasicUser.Email
	_, appErr := f.s.Login(f.th.Context, f.token(t, claims))
	require.NotNil(t, appErr)
	u, appErr := f.th.App.GetUser(f.th.Context, f.th.BasicUser.Id)
	require.Nil(t, appErr)
	require.Empty(t, u.AuthService)
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.TeamSettings.EnableUserCreation = new(false) })
	_, appErr = f.s.Login(f.th.Context, f.token(t, validClaims()))
	require.Equal(t, http.StatusForbidden, appErr.StatusCode)
}

func TestIntuneSigningKeyRotationAndMetadataBoundary(t *testing.T) {
	f := setupIntune(t)
	_, appErr := f.s.Login(f.th.Context, f.token(t, validClaims()))
	require.Nil(t, appErr)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	f.mu.Lock()
	f.private, f.kid = key, "key-2"
	f.mu.Unlock()
	f.s.mu.Lock()
	f.s.keys[fixtureTenant+":2.0:public"].attempted = time.Now().Add(-6 * time.Minute)
	f.s.mu.Unlock()
	_, appErr = f.s.Login(f.th.Context, f.token(t, validClaims()))
	require.Nil(t, appErr)
	require.Equal(t, int32(4), f.requests.Load())
	// A discovery URL is pinned; even trusted metadata cannot redirect key
	// retrieval to an arbitrary server or the local network.
	f.mu.Lock()
	f.keysURL = "https://attacker.example.test/keys"
	f.mu.Unlock()
	f.s.mu.Lock()
	f.s.keys = make(map[string]*keyCache)
	f.s.mu.Unlock()
	_, appErr = f.s.Login(f.th.Context, f.token(t, validClaims()))
	require.NotNil(t, appErr)
	require.Equal(t, int32(5), f.requests.Load())
}

func TestIntuneV1TokenAndExpiredKeyCache(t *testing.T) {
	f := setupIntune(t)
	claims := validClaims()
	claims.Version, claims.Issuer, claims.Audience = "1.0", issuerFor(fixtureTenant, "1.0"), jwt.ClaimStrings{"api://" + fixtureClient}
	_, appErr := f.s.Login(f.th.Context, f.token(t, claims))
	require.Nil(t, appErr)
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	f.s.mu.Lock()
	entry := f.s.keys[fixtureTenant+":1.0:public"]
	entry.refreshed, entry.attempted = time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)
	f.s.mu.Unlock()
	_, appErr = f.s.Login(f.th.Context, f.token(t, claims))
	require.Nil(t, appErr, "recent trusted keys survive a temporary metadata outage")
	f.s.mu.Lock()
	entry.refreshed = time.Now().Add(-25 * time.Hour)
	f.s.mu.Unlock()
	_, appErr = f.s.Login(f.th.Context, f.token(t, claims))
	require.NotNil(t, appErr, "keys cannot be trusted indefinitely during an outage")
}

func TestIntuneAudienceMatchesConfiguredClientIDCase(t *testing.T) {
	f := setupIntune(t)
	uppercaseClientID := strings.ToUpper(fixtureClient)
	f.th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.IntuneSettings.ClientId = new(uppercaseClientID)
	})

	for _, version := range []string{"1.0", "2.0"} {
		t.Run(version, func(t *testing.T) {
			claims := validClaims()
			claims.Version = version
			claims.Issuer = issuerFor(fixtureTenant, version)
			claims.Audience = jwt.ClaimStrings{uppercaseClientID}
			if version == "1.0" {
				claims.Audience = jwt.ClaimStrings{"api://" + uppercaseClientID}
			}
			_, appErr := f.s.Login(f.th.Context, f.token(t, claims))
			require.Nil(t, appErr)
			claims.Audience = jwt.ClaimStrings{fixtureClient}
			if version == "1.0" {
				claims.Audience = jwt.ClaimStrings{"api://" + fixtureClient}
			}
			_, appErr = f.s.Login(f.th.Context, f.token(t, claims))
			require.Nil(t, appErr)

			claims.Audience = jwt.ClaimStrings{fixtureTenant}
			_, appErr = f.s.Login(f.th.Context, f.token(t, claims))
			require.NotNil(t, appErr)
			require.Equal(t, http.StatusBadRequest, appErr.StatusCode)
		})
	}
}

func TestIntuneKeyHostIsPinned(t *testing.T) {
	require.True(t, trustedEndpoint("https://login.microsoftonline.com/keys"))
	require.False(t, trustedEndpoint("https://login.microsoftonline.com.evil.test/keys"))
	require.False(t, trustedEndpoint("https://login.microsoftonline.us/keys"))
}

func TestIntuneKnownKeyDoesNotWaitForStalledRefresh(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRefresh := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRefresh)
	s := &Service{
		keys: map[string]*keyCache{
			fixtureTenant + ":2.0:public": {
				keys:      map[string]*rsa.PublicKey{"known": &private.PublicKey},
				refreshed: time.Now().Add(-2 * time.Hour),
			},
		},
		client: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			close(started)
			<-release
			return nil, errors.New("key service unavailable")
		})},
	}
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		_, _ = s.signingKey(context.Background(), fixtureTenant, "2.0", "unknown")
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("key refresh did not start")
	}
	knownDone := make(chan error, 1)
	go func() {
		key, err := s.signingKey(context.Background(), fixtureTenant, "2.0", "known")
		if err == nil && key != &private.PublicKey {
			err = errors.New("wrong cached signing key")
		}
		knownDone <- err
	}()
	select {
	case err := <-knownDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("known signing key was blocked by a stalled refresh")
	}
	releaseRefresh()
	select {
	case <-refreshDone:
	case <-time.After(time.Second):
		t.Fatal("stalled refresh did not terminate")
	}
}

func TestIntuneConcurrentProvisioningAcrossServices(t *testing.T) {
	f := setupIntune(t)
	encoded := f.token(t, validClaims())
	type result struct {
		user *model.User
		err  *model.AppError
	}
	results := make(chan result, 4)
	var wg sync.WaitGroup
	for range 4 {
		s := New(f.th.App).(*Service)
		s.client = f.s.client
		wg.Go(func() { u, err := s.Login(f.th.Context, encoded); results <- result{u, err} })
	}
	wg.Wait()
	close(results)
	ids := map[string]bool{}
	for got := range results {
		require.Nil(t, got.err)
		ids[got.user.Id] = true
	}
	require.Len(t, ids, 1, "only one account is provisioned across independent service instances")
}
