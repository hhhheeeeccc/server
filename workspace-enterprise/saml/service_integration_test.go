package saml

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/mattermost/enterprise/account_migration"
	"github.com/mattermost/enterprise/localization"
	saml2 "github.com/mattermost/gosaml2"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/app/platform"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// Match main.go: install the primary session adapter before Platform.Start
	// launches WebSocket hub readers, rather than during Channels creation.
	app.RegisterAutoTranslationInterface(func(server *app.Server) einterfaces.AutoTranslationInterface {
		account_migration.RegisterPrimarySessionLookupsBeforeStart(server)
		return nil
	})
	// Match Enterprise startup: config validation must recognize the embedded
	// Arabic catalog before a fixture selects Arabic as the client default.
	if err := localization.RegisterArabicLocale(); err != nil {
		panic(err)
	}
	app.RegisterSamlInterface(New)
	app.RegisterAccountMigrationInterface(account_migration.New)
	app.RegisterComplianceInterface(func(a *app.App) einterfaces.ComplianceInterface {
		account_migration.RegisterAPI(a)
		return nil
	})
	platform.RegisterSamlDiagnosticInterface(NewDiagnostic)
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

type fixture struct {
	th              *api4.TestHelper
	key             tls.Certificate
	certPEM, keyPEM []byte
}

type testLDAPResolver struct{ einterfaces.LdapInterface }

func (*testLDAPResolver) ResolveSAMLUser(_ request.CTX, id, _ string, useID bool) (*model.User, *model.AppError) {
	if !useID || id != "immutable-1" {
		return nil, model.NewAppError("testLDAPResolver", "ent.ldap.invalid_mapping.app_error", nil, "", http.StatusUnauthorized)
	}
	return &model.User{AuthData: new("immutable-1"), Email: "ldap@example.test", Username: "ldap_user", FirstName: "LDAP"}, nil
}

func certificate(t *testing.T) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture-idp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	return pair, certPEM, keyPEM
}

func setupSAML(t *testing.T) *fixture {
	t.Helper()
	pair, cert, key := certificate(t)
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	require.NoError(t, th.Server.Platform().SetConfigFile("test-idp.pem", cert))
	require.NoError(t, th.Server.Platform().SetConfigFile("test-sp.pem", cert))
	require.NoError(t, th.Server.Platform().SetConfigFile("test-sp.key", key))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.SamlSettings.Enable = new(true)
		cfg.SamlSettings.Encrypt = new(false)
		cfg.SamlSettings.Verify = new(true)
		cfg.SamlSettings.IdpURL = new("https://idp.example.test/sso")
		cfg.SamlSettings.IdpDescriptorURL = new("https://idp.example.test/entity")
		cfg.SamlSettings.ServiceProviderIdentifier = new("https://workspace.example.test/entity")
		cfg.SamlSettings.AssertionConsumerServiceURL = new(th.Client.URL + "/login/sso/saml")
		cfg.SamlSettings.IdpCertificateFile = new("test-idp.pem")
		cfg.SamlSettings.PublicCertificateFile = new("test-sp.pem")
		cfg.SamlSettings.PrivateKeyFile = new("test-sp.key")
		cfg.SamlSettings.EmailAttribute = new("mail")
		cfg.SamlSettings.UsernameAttribute = new("username")
		cfg.SamlSettings.IdAttribute = new("uid")
		cfg.SamlSettings.FirstNameAttribute = new("givenName")
	})
	return &fixture{th: th, key: pair, certPEM: cert, keyPEM: key}
}

func TestConfigureSPWithDisabledSAML(t *testing.T) {
	f := setupSAML(t)
	f.th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.SamlSettings.Enable = new(false)
		cfg.SamlSettings.Verify = new(false)
	})
	require.False(t, *f.th.App.Config().SamlSettings.Enable)
	require.NoError(t, f.th.App.Saml().ConfigureSP(f.th.Context))

	f.th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.SamlSettings.Enable = new(true)
	})
	require.True(t, *f.th.App.Config().SamlSettings.Enable)
	require.Error(t, f.th.App.Saml().ConfigureSP(f.th.Context))
}

func samlElement(parent *etree.Element, name, value string) *etree.Element {
	e := parent.CreateElement("saml:" + name)
	e.SetText(value)
	return e
}

func (f *fixture) response(t *testing.T, requestID string, values map[string]string, mutate func(*etree.Element), sign bool) string {
	t.Helper()
	cfg := f.th.App.Config().SamlSettings
	now := time.Now().UTC()
	expires := now.Add(5 * time.Minute).Format(time.RFC3339)
	r := etree.NewElement("samlp:Response")
	r.CreateAttr("xmlns:samlp", "urn:oasis:names:tc:SAML:2.0:protocol")
	r.CreateAttr("xmlns:saml", "urn:oasis:names:tc:SAML:2.0:assertion")
	r.CreateAttr("ID", "_"+model.NewId())
	r.CreateAttr("Version", "2.0")
	r.CreateAttr("IssueInstant", now.Format(time.RFC3339))
	r.CreateAttr("Destination", *cfg.AssertionConsumerServiceURL)
	r.CreateAttr("InResponseTo", requestID)
	samlElement(r, "Issuer", *cfg.IdpDescriptorURL)
	r.CreateElement("samlp:Status").CreateElement("samlp:StatusCode").CreateAttr("Value", saml2.StatusCodeSuccess)
	a := etree.NewElement("saml:Assertion")
	a.CreateAttr("xmlns:saml", "urn:oasis:names:tc:SAML:2.0:assertion")
	a.CreateAttr("ID", "_"+model.NewId())
	a.CreateAttr("Version", "2.0")
	a.CreateAttr("IssueInstant", now.Format(time.RFC3339))
	samlElement(a, "Issuer", *cfg.IdpDescriptorURL)
	subject := a.CreateElement("saml:Subject")
	samlElement(subject, "NameID", "opaque-name-id")
	confirmation := subject.CreateElement("saml:SubjectConfirmation")
	confirmation.CreateAttr("Method", saml2.SubjMethodBearer)
	data := confirmation.CreateElement("saml:SubjectConfirmationData")
	data.CreateAttr("Recipient", *cfg.AssertionConsumerServiceURL)
	data.CreateAttr("NotOnOrAfter", expires)
	data.CreateAttr("InResponseTo", requestID)
	conditions := a.CreateElement("saml:Conditions")
	conditions.CreateAttr("NotBefore", now.Add(-time.Minute).Format(time.RFC3339))
	conditions.CreateAttr("NotOnOrAfter", expires)
	samlElement(conditions.CreateElement("saml:AudienceRestriction"), "Audience", *cfg.ServiceProviderIdentifier)
	authn := a.CreateElement("saml:AuthnStatement")
	authn.CreateAttr("AuthnInstant", now.Format(time.RFC3339))
	authn.CreateAttr("SessionIndex", "session-"+model.NewId())
	attrs := a.CreateElement("saml:AttributeStatement")
	if values == nil {
		values = map[string]string{"uid": "immutable-1", "mail": "saml@example.test", "username": "saml_user", "givenName": "First"}
	}
	for name, value := range values {
		attr := attrs.CreateElement("saml:Attribute")
		attr.CreateAttr("Name", name)
		samlElement(attr, "AttributeValue", value)
	}
	if mutate != nil {
		mutate(a)
	}
	if sign {
		ctx := dsig.NewDefaultSigningContext(dsig.TLSCertKeyStore(f.key))
		ctx.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
		var err error
		a, err = ctx.SignEnveloped(a)
		require.NoError(t, err)
	}
	r.AddChild(a)
	doc := etree.NewDocument()
	doc.SetRoot(r)
	raw, err := doc.WriteToBytes()
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(raw)
}

func (f *fixture) request(t *testing.T, props map[string]string) (string, string) {
	t.Helper()
	relay := model.SignSamlRelayState(f.th.App.SamlRelayStateSigningKey(), props)
	req, appErr := f.th.App.Saml().BuildRequest(f.th.Context, relay)
	require.Nil(t, appErr)
	raw, err := base64.StdEncoding.DecodeString(req.Base64AuthRequest)
	require.NoError(t, err)
	doc := etree.NewDocument()
	require.NoError(t, doc.ReadFromBytes(raw))
	return doc.Root().SelectAttrValue("ID", ""), relay
}

func TestSAMLStableIdentityAndReplayAcrossServiceInstances(t *testing.T) {
	f := setupSAML(t)
	s := f.th.App.Saml()
	encoded := f.response(t, "", nil, nil, true)
	u, info, _, appErr := s.DoLogin(f.th.Context, encoded, nil)
	require.Nil(t, appErr)
	require.True(t, info.Assertions[0].SignatureValidated)
	require.Equal(t, "immutable-1", *u.AuthData)
	require.Equal(t, "First", u.FirstName)
	id := u.Id
	_, _, _, appErr = New(f.th.App).DoLogin(f.th.Context, encoded, nil)
	require.NotNil(t, appErr, "another service instance must reject an already-used assertion")
	encoded = f.response(t, "", map[string]string{"uid": "immutable-1", "mail": "new@example.test", "username": "renamed", "givenName": "Updated"}, nil, true)
	u, _, _, appErr = s.DoLogin(f.th.Context, encoded, nil)
	require.Nil(t, appErr)
	require.Equal(t, id, u.Id)
	require.Equal(t, "new@example.test", u.Email)
	require.Equal(t, "renamed", u.Username)
	require.Equal(t, "Updated", u.FirstName)
	require.Equal(t, "email", s.CheckProviderAttributes(f.th.Context, &f.th.App.Config().SamlSettings, u, &model.UserPatch{Email: new("changed@example.test")}))
	metadata, appErr := s.GetMetadata(f.th.Context)
	require.Nil(t, appErr)
	require.Contains(t, metadata, *f.th.App.Config().SamlSettings.AssertionConsumerServiceURL)
	f.th.Server.SetLicense(nil)
	_, _, _, appErr = s.DoLogin(f.th.Context, f.response(t, "", nil, nil, true), nil)
	require.Equal(t, http.StatusForbidden, appErr.StatusCode)
}

func TestSAMLLoginKeepsLDAPOwnedProfileWhenSyncEnabled(t *testing.T) {
	f := setupSAML(t)
	u, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", nil, nil, true), nil)
	require.Nil(t, appErr)
	changed := u.DeepCopy()
	changed.FirstName = "From LDAP"
	_, err := f.th.Store.User().Update(f.th.Context, changed, true)
	require.NoError(t, err)
	f.th.App.InvalidateCacheForUser(u.Id)
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.EnableSyncWithLdap = new(true) })
	u, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", map[string]string{
		"uid": "immutable-1", "mail": "saml@example.test", "username": "saml_user", "givenName": "From SAML Again",
	}, nil, true), nil)
	require.Nil(t, appErr)
	require.Equal(t, "From LDAP", u.FirstName)
}

func TestSAMLLoginOverridesBindingWithResolvedLDAPIdentity(t *testing.T) {
	f := setupSAML(t)
	f.th.Server.Channels().Ldap = &testLDAPResolver{}
	f.th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.SamlSettings.EnableSyncWithLdap = new(true)
		cfg.SamlSettings.EnableSyncWithLdapIncludeAuth = new(true)
	})
	u, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", nil, nil, true), nil)
	require.Nil(t, appErr)
	require.Equal(t, "immutable-1", *u.AuthData)
	require.Equal(t, "ldap@example.test", u.Email)
	require.Equal(t, "LDAP", u.FirstName)
}

func TestSAMLRejectsInvalidAssertionsBeforeProvisioning(t *testing.T) {
	f := setupSAML(t)
	for name, mutate := range map[string]func(*etree.Element){
		"audience": func(a *etree.Element) {
			a.FindElement("./Conditions/AudienceRestriction/Audience").SetText("wrong-audience")
		},
		"missing audience": func(a *etree.Element) {
			c := a.FindElement("./Conditions")
			c.RemoveChild(c.FindElement("./AudienceRestriction"))
		},
		"expired": func(a *etree.Element) {
			a.FindElement("./Conditions").CreateAttr("NotOnOrAfter", time.Now().Add(-time.Minute).Format(time.RFC3339))
		},
		"future": func(a *etree.Element) {
			a.FindElement("./Conditions").CreateAttr("NotBefore", time.Now().Add(time.Hour).Format(time.RFC3339))
		},
		"issuer": func(a *etree.Element) { a.FindElement("./Issuer").SetText("wrong-issuer") },
		"recipient": func(a *etree.Element) {
			a.FindElement("./Subject/SubjectConfirmation/SubjectConfirmationData").CreateAttr("Recipient", "https://other.test")
		},
		"confirmation expired": func(a *etree.Element) {
			a.FindElement("./Subject/SubjectConfirmation/SubjectConfirmationData").CreateAttr("NotOnOrAfter", time.Now().Add(-time.Minute).Format(time.RFC3339))
		},
		"duplicate attribute": func(a *etree.Element) {
			attrs := a.FindElement("./AttributeStatement")
			attrs.AddChild(attrs.ChildElements()[0].Copy())
		},
		"missing ID": func(a *etree.Element) { a.RemoveAttr("ID") },
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", nil, mutate, true), nil)
			require.NotNil(t, appErr)
		})
	}
	_, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", nil, nil, false), nil)
	require.NotNil(t, appErr, "unsigned assertion")
	_, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context.WithAcceptLanguage("ar"), "invalid", nil)
	require.NotNil(t, appErr)
	require.Contains(t, appErr.Message, "استجابة SAML")
	require.True(t, appErr.SkipTranslation, "the web redirect must retain the already-localized message")
	encoded := f.response(t, "", nil, nil, true)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	raw = bytes.ReplaceAll(raw, []byte("saml@example.test"), []byte("attacker@example.test"))
	_, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context, base64.StdEncoding.EncodeToString(raw), nil)
	require.NotNil(t, appErr, "tampered signature")
	other, _, _ := certificate(t)
	f.key = other
	_, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", nil, nil, true), nil)
	require.NotNil(t, appErr, "untrusted signing certificate")
	_, appErr = f.th.App.GetUserByAuth(new("immutable-1"), model.UserAuthServiceSaml)
	require.NotNil(t, appErr, "invalid assertions must not provision an account")
}

func TestSAMLRequestCorrelationAndLocalAccountConversion(t *testing.T) {
	f := setupSAML(t)
	local := f.th.BasicUser
	values := map[string]string{"uid": "immutable-1", "mail": local.Email, "username": local.Username}
	_, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", values, nil, true), nil)
	require.NotNil(t, appErr, "matching email never authorizes account linking")
	token, appErr := f.th.App.CreateSamlRelayToken(model.TokenTypeSaml, local.Email)
	require.Nil(t, appErr)
	props := map[string]string{"action": model.OAuthActionEmailToSSO, "email_token": token.Token}
	requestID, _ := f.request(t, props)
	encoded := f.response(t, requestID, values, nil, true)
	_, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context, encoded, map[string]string{})
	require.NotNil(t, appErr, "relay properties cannot be substituted")
	u, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, encoded, props)
	require.Nil(t, appErr)
	require.Equal(t, local.Id, u.Id)
	require.Equal(t, model.UserAuthServiceSaml, u.AuthService)
	require.Empty(t, u.Password)
	_, err := f.th.Store.Token().GetByToken(token.Token)
	require.Error(t, err)
	_, _, err = f.th.Client.GetMe(f.th.Context.Context(), "")
	require.Error(t, err, "conversion revokes existing sessions")
	_, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", values, nil, true), props)
	require.NotNil(t, appErr, "IdP-initiated responses cannot perform account conversion")
	_, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "_unknown-request", values, nil, true), nil)
	require.NotNil(t, appErr)
}

func TestSAMLHTTPRedirectCallbackAndSession(t *testing.T) {
	f := setupSAML(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(f.th.Client.URL + "/login/sso/saml")
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, http.StatusFound, response.StatusCode)
	location, err := url.Parse(response.Header.Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "idp.example.test", location.Host)
	raw, err := base64.StdEncoding.DecodeString(location.Query().Get("SAMLRequest"))
	require.NoError(t, err)
	reader := flate.NewReader(bytes.NewReader(raw))
	requestXML, err := io.ReadAll(reader)
	reader.Close()
	require.NoError(t, err)
	doc := etree.NewDocument()
	require.NoError(t, doc.ReadFromBytes(requestXML))
	requestID := doc.Root().SelectAttrValue("ID", "")
	values := url.Values{"SAMLResponse": {f.response(t, requestID, nil, nil, true)}, "RelayState": {location.Query().Get("RelayState")}}
	response, err = client.PostForm(f.th.Client.URL+"/login/sso/saml", values)
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, http.StatusFound, response.StatusCode)
	require.NotContains(t, response.Header.Get("Location"), "/error")
	var authCookie string
	for _, cookie := range response.Cookies() {
		if cookie.Name == model.SessionCookieToken {
			authCookie = cookie.Value
		}
	}
	require.NotEmpty(t, authCookie)
	api := f.th.CreateClient()
	api.SetToken(authCookie)
	u, _, err := api.GetMe(f.th.Context.Context(), "")
	require.NoError(t, err)
	require.Equal(t, "saml_user", u.Username)
	response, err = client.PostForm(f.th.Client.URL+"/login/sso/saml", values)
	require.NoError(t, err)
	response.Body.Close()
	require.True(t, strings.Contains(response.Header.Get("Location"), "/error"))
	for _, cookie := range response.Cookies() {
		if cookie.Name == model.SessionCookieToken {
			require.Empty(t, cookie.Value)
		}
	}
}
