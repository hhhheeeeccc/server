package saml

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/mattermost/gosaml2/types"
	"github.com/mattermost/mattermost/server/public/model"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/stretchr/testify/require"
)

func encryptedResponse(t *testing.T, encoded string, key *rsa.PrivateKey, corrupt bool) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	doc := etree.NewDocument()
	require.NoError(t, doc.ReadFromBytes(raw))
	a := doc.Root().FindElement("./Assertion")
	assertionDoc := etree.NewDocument()
	assertionDoc.SetRoot(a.Copy())
	plain, err := assertionDoc.WriteToBytes()
	require.NoError(t, err)
	symmetric := make([]byte, 32)
	_, err = rand.Read(symmetric)
	require.NoError(t, err)
	block, err := aes.NewCipher(symmetric)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce := make([]byte, gcm.NonceSize())
	_, err = rand.Read(nonce)
	require.NoError(t, err)
	ciphertext := gcm.Seal(nonce, nonce, plain, nil)
	if corrupt {
		ciphertext = []byte{1}
	}
	wrapped, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &key.PublicKey, symmetric, nil)
	require.NoError(t, err)
	e := etree.NewElement("saml:EncryptedAssertion")
	e.CreateAttr("xmlns:xenc", "http://www.w3.org/2001/04/xmlenc#")
	e.CreateAttr("xmlns:ds", "http://www.w3.org/2000/09/xmldsig#")
	ed := e.CreateElement("xenc:EncryptedData")
	ed.CreateAttr("Type", "http://www.w3.org/2001/04/xmlenc#Element")
	ed.CreateElement("xenc:EncryptionMethod").CreateAttr("Algorithm", types.MethodAES256GCM)
	ek := ed.CreateElement("ds:KeyInfo").CreateElement("xenc:EncryptedKey")
	ek.CreateElement("xenc:EncryptionMethod").CreateAttr("Algorithm", types.MethodRSAOAEP)
	ek.CreateElement("xenc:CipherData").CreateElement("xenc:CipherValue").SetText(base64.StdEncoding.EncodeToString(wrapped))
	ed.CreateElement("xenc:CipherData").CreateElement("xenc:CipherValue").SetText(base64.StdEncoding.EncodeToString(ciphertext))
	doc.Root().RemoveChild(a)
	doc.Root().AddChild(e)
	raw, err = doc.WriteToBytes()
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(raw)
}

func TestSAMLEncryptionAndSignedRequests(t *testing.T) {
	f := setupSAML(t)
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.SignRequest = new(true) })
	req, appErr := f.th.App.Saml().BuildRequest(f.th.Context, "")
	require.Nil(t, appErr)
	u, err := url.Parse(req.URL)
	require.NoError(t, err)
	require.Equal(t, dsig.RSASHA256SignatureMethod, u.Query().Get("SigAlg"))
	require.NotEmpty(t, u.Query().Get("Signature"))
	// Having a signing key must not accidentally require encryption as well.
	_, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", nil, nil, true), nil)
	require.Nil(t, appErr)
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.Encrypt = new(true) })
	_, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", nil, nil, true), nil)
	require.NotNil(t, appErr, "plaintext is rejected when encryption is required")
	encoded := encryptedResponse(t, f.response(t, "", nil, nil, true), f.key.PrivateKey.(*rsa.PrivateKey), false)
	user, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, encoded, nil)
	require.Nil(t, appErr)
	require.Equal(t, "saml_user", user.Username)
	malformed := encryptedResponse(t, f.response(t, "", nil, nil, true), f.key.PrivateKey.(*rsa.PrivateKey), true)
	require.NotPanics(t, func() {
		_, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context, malformed, nil)
		require.NotNil(t, appErr)
	})
	metadata, appErr := f.th.App.Saml().GetMetadata(f.th.Context)
	require.Nil(t, appErr)
	require.Contains(t, metadata, `use="encryption"`)
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.Verify = new(false) })
	require.Error(t, f.th.App.Saml().ConfigureSP(f.th.Context), "unsigned identity is not an allowed fallback")
}

func TestSAMLGuestAndAdminAttributes(t *testing.T) {
	f := setupSAML(t)
	f.th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.GuestAccountsSettings.Enable = new(true)
		cfg.SamlSettings.EnableAdminAttribute = new(true)
		cfg.SamlSettings.AdminAttribute = new("admin=true")
		cfg.SamlSettings.GuestAttribute = new("guest=true")
	})
	require.True(t, *f.th.App.Config().SamlSettings.EnableAdminAttribute)
	values := map[string]string{"uid": "roles-1", "username": "roles_user", "mail": "roles@example.test", "admin": "true"}
	login := func() *model.User {
		u, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", values, nil, true), nil)
		require.Nil(t, appErr)
		return u
	}
	u := login()
	require.True(t, u.IsSystemAdmin())
	values["admin"] = "false"
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.EnableAdminAttribute = new(false) })
	require.True(t, login().IsSystemAdmin(), "disabling admin mapping retains manually managed roles")
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.EnableAdminAttribute = new(true) })
	require.False(t, login().IsSystemAdmin(), "nonmatching field=value filter demotes")
	delete(values, "admin")
	require.False(t, login().IsSystemAdmin(), "removing the admin attribute demotes")
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.AdminAttribute = new("groups=admins") })
	values["groups"] = "members"
	u, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", values, func(a *etree.Element) {
		for _, attr := range a.FindElement("./AttributeStatement").ChildElements() {
			if attr.SelectAttrValue("Name", "") == "groups" {
				samlElement(attr, "AttributeValue", "admins")
			}
		}
	}, true), nil)
	require.Nil(t, appErr)
	require.True(t, u.IsSystemAdmin(), "multi-valued group claim can match a role filter")
	delete(values, "groups")
	values["guest"] = "true"
	u = login()
	require.True(t, u.IsGuest())
	values["guest"] = "false"
	require.True(t, login().IsGuest(), "SAML must not automatically promote guests")
	appErr = f.th.App.UpdateUserActive(f.th.Context, u.Id, false)
	require.Nil(t, appErr)
	_, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", values, nil, true), nil)
	require.NotNil(t, appErr)
	stored, appErr := f.th.App.GetUser(f.th.Context, u.Id)
	require.Nil(t, appErr)
	require.Positive(t, stored.DeleteAt)
}

func TestSAMLRequestsExpireAndConfigurationChangesInvalidate(t *testing.T) {
	f := setupSAML(t)
	requestID, _ := f.request(t, nil)
	_, err := f.th.Store.GetInternalMasterDB().Exec(`UPDATE WorkspaceSAMLRequests SET ExpiresAt=$1 WHERE Id=$2`, time.Now().Add(-time.Minute).UnixMilli(), requestID)
	require.NoError(t, err)
	_, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, requestID, nil, nil, true), nil)
	require.NotNil(t, appErr)
	requestID, _ = f.request(t, nil)
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.IdAttribute = new("mail") })
	_, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context, f.response(t, requestID, nil, nil, true), nil)
	require.NotNil(t, appErr)
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.IdAttribute = new("") })
	u, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", nil, nil, true), nil)
	require.Nil(t, appErr)
	require.Equal(t, "saml@example.test", *u.AuthData, "an unset ID mapping uses email, not NameID")
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.EnableSyncWithLdap = new(true) })
	synced, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", nil, nil, true), nil)
	require.Nil(t, appErr)
	require.Equal(t, u.Id, synced.Id)
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.EnableSyncWithLdapIncludeAuth = new(true) })
	_, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context, f.response(t, "", nil, nil, true), nil)
	require.Equal(t, http.StatusNotImplemented, appErr.StatusCode)
}

func TestSAMLConcurrentReplayAcceptedOnlyOnce(t *testing.T) {
	f := setupSAML(t)
	encoded := f.response(t, "", nil, nil, true)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			_, _, _, appErr := New(f.th.App).DoLogin(f.th.Context, encoded, nil)
			if appErr == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	require.Equal(t, int32(1), accepted.Load())
}

func TestSAMLResponseSignatureAndAmbiguousAssertions(t *testing.T) {
	f := setupSAML(t)
	raw, err := base64.StdEncoding.DecodeString(f.response(t, "", nil, nil, false))
	require.NoError(t, err)
	doc := etree.NewDocument()
	require.NoError(t, doc.ReadFromBytes(raw))
	ctx := dsig.NewDefaultSigningContext(dsig.TLSCertKeyStore(f.key))
	ctx.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	signed, err := ctx.SignEnveloped(doc.Root())
	require.NoError(t, err)
	doc.SetRoot(signed)
	raw, err = doc.WriteToBytes()
	require.NoError(t, err)
	_, info, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, base64.StdEncoding.EncodeToString(raw), nil)
	require.Nil(t, appErr)
	require.True(t, info.ResponseSignatureValidated)
	raw, err = base64.StdEncoding.DecodeString(f.response(t, "", nil, nil, true))
	require.NoError(t, err)
	require.NoError(t, doc.ReadFromBytes(raw))
	doc.Root().AddChild(doc.Root().FindElement("./Assertion").Copy())
	raw, err = doc.WriteToBytes()
	require.NoError(t, err)
	_, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context, base64.StdEncoding.EncodeToString(raw), nil)
	require.NotNil(t, appErr)
	_, _, _, appErr = f.th.App.Saml().DoLogin(f.th.Context, strings.Repeat("A", 2*1024*1024+1), nil)
	require.NotNil(t, appErr)
}
