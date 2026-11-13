package elasticsearch_indexing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/opensearch-project/opensearch-go/v4/opensearchutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenSearchClientBoundsStalledResponse(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"took":1,`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(unblock)
	cfg := &model.Config{}
	cfg.SetDefaults()
	cfg.ElasticsearchSettings.ConnectionURL = new(server.URL)
	cfg.ElasticsearchSettings.RequestTimeoutSeconds = new(1)
	cfg.ElasticsearchSettings.Sniff = new(false)
	config, err := openSearchClientConfig(cfg.ElasticsearchSettings, nil, mlog.CreateConsoleTestLogger(t))
	require.NoError(t, err)
	client, err := opensearchapi.NewClient(config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	processor, err := opensearchutil.NewBulkIndexer(opensearchutil.BulkIndexerConfig{Client: client, NumWorkers: 1, FlushBytes: 1})
	require.NoError(t, err)
	var failed atomic.Int64
	require.NoError(t, processor.Add(context.Background(), opensearchutil.BulkIndexerItem{
		Index: "posts", Action: "index", DocumentID: model.NewId(), Body: strings.NewReader(`{}`),
		OnFailure: func(_ context.Context, _ opensearchutil.BulkIndexerItem, _ opensearchapi.BulkRespItem, err error) {
			assert.Error(t, err)
			failed.Add(1)
		},
	}))
	done := make(chan error, 1)
	go func() { done <- processor.Close(context.Background()) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(6 * time.Second):
		unblock()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("processor did not stop after releasing the fixture")
		}
		t.Fatal("processor did not respect the configured request timeout")
	}
	require.EqualValues(t, 1, failed.Load())
}

func TestOpenSearchClientTLSAndAuthentication(t *testing.T) {
	certPEM, keyPEM := indexingClientCertificate(t)
	block, _ := pem.Decode(certPEM)
	clientCert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(clientCert)
	var authenticated atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "indexer-fixture" || password != "fixture-password" || r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		authenticated.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"indexer-fixture","version":{"number":"2.19.3","distribution":"opensearch"}}`)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	directory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(directory, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "client.pem"), certPEM, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "client.key"), keyPEM, 0600))
	backend, err := filestore.NewFileBackend(filestore.FileBackendSettings{DriverName: model.ImageDriverLocal, Directory: directory})
	require.NoError(t, err)
	for _, tc := range []struct {
		name                  string
		trust, skip, sendCert bool
		password              string
		wantSuccess           bool
	}{
		{"trusted CA and client certificate", true, false, true, "fixture-password", true},
		{"untrusted server rejected", false, false, true, "fixture-password", false},
		{"missing client certificate rejected", true, false, false, "fixture-password", false},
		{"wrong password rejected", true, false, true, "wrong-fixture-password", false},
		{"explicit administrator skip verification", false, true, true, "fixture-password", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &model.Config{}
			cfg.SetDefaults()
			cfg.ElasticsearchSettings.ConnectionURL = new(server.URL)
			cfg.ElasticsearchSettings.Username = new("indexer-fixture")
			cfg.ElasticsearchSettings.Password = new(tc.password)
			cfg.ElasticsearchSettings.RequestTimeoutSeconds = new(1)
			cfg.ElasticsearchSettings.Sniff = new(false)
			cfg.ElasticsearchSettings.SkipTLSVerification = new(tc.skip)
			if tc.trust {
				cfg.ElasticsearchSettings.CA = new("ca.pem")
			}
			if tc.sendCert {
				cfg.ElasticsearchSettings.ClientCert = new("client.pem")
				cfg.ElasticsearchSettings.ClientKey = new("client.key")
			}
			config, err := openSearchClientConfig(cfg.ElasticsearchSettings, backend, mlog.CreateConsoleTestLogger(t))
			require.NoError(t, err)
			client, err := opensearchapi.NewClient(config)
			require.NoError(t, err)
			defer func() { require.NoError(t, client.Close()) }()
			info, err := client.Info(t.Context(), nil)
			if tc.wantSuccess {
				require.NoError(t, err)
				require.Equal(t, "indexer-fixture", info.Name)
			} else {
				require.Error(t, err)
			}
		})
	}
	require.EqualValues(t, 2, authenticated.Load())
}

func TestOpenSearchClientRejectsInvalidCertificateFiles(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(directory, "invalid.pem"), []byte("invalid certificate fixture"), 0600))
	backend, err := filestore.NewFileBackend(filestore.FileBackendSettings{DriverName: model.ImageDriverLocal, Directory: directory})
	require.NoError(t, err)
	for _, tc := range []struct{ name, ca, cert, key string }{
		{"missing CA", "missing.pem", "", ""},
		{"invalid CA", "invalid.pem", "", ""},
		{"missing key", "", "invalid.pem", "missing.key"},
		{"invalid pair", "", "invalid.pem", "invalid.pem"},
		{"key without certificate", "", "", "invalid.pem"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &model.Config{}
			cfg.SetDefaults()
			cfg.ElasticsearchSettings.Sniff = new(false)
			cfg.ElasticsearchSettings.CA = new(tc.ca)
			cfg.ElasticsearchSettings.ClientCert = new(tc.cert)
			cfg.ElasticsearchSettings.ClientKey = new(tc.key)
			config, err := openSearchClientConfig(cfg.ElasticsearchSettings, backend, mlog.CreateConsoleTestLogger(t))
			if err == nil {
				var client *opensearchapi.Client
				client, err = opensearchapi.NewClient(config)
				if client != nil {
					defer client.Close()
				}
			}
			require.Error(t, err)
		})
	}
}

func indexingClientCertificate(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "indexing-client-fixture"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	encodedKey, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encodedKey})
}
