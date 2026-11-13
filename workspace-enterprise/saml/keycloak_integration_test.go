package saml

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/mattermost/gosaml2/types"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

// Opt-in: the supplied administrator must belong to a disposable local Keycloak.
// The runner in deployment/saml owns that container and a separate PostgreSQL.
func TestKeycloakSAML(t *testing.T) {
	admin := keycloakTestAdmin(t)
	origin := admin.origin

	for _, mode := range []struct {
		name    string
		sign    bool
		encrypt bool
		browser bool
	}{
		{name: "signed_response_and_assertion"},
		{name: "signed_authentication_request", sign: true},
		{name: "encrypted_assertion_and_signed_request", sign: true, encrypt: true},
		{name: "encrypted_browser_tls_proxy", sign: true, encrypt: true, browser: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			if mode.browser && os.Getenv("WORKSPACE_TEST_CHROME_EXECUTABLE") == "" {
				t.Skip("use --browser with the isolated runner to exercise Chromium")
			}
			f := setupSAML(t)
			realm := "workspace-saml-" + model.NewId()
			username := "keycloak" + model.NewId()
			userID := model.NewId()
			password := model.NewId() + "-Test1!"
			appOrigin := f.th.Client.URL
			if mode.browser {
				appOrigin = samlBrowserOrigin(t, f)
			}
			configureKeycloakSAML(t, f, admin, realm, appOrigin, keycloakSAMLUser{
				id: userID, username: username, email: username + "@example.test", password: password,
			}, mode.sign, mode.encrypt)
			acs := appOrigin + "/login/sso/saml"

			if mode.browser {
				verifySAMLBrowser(t, f, appOrigin, origin, username, password, userID)
				return
			}
			browser, values := keycloakResponse(t, f.th.Client.URL, origin, username, password, true)
			raw, err := base64.StdEncoding.DecodeString(values.Get("SAMLResponse"))
			require.NoError(t, err)
			require.Equal(t, mode.encrypt, bytes.Contains(raw, []byte("EncryptedAssertion")), "the IdP must actually exercise the configured encryption mode")
			sp, err := configure(f.th.App.Config().SamlSettings, f.th.App.GetConfigFile)
			require.NoError(t, err)
			info, err := validatedAssertion(sp, values.Get("SAMLResponse"))
			require.NoError(t, err, "the real provider response must validate before the callback")
			require.True(t, info.ResponseSignatureValidated)
			require.Len(t, info.Assertions, 1)
			require.True(t, info.Assertions[0].SignatureValidated)
			if mode.encrypt {
				var envelope struct {
					Assertions []types.EncryptedAssertion `xml:"EncryptedAssertion"`
				}
				require.NoError(t, xml.Unmarshal(raw, &envelope))
				require.Len(t, envelope.Assertions, 1)
				key := envelope.Assertions[0].EncryptedKey
				if key.CipherValue == "" {
					key = envelope.Assertions[0].DetEncryptedKey
				}
				require.NotNil(t, key.EncryptionMethod.DigestMethod)
				require.Equal(t, "http://www.w3.org/2001/04/xmlenc#sha256", key.EncryptionMethod.DigestMethod.Algorithm)
			}
			user := finishKeycloakLogin(t, f, browser, values)
			require.Equal(t, username, user.Username)
			require.Equal(t, userID, *user.AuthData)
			require.Equal(t, "Before", user.FirstName)

			// Replay from a separate browser must not obtain a new session.
			assertKeycloakCallbackRejected(t, noRedirectClient(), acs, values)

			// The provider's stable ID, rather than email, owns the local account.
			admin.request(t, http.MethodPut, "/admin/realms/"+realm+"/users/"+userID, map[string]any{
				"email": username + "-changed@example.test", "emailVerified": true, "firstName": "After",
			}, http.StatusNoContent)
			browser, values = keycloakResponse(t, f.th.Client.URL, origin, username, password, false)
			updated := finishKeycloakLogin(t, f, browser, values)
			require.Equal(t, user.Id, updated.Id)
			require.Equal(t, username+"-changed@example.test", updated.Email)
			require.Equal(t, "After", updated.FirstName)

			browser, values = keycloakResponse(t, f.th.Client.URL, origin, username, password, false)
			f.th.Server.SetLicense(nil)
			assertKeycloakCallbackRejected(t, browser, acs, values)
			require.True(t, f.th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
			require.Nil(t, f.th.App.UpdateUserActive(f.th.Context, user.Id, false))
			browser, values = keycloakResponse(t, f.th.Client.URL, origin, username, password, false)
			assertKeycloakCallbackRejected(t, browser, acs, values)
			stored, appErr := f.th.App.GetUser(f.th.Context, user.Id)
			require.Nil(t, appErr)
			require.Positive(t, stored.DeleteAt, "a valid provider login must not reactivate a locally disabled account")
		})
	}
}

type keycloakAdmin struct {
	origin string
	token  string
	client *http.Client
}

func noRedirectClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (k *keycloakAdmin) authenticate(t *testing.T) {
	t.Helper()
	username, password := os.Getenv("WORKSPACE_TEST_KEYCLOAK_ADMIN"), os.Getenv("WORKSPACE_TEST_KEYCLOAK_PASSWORD")
	require.NotEmpty(t, username)
	require.NotEmpty(t, password)
	response, err := k.client.PostForm(k.origin+"/realms/master/protocol/openid-connect/token", url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {username}, "password": {password},
	})
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode, "Keycloak administrator authentication")
	var token struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token))
	require.NotEmpty(t, token.AccessToken)
	k.token = token.AccessToken
}

func (k *keycloakAdmin) request(t *testing.T, method, path string, body any, status int) {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(method, k.origin+path, bytes.NewReader(encoded))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+k.token)
	response, err := k.client.Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, status, response.StatusCode, "Keycloak admin operation %s", method)
}

func (k *keycloakAdmin) signingCertificate(t *testing.T, issuer string) []byte {
	t.Helper()
	response, err := k.client.Get(issuer + "/protocol/saml/descriptor")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var metadata struct {
		Keys []struct {
			Use         string `xml:"use,attr"`
			Certificate string `xml:"KeyInfo>X509Data>X509Certificate"`
		} `xml:"IDPSSODescriptor>KeyDescriptor"`
	}
	require.NoError(t, xml.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&metadata))
	for _, key := range metadata.Keys {
		if key.Use == "signing" {
			der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(key.Certificate), ""))
			require.NoError(t, err)
			return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		}
	}
	t.Fatal("Keycloak metadata has no signing certificate")
	return nil
}

func keycloakResponse(t *testing.T, appOrigin, idpOrigin, username, password string, checkWrongPassword bool) (*http.Client, url.Values) {
	t.Helper()
	browser := noRedirectClient()
	response, err := browser.Get(appOrigin + "/login/sso/saml")
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, http.StatusFound, response.StatusCode)
	authURL, err := url.Parse(response.Header.Get("Location"))
	require.NoError(t, err)
	require.Equal(t, idpOrigin, authURL.Scheme+"://"+authURL.Host, "authentication must stay on the isolated IdP")
	response, err = browser.Get(authURL.String())
	require.NoError(t, err)
	doc := readKeycloakHTML(t, response)
	if checkWrongPassword {
		doc = postKeycloakCredentials(t, browser, doc, idpOrigin, username, "incorrect-test-password")
		require.Zero(t, doc.Find("input[name='SAMLResponse']").Length(), "incorrect credentials must not produce an assertion")
		require.Equal(t, 1, doc.Find("form#kc-form-login").Length())
	}
	doc = postKeycloakCredentials(t, browser, doc, idpOrigin, username, password)
	values := url.Values{}
	form := doc.Find("form").First()
	action, ok := form.Attr("action")
	require.True(t, ok)
	require.Equal(t, appOrigin+"/login/sso/saml", action, "the provider must post back to the exact test callback")
	form.Find("input[name]").Each(func(_ int, input *goquery.Selection) {
		name, _ := input.Attr("name")
		value, _ := input.Attr("value")
		values.Set(name, value)
	})
	require.NotEmpty(t, values.Get("SAMLResponse"))
	require.NotEmpty(t, values.Get("RelayState"))
	return browser, values
}

func readKeycloakHTML(t *testing.T, response *http.Response) *goquery.Document {
	t.Helper()
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode, "Keycloak form response")
	doc, err := goquery.NewDocumentFromReader(io.LimitReader(response.Body, 2<<20))
	require.NoError(t, err)
	return doc
}

func postKeycloakCredentials(t *testing.T, browser *http.Client, doc *goquery.Document, idpOrigin, username, password string) *goquery.Document {
	t.Helper()
	form := doc.Find("form#kc-form-login")
	action, ok := form.Attr("action")
	require.True(t, ok, "the provider must present its login form")
	parsed, err := url.Parse(action)
	require.NoError(t, err)
	require.Equal(t, idpOrigin, parsed.Scheme+"://"+parsed.Host, "credentials must only be sent to the isolated IdP")
	response, err := browser.PostForm(action, url.Values{"username": {username}, "password": {password}, "credentialId": {""}})
	require.NoError(t, err)
	return readKeycloakHTML(t, response)
}

func finishKeycloakLogin(t *testing.T, f *fixture, browser *http.Client, values url.Values) *model.User {
	t.Helper()
	response, err := browser.PostForm(f.th.Client.URL+"/login/sso/saml", values)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusFound, response.StatusCode)
	require.NotContains(t, response.Header.Get("Location"), "/error")
	token := ""
	for _, cookie := range response.Cookies() {
		if cookie.Name == model.SessionCookieToken {
			token = cookie.Value
		}
	}
	require.NotEmpty(t, token, "the core callback must issue a real session")
	api := f.th.CreateClient()
	api.SetToken(token)
	user, _, err := api.GetMe(f.th.Context.Context(), "")
	require.NoError(t, err)
	return user
}

func assertKeycloakCallbackRejected(t *testing.T, browser *http.Client, acs string, values url.Values) {
	t.Helper()
	response, err := browser.PostForm(acs, values)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusFound, response.StatusCode)
	require.Contains(t, response.Header.Get("Location"), "/error")
	for _, cookie := range response.Cookies() {
		if cookie.Name == model.SessionCookieToken {
			require.Empty(t, cookie.Value, "a rejected callback must not create a session")
		}
	}
}
