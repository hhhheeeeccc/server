package elasticsearch_indexing

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestIndexingWorkerFactoryTLSAndAuthentication(t *testing.T) {
	th, _, prefix := setupIndexer(t)
	certPEM, keyPEM := indexingClientCertificate(t)
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	var authenticated atomic.Int64
	receiver := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "indexer-fixture" || password != "fixture-password" || r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		authenticated.Add(1)
		items := []map[string]any{}
		scanner := bufio.NewScanner(r.Body)
		for scanner.Scan() {
			var meta map[string]map[string]any
			if json.Unmarshal(scanner.Bytes(), &meta) != nil || !scanner.Scan() {
				http.Error(w, "invalid Bulk fixture", 400)
				return
			}
			for action, fields := range meta {
				items = append(items, map[string]any{action: map[string]any{"_id": fields["_id"], "_index": fields["_index"], "status": 201}})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		_ = json.NewEncoder(w).Encode(map[string]any{"took": 1, "errors": false, "items": items})
	}))
	receiver.Config.ErrorLog = log.New(io.Discard, "", 0)
	receiver.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, MinVersion: tls.VersionTLS12}
	receiver.StartTLS()
	t.Cleanup(receiver.Close)
	backend := th.Server.Platform().FileBackend()
	paths := []string{model.NewId() + "-ca.pem", model.NewId() + "-client.pem", model.NewId() + "-client.key"}
	for n, data := range [][]byte{pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: receiver.Certificate().Raw}), certPEM, keyPEM} {
		_, err := backend.WriteFile(bytes.NewReader(data), paths[n])
		require.NoError(t, err)
		path := paths[n]
		t.Cleanup(func() { require.NoError(t, backend.RemoveFile(path)) })
	}
	for _, scenario := range []struct {
		name                        string
		trust, sendCert, skip, want bool
		password                    string
	}{
		{"trusted-ca-and-client-certificate", true, true, false, true, "fixture-password"},
		{"untrusted-ca", false, true, false, false, "fixture-password"},
		{"missing-client-certificate", true, false, false, false, "fixture-password"},
		{"wrong-password", true, true, false, false, "wrong-fixture-password"},
		{"explicit-skip-verification", false, true, true, true, "fixture-password"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.ElasticsearchSettings.ConnectionURL = new(receiver.URL)
				cfg.ElasticsearchSettings.Username = new("indexer-fixture")
				cfg.ElasticsearchSettings.Password = new(scenario.password)
				cfg.ElasticsearchSettings.RequestTimeoutSeconds = new(1)
				cfg.ElasticsearchSettings.SkipTLSVerification = new(scenario.skip)
				cfg.ElasticsearchSettings.CA = new("")
				cfg.ElasticsearchSettings.ClientCert = new("")
				cfg.ElasticsearchSettings.ClientKey = new("")
				if scenario.trust {
					cfg.ElasticsearchSettings.CA = new(paths[0])
				}
				if scenario.sendCert {
					cfg.ElasticsearchSettings.ClientCert = new(paths[1])
					cfg.ElasticsearchSettings.ClientKey = new(paths[2])
				}
			})
			w := New(th.Server).MakeWorker().(*checkpointWorker)
			// No SQL Job is claimed: this tests the actual factory's TLS,
			// ownership transport wrapper, native queue, callbacks and Close.
			w.fence.enabled = false
			w.store.postgres = false
			require.NoError(t, w.create(), "wrapping the HTTP transport must not invalidate a configured CA")
			require.NoError(t, w.add(prefix+"tls", "index", model.NewId(), strings.NewReader(`{"message":"TLS factory"}`)))
			failed, err := w.close()
			if scenario.want {
				require.NoError(t, err)
				require.Zero(t, failed)
			} else {
				require.Positive(t, failed, "TLS/authentication rejection must never become a successful write")
			}
		})
	}
	require.EqualValues(t, 2, authenticated.Load())
}
