package license

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost/server/public/model"
)

func TestValidatorUsesWorkspaceKeyAndAddsStableLicenseID(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	require.NoError(t, err)
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	validator := newValidator(publicPEM)

	payload := []byte(`{"id":"","customer":{"name":"Workspace"},"features":{"users":100}}`)
	digest := sha512.Sum512(payload)
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA512, digest[:])
	require.NoError(t, err)
	signed := base64.StdEncoding.EncodeToString(append(payload, signature...))

	first, err := validator.ValidateLicense([]byte(signed))
	require.NoError(t, err)
	second, err := validator.ValidateLicense([]byte(signed))
	require.NoError(t, err)
	require.JSONEq(t, first, second)

	var validated model.License
	require.NoError(t, json.Unmarshal([]byte(first), &validated))
	require.True(t, model.IsValidId(validated.Id))

	decoded, appErr := validator.LicenseFromBytes([]byte(signed))
	require.Nil(t, appErr)
	require.Equal(t, validated.Id, decoded.Id)
	require.NotNil(t, decoded.Features.AutoTranslation)
	require.True(t, *decoded.Features.AutoTranslation)

	tampered := append([]byte(nil), []byte(signed)...)
	tampered[len(tampered)-2] ^= 1
	_, err = validator.ValidateLicense(tampered)
	require.Error(t, err)
}

func TestEmbeddedWorkspacePublicKey(t *testing.T) {
	publicKey, err := parseRSAPublicKey(workspacePublicKey)
	require.NoError(t, err)
	require.Equal(t, 2048, publicKey.Size()*8)
}
