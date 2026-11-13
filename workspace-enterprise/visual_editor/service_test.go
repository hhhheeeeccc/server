package visual_editor

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/gzhttp"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func TestSettings(t *testing.T) {
	for _, tc := range []struct {
		name, text     string
		valid, enabled bool
	}{
		{"enabled", `{"Enabled":true}`, true, true},
		{"disabled", `{"Enabled":false}`, true, false},
		{"default disabled", `{}`, true, false},
		{"unknown field", `{"Enable":true}`, false, false},
		{"wrong type", `{"Enabled":"true"}`, false, false},
		{"trailing object", `{"Enabled":true}{}`, false, false},
		{"null", `null`, false, false},
		{"oversized", strings.Repeat(" ", 4097), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "config.json")
			require.NoError(t, os.WriteFile(filename, []byte(tc.text), 0600))
			settings, err := loadSettings(filename)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.enabled, settings.Enabled)
			reloaded, err := loadSettings(filename)
			require.NoError(t, err)
			require.Equal(t, settings, reloaded)
		})
	}
}

func TestMissingSettings(t *testing.T) {
	for _, tc := range []struct {
		name      string
		directory bool
		wantError bool
	}{
		{name: "missing file defaults disabled"},
		{name: "directory is an error", directory: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "settings.json")
			if tc.directory {
				require.NoError(t, os.Mkdir(filename, 0700))
			}
			settings, err := loadSettings(filename)
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.False(t, settings.Enabled)
			// Loading defaults does not create or overwrite deployment files.
			_, err = os.Stat(filename)
			require.True(t, os.IsNotExist(err))
		})
	}
}

func TestClientMiddleware(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		licensed := false
		handler := middleware(func() bool { return enabled }, "/workspace/api/v4/config/client", func() bool { return licensed })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Request-Id", "preserved")
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "999")
			w.Header().Set("ETag", "original")
			_, _ = w.Write([]byte(`{"FeatureFlagWysiwygEditor":"false","SiteName":"Workspace"}`))
		}))
		for _, license := range []bool{false, true, false} {
			licensed = license
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/workspace/api/v4/config/client?format=old", nil))
			require.Equal(t, http.StatusOK, response.Code)
			var config map[string]string
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &config))
			expected := "false"
			if enabled && licensed {
				expected = "true"
			}
			require.Equal(t, expected, config[clientSetting])
			require.Equal(t, "Workspace", config["SiteName"])
			require.Equal(t, "false", config["FeatureFlagWysiwygEditor"])
			require.Equal(t, "preserved", response.Header().Get("X-Request-Id"))
			require.Empty(t, response.Header().Get("Content-Length"))
			require.Empty(t, response.Header().Get("ETag"))
		}
	}
}

func TestMiddlewarePassThrough(t *testing.T) {
	for _, tc := range []struct {
		method, route string
		status        int
		body          string
	}{
		{http.MethodGet, "/api/v4/websocket", http.StatusSwitchingProtocols, "stream"},
		{http.MethodPut, "/api/v4/config/client", http.StatusBadRequest, "bad request"},
		{http.MethodGet, "/api/v4/config/client", http.StatusUnauthorized, `{"error":"unauthorized"}`},
		{http.MethodGet, "/api/v4/config/client", http.StatusOK, "invalid json"},
	} {
		t.Run(tc.method+tc.route+tc.body, func(t *testing.T) {
			handler := middleware(func() bool { return true }, "/api/v4/config/client", func() bool { return true })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.route, nil))
			require.Equal(t, tc.status, response.Code)
			require.Equal(t, tc.body, response.Body.String())
		})
	}
}

func TestCompressedClientConfig(t *testing.T) {
	for _, encoding := range []string{"gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			core := gzhttp.GzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"SiteName": strings.Repeat("Workspace", 2000)})
			}))
			handler := middleware(func() bool { return true }, "/api/v4/config/client", func() bool { return true })(core)
			request := httptest.NewRequest(http.MethodGet, "/api/v4/config/client", nil)
			request.Header.Set("Accept-Encoding", encoding)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			require.Equal(t, encoding, response.Header().Get("Content-Encoding"))
			require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			var decoded io.Reader
			if encoding == "gzip" {
				reader, err := gzip.NewReader(response.Body)
				require.NoError(t, err)
				defer reader.Close()
				decoded = reader
			} else {
				reader, err := zstd.NewReader(response.Body)
				require.NoError(t, err)
				defer reader.Close()
				decoded = reader
			}
			var config map[string]string
			require.NoError(t, json.NewDecoder(decoded).Decode(&config))
			require.Equal(t, "true", config[clientSetting])
			require.Equal(t, strings.Repeat("Workspace", 2000), config["SiteName"])
		})
	}
}

func TestInvalidEncodedClientBody(t *testing.T) {
	for _, encoding := range []string{"gzip", "zstd", "br"} {
		_, ok := updateClientBody([]byte("invalid"), encoding, true)
		require.False(t, ok)
	}
	var body bytes.Buffer
	writer := gzip.NewWriter(&body)
	_, err := io.WriteString(writer, strings.Repeat(" ", (1<<20)+1))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	_, ok := updateClientBody(body.Bytes(), "gzip", true)
	require.False(t, ok)
	encoder, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	defer encoder.Close()
	_, ok = updateClientBody(encoder.EncodeAll([]byte(strings.Repeat(" ", (1<<20)+1)), nil), "zstd", true)
	require.False(t, ok)
}
