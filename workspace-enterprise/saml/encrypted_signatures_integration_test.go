package saml

import (
	"crypto/rsa"
	"encoding/base64"
	"testing"

	"github.com/beevik/etree"
	"github.com/mattermost/mattermost/server/public/model"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/stretchr/testify/require"
)

func TestSAMLEncryptedResponseSignatures(t *testing.T) {
	f := setupSAML(t)
	f.th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.Encrypt = new(true) })
	sp, err := configure(f.th.App.Config().SamlSettings, f.th.App.GetConfigFile)
	require.NoError(t, err)
	parse := func(encoded string) *etree.Document {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		require.NoError(t, err)
		doc := etree.NewDocument()
		require.NoError(t, doc.ReadFromBytes(raw))
		return doc
	}
	encode := func(doc *etree.Document) string {
		raw, err := doc.WriteToBytes()
		require.NoError(t, err)
		return base64.StdEncoding.EncodeToString(raw)
	}
	encrypt := func(encoded string) string {
		return encryptedResponse(t, encoded, f.key.PrivateKey.(*rsa.PrivateKey), false)
	}
	signResponse := func(encoded string) string {
		doc := parse(encoded)
		ctx := dsig.NewDefaultSigningContext(dsig.TLSCertKeyStore(f.key))
		ctx.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
		signed, err := ctx.SignEnveloped(doc.Root())
		require.NoError(t, err)
		doc.SetRoot(signed)
		return encode(doc)
	}
	for _, mode := range []struct {
		name              string
		assertion, parent bool
	}{
		{"signed_assertion", true, false},
		{"signed_response", false, true},
		{"both_signed", true, true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			encoded := encrypt(f.response(t, "", nil, nil, mode.assertion))
			if mode.parent {
				encoded = signResponse(encoded)
			}
			info, err := validatedAssertion(sp, encoded)
			require.NoError(t, err)
			require.Equal(t, mode.parent, info.ResponseSignatureValidated)
			require.Equal(t, mode.assertion, info.Assertions[0].SignatureValidated)
			require.Equal(t, "saml_user", info.Values.Get("username"))
		})
	}
	t.Run("neither_signed_is_rejected", func(t *testing.T) {
		_, err := validatedAssertion(sp, encrypt(f.response(t, "", nil, nil, false)))
		require.Error(t, err)
	})
	t.Run("invalid_response_signature_is_not_ignored", func(t *testing.T) {
		doc := parse(signResponse(encrypt(f.response(t, "", nil, nil, true))))
		// This unknown attribute is ignored by the semantic XML decoder. Only
		// checking the parent's cryptographic signature can detect its change.
		doc.Root().CreateAttr("WorkspaceTampered", "true")
		_, err := validatedAssertion(sp, encode(doc))
		require.Error(t, err)
	})
	t.Run("invalid_assertion_signature_is_not_covered_by_valid_response", func(t *testing.T) {
		doc := parse(f.response(t, "", nil, nil, true))
		doc.Root().FindElement("./Assertion").CreateAttr("WorkspaceTampered", "true")
		_, err := validatedAssertion(sp, signResponse(encrypt(encode(doc))))
		require.Error(t, err)
	})
	t.Run("multiple_encrypted_assertions_are_rejected", func(t *testing.T) {
		doc := parse(encrypt(f.response(t, "", nil, nil, true)))
		second := parse(encrypt(f.response(t, "", nil, nil, true)))
		doc.Root().AddChild(second.Root().FindElement("./EncryptedAssertion").Copy())
		_, _, _, appErr := f.th.App.Saml().DoLogin(f.th.Context, signResponse(encode(doc)), nil)
		require.NotNil(t, appErr)
	})
}
