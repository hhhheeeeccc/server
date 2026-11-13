// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package config

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const signedTestDSN = "postgres://test_user:test_password@localhost/example_database?sslmode=disable"

func signTestSQL(t *testing.T, value string, private *rsa.PrivateKey) string {
	t.Helper()
	digest := sha256.Sum256(append([]byte(signedSQLPrefix), []byte(value)...))
	signature, err := rsa.SignPSS(rand.Reader, private, crypto.SHA256, digest[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	require.NoError(t, err)
	data, err := json.Marshal(signedSQLDocument{Data: []byte(value), Signature: signature})
	require.NoError(t, err)
	return signedSQLPrefix + base64.RawURLEncoding.EncodeToString(data)
}

func signedSQLFixture(t *testing.T) (string, string, *rsa.PrivateKey) {
	t.Helper()
	t.Setenv("MM_SQLSETTINGS_DATASOURCE", "")
	require.NoError(t, os.Unsetenv("MM_SQLSETTINGS_DATASOURCE"))
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	publicDER, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	require.NoError(t, err)
	previousPublic := databasePublicKeyPEM
	databasePublicKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	t.Cleanup(func() { databasePublicKeyPEM = previousPublic })
	token := signTestSQL(t, signedTestDSN, private)
	cfg := minimalConfig.Clone()
	cfg.SqlSettings.DataSource = new(token)
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, data, 0600))
	return path, token, private
}

func TestSignedSQLLoadSaveReload(t *testing.T) {
	path, original, private := signedSQLFixture(t)
	store, err := NewStoreFromDSN(path, false, nil, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.Equal(t, signedTestDSN, *store.Get().SqlSettings.DataSource)
	require.Equal(t, original, *store.GetNoEnv().SqlSettings.DataSource)

	next := store.Get().Clone()
	next.ServiceSettings.SiteURL = new("http://signed-update")
	_, _, err = store.Set(next)
	require.NoError(t, err)
	require.Equal(t, original, *store.GetNoEnv().SqlSettings.DataSource)
	require.NoError(t, store.Load())

	// A public-only server must not persist unsigned credential changes.
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	next = store.Get().Clone()
	next.SqlSettings.DataSource = new("postgres://changed:password@localhost/changed")
	_, _, err = store.Set(next)
	require.Error(t, err)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, signedTestDSN, *store.Get().SqlSettings.DataSource)

	// An externally signed replacement and signed replicas can be saved safely.
	changed := "postgres://changed:password@localhost/new_database"
	replica := "postgres://replica:password@localhost/new_database"
	next.SqlSettings.DataSource = new(signTestSQL(t, changed, private))
	next.SqlSettings.DataSourceReplicas = []string{signTestSQL(t, replica, private)}
	next.SqlSettings.DataSourceSearchReplicas = []string{signTestSQL(t, replica, private)}
	_, runtime, err := store.Set(next)
	require.NoError(t, err)
	require.Equal(t, changed, *runtime.SqlSettings.DataSource)
	require.NoError(t, store.Load())
	require.Equal(t, changed, *store.Get().SqlSettings.DataSource)
	require.Equal(t, []string{replica}, store.Get().SqlSettings.DataSourceReplicas)
	require.Equal(t, []string{replica}, store.Get().SqlSettings.DataSourceSearchReplicas)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(data), "postgres://")
}

func TestSignedSQLRejectsInvalidInput(t *testing.T) {
	for _, failure := range []string{"wrong issuer", "data modified", "signature modified", "bad base64", "bad json", "empty data", "unknown version", "encrypted format"} {
		t.Run(failure, func(t *testing.T) {
			path, token, _ := signedSQLFixture(t)
			data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, signedSQLPrefix))
			require.NoError(t, err)
			var document signedSQLDocument
			require.NoError(t, json.Unmarshal(data, &document))
			switch failure {
			case "wrong issuer":
				other, err := rsa.GenerateKey(rand.Reader, 2048)
				require.NoError(t, err)
				token = signTestSQL(t, signedTestDSN, other)
			case "data modified":
				document.Data[0] ^= 1
			case "signature modified":
				document.Signature[0] ^= 1
			case "bad base64":
				token = signedSQLPrefix + "%%%"
			case "bad json":
				token = signedSQLPrefix + base64.RawURLEncoding.EncodeToString([]byte("invalid json"))
			case "empty data":
				document.Data = nil
			case "unknown version":
				token = strings.Replace(token, ":v1:", ":v9:", 1)
			case "encrypted format":
				token = "encrypted:rsa:v1:obsolete"
			}
			if failure == "data modified" || failure == "signature modified" || failure == "empty data" {
				data, err = json.Marshal(document)
				require.NoError(t, err)
				token = signedSQLPrefix + base64.RawURLEncoding.EncodeToString(data)
			}
			cfg := minimalConfig.Clone()
			cfg.SqlSettings.DataSource = new(token)
			before, err := json.Marshal(cfg)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, before, 0600))
			store, err := NewStoreFromDSN(path, false, nil, false)
			require.Error(t, err)
			require.Nil(t, store)
			require.NotContains(t, err.Error(), signedTestDSN)
			require.NotContains(t, err.Error(), token)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestSignedSQLEnvironmentPrecedence(t *testing.T) {
	path, token, private := signedSQLFixture(t)
	override := "postgres://override:password@localhost/override"
	t.Setenv("MM_SQLSETTINGS_DATASOURCE", signTestSQL(t, override, private))
	store, err := NewStoreFromDSN(path, false, nil, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.Equal(t, override, *store.Get().SqlSettings.DataSource)
	_, _, err = store.Set(store.Get())
	require.NoError(t, err)
	require.Equal(t, token, *store.GetNoEnv().SqlSettings.DataSource)
	t.Setenv("MM_SQLSETTINGS_DATASOURCE", override)
	require.NoError(t, store.Load())
	require.Equal(t, override, *store.Get().SqlSettings.DataSource)
	_, _, err = store.Set(store.Get())
	require.NoError(t, err)
	require.Equal(t, token, *store.GetNoEnv().SqlSettings.DataSource)
}

func TestSignedSQLSupportsLongURLs(t *testing.T) {
	_, _, private := signedSQLFixture(t)
	url := signedTestDSN + "&application_name=" + strings.Repeat("x", 4096)
	actual, err := verifySQLDataSource(signTestSQL(t, url, private))
	require.NoError(t, err)
	require.Equal(t, url, actual)
}

func TestSignedSQLPlaintextCompatibility(t *testing.T) {
	store := NewTestMemoryStore()
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, _, err := store.Set(minimalConfig.Clone())
	require.NoError(t, err)
}
