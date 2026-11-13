package encryptedresponse

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"encoding/base64"
	"testing"

	"github.com/mattermost/gosaml2/types"
	"github.com/stretchr/testify/require"
)

func testCiphertext(t *testing.T, key *rsa.PrivateKey, options *rsa.OAEPOptions, algorithm string) (encryptedAssertion, []byte) {
	t.Helper()
	plain := []byte("<Assertion>independent OAEP and MGF hashes</Assertion>")
	symmetric := make([]byte, 32)
	_, err := rand.Read(symmetric)
	require.NoError(t, err)
	wrapped, err := rsa.EncryptOAEPWithOptions(rand.Reader, &key.PublicKey, symmetric, options)
	require.NoError(t, err)
	block, err := aes.NewCipher(symmetric)
	require.NoError(t, err)
	var data []byte
	if algorithm == types.MethodAES256GCM {
		gcm, err := cipher.NewGCM(block)
		require.NoError(t, err)
		nonce := make([]byte, gcm.NonceSize())
		_, err = rand.Read(nonce)
		require.NoError(t, err)
		data = gcm.Seal(nonce, nonce, plain, nil)
	} else {
		padding := block.BlockSize() - len(plain)%block.BlockSize()
		padded := append(bytes.Clone(plain), bytes.Repeat([]byte{byte(padding)}, padding)...)
		data = make([]byte, block.BlockSize()+len(padded))
		_, err = rand.Read(data[:block.BlockSize()])
		require.NoError(t, err)
		cipher.NewCBCEncrypter(block, data[:block.BlockSize()]).CryptBlocks(data[block.BlockSize():], padded)
	}
	return encryptedAssertion{EncryptionMethod: encryptionMethod{Algorithm: algorithm},
		EncryptedKey: encryptedKey{CipherValue: base64.StdEncoding.EncodeToString(wrapped),
			EncryptionMethod: encryptionMethod{Algorithm: types.MethodRSAOAEP2, OAEPparams: base64.StdEncoding.EncodeToString(options.Label)}},
		CipherValue: base64.StdEncoding.EncodeToString(data)}, plain
}

func TestOAEPStandardDigestsAndIndependentMGF(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	cert := &tls.Certificate{Certificate: [][]byte{[]byte("test-sp-certificate")}, PrivateKey: key}
	for _, test := range []struct {
		name, algorithm, digestURI, mgfURI string
		digest, mgf                        crypto.Hash
		detached, cbc                      bool
	}{
		{name: "XML10_defaults", algorithm: types.MethodRSAOAEP, digest: crypto.SHA1, mgf: crypto.SHA1},
		{name: "XML10_SHA256_fixed_MGF1_SHA1", algorithm: types.MethodRSAOAEP, digest: crypto.SHA256, mgf: crypto.SHA1, digestURI: "http://www.w3.org/2001/04/xmlenc#sha256"},
		{name: "XML11_SHA256_default_MGF1_SHA1", algorithm: types.MethodRSAOAEP2, digest: crypto.SHA256, mgf: crypto.SHA1, digestURI: "http://www.w3.org/2001/04/xmlenc#sha256"},
		{name: "XML11_SHA256_explicit_MGF1_SHA256", algorithm: types.MethodRSAOAEP2, digest: crypto.SHA256, mgf: crypto.SHA256, digestURI: "http://www.w3.org/2001/04/xmlenc#sha256", mgfURI: "http://www.w3.org/2009/xmlenc11#mgf1sha256"},
		{name: "XML11_SHA384_detached_key", algorithm: types.MethodRSAOAEP2, digest: crypto.SHA384, mgf: crypto.SHA384, digestURI: "http://www.w3.org/2001/04/xmldsig-more#sha384", mgfURI: "http://www.w3.org/2009/xmlenc11#mgf1sha384", detached: true},
		{name: "XML11_SHA512_CBC", algorithm: types.MethodRSAOAEP2, digest: crypto.SHA512, mgf: crypto.SHA512, digestURI: "http://www.w3.org/2001/04/xmlenc#sha512", mgfURI: "http://www.w3.org/2009/xmlenc11#mgf1sha512", cbc: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			algorithm := types.MethodAES256GCM
			if test.cbc {
				algorithm = types.MethodAES256CBC
			}
			ea, want := testCiphertext(t, key, &rsa.OAEPOptions{Hash: test.digest, MGFHash: test.mgf, Label: []byte("fixture-label")}, algorithm)
			ea.EncryptedKey.EncryptionMethod.Algorithm = test.algorithm
			if test.digestURI != "" {
				ea.EncryptedKey.EncryptionMethod.DigestMethod = &types.DigestMethod{Algorithm: test.digestURI}
			}
			if test.mgfURI != "" {
				ea.EncryptedKey.EncryptionMethod.MGF = &struct {
					Algorithm string `xml:",attr"`
				}{Algorithm: test.mgfURI}
			}
			if test.detached {
				ea.DetEncryptedKey, ea.EncryptedKey = ea.EncryptedKey, encryptedKey{}
			}
			got, err := ea.DecryptBytes(cert)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

func TestOAEPRejectsInvalidParametersAndCiphertext(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	cert := &tls.Certificate{Certificate: [][]byte{[]byte("test-sp-certificate")}, PrivateKey: key}
	valid, _ := testCiphertext(t, key, &rsa.OAEPOptions{Hash: crypto.SHA256, MGFHash: crypto.SHA1}, types.MethodAES256GCM)
	valid.EncryptedKey.EncryptionMethod.DigestMethod = &types.DigestMethod{Algorithm: "http://www.w3.org/2001/04/xmlenc#sha256"}
	for _, test := range []struct {
		name string
		edit func(*encryptedAssertion)
	}{
		{"unknown_digest", func(ea *encryptedAssertion) {
			ea.EncryptedKey.EncryptionMethod.DigestMethod = &types.DigestMethod{Algorithm: "unsupported"}
		}},
		{"unknown_MGF", func(ea *encryptedAssertion) {
			ea.EncryptedKey.EncryptionMethod.MGF = &struct {
				Algorithm string `xml:",attr"`
			}{Algorithm: "unsupported"}
		}},
		{"wrong_MGF_is_not_ignored", func(ea *encryptedAssertion) {
			ea.EncryptedKey.EncryptionMethod.MGF = &struct {
				Algorithm string `xml:",attr"`
			}{Algorithm: "http://www.w3.org/2009/xmlenc11#mgf1sha256"}
		}},
		{"wrong_label_is_not_ignored", func(ea *encryptedAssertion) {
			ea.EncryptedKey.EncryptionMethod.OAEPparams = base64.StdEncoding.EncodeToString([]byte("wrong-label"))
		}},
		{"mismatched_certificate", func(ea *encryptedAssertion) {
			ea.EncryptedKey.X509Data = base64.StdEncoding.EncodeToString([]byte("another-certificate"))
		}},
		{"truncated_GCM", func(ea *encryptedAssertion) { ea.CipherValue = "AQ==" }},
		{"truncated_CBC", func(ea *encryptedAssertion) {
			ea.EncryptionMethod.Algorithm = types.MethodAES256CBC
			ea.CipherValue = "AQ=="
		}},
		{"wrong_AES_key_size", func(ea *encryptedAssertion) { ea.EncryptionMethod.Algorithm = types.MethodAES128GCM }},
		{"invalid_cipher_base64", func(ea *encryptedAssertion) { ea.CipherValue = "*invalid*" }},
		{"invalid_key_base64", func(ea *encryptedAssertion) { ea.EncryptedKey.CipherValue = "*invalid*" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ea := valid
			test.edit(&ea)
			require.NotPanics(t, func() {
				plain, err := ea.DecryptBytes(cert)
				require.Error(t, err)
				require.Nil(t, plain)
			})
		})
	}
}
