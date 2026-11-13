package autotranslation

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestHistoryETagScope(t *testing.T) {
	path := "/api/v4/channels/" + model.NewId() + "/posts"
	for _, tc := range []struct {
		name, method, path string
		protected          bool
	}{
		{"page", "GET", path, true},
		{"subpath", "GET", "/workspace" + path + "?before=" + model.NewId(), true},
		{"since", "GET", path + "?since=10", false},
		{"since_zero", "GET", path + "?since=0", true},
		{"since_negative", "GET", path + "?since=-9223372036854775808", true},
		{"since_empty", "GET", path + "?since=", true},
		{"since_key_only", "GET", path + "?since", true},
		{"since_signed_zero", "GET", path + "?since=%2B0", true},
		{"since_leading_zeroes", "GET", path + "?since=0000", true},
		{"since_first_zero", "GET", path + "?since=0&since=10", true},
		{"since_first_empty", "GET", path + "?since=&since=10", true},
		{"since_first_positive", "GET", path + "?since=10&since=0", false},
		{"since_before", "GET", path + "?since=0&before=" + model.NewId(), true},
		{"since_after", "GET", path + "?since=-1&after=" + model.NewId(), true},
		{"since_invalid", "GET", path + "?since=broken", false},
		{"since_overflow", "GET", path + "?since=9223372036854775808", false},
		{"since_underflow", "GET", path + "?since=-9223372036854775809", false},
		{"post", "POST", path, false},
		{"other_endpoint", "GET", path + "/search", false},
		{"invalid_id", "GET", "/api/v4/channels/bad/posts", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.path, nil)
			condition := historyETagPrefix + `old"`
			request.Header.Set(model.HeaderEtagClient, condition)
			response := httptest.NewRecorder()
			(&Service{}).historyETag(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.protected {
					require.Empty(t, r.Header.Get(model.HeaderEtagClient))
				} else {
					require.Equal(t, condition, r.Header.Get(model.HeaderEtagClient))
				}
				_, _ = w.Write([]byte(`{"posts":{}}`))
			})).ServeHTTP(response, request)
			require.Equal(t, condition, request.Header.Get(model.HeaderEtagClient), "the original request must remain intact")
			require.Equal(t, http.StatusOK, response.Code)
		})
	}
}

func TestHistoryETagEncodedResponsesAndConditionalMatching(t *testing.T) {
	path := "/api/v4/channels/" + model.NewId() + "/posts"
	body := strings.Repeat(`{"message":"synthetic translated payload"}`, 100)
	handler := (&Service{}).historyETag(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(w)
		_, _ = io.WriteString(compressed, body)
		require.NoError(t, compressed.Close())
	}))
	first := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set(model.HeaderEtagClient, historyETagPrefix+`old"`)
	handler.ServeHTTP(first, request)
	reader, err := gzip.NewReader(bytes.NewReader(first.Body.Bytes()))
	require.NoError(t, err)
	decoded, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, body, string(decoded))
	etag := first.Header().Get(model.HeaderEtagServer)
	for _, condition := range []string{etag, "W/" + etag, `"other", W/` + etag, "*, " + etag} {
		second := httptest.NewRecorder()
		request.Header.Set(model.HeaderEtagClient, condition)
		handler.ServeHTTP(second, request)
		require.Equal(t, http.StatusNotModified, second.Code)
		require.Zero(t, second.Body.Len())
		require.Equal(t, etag, second.Header().Get(model.HeaderEtagServer))
	}
}

func TestHistoryETagBoundedBufferAndFlushPreserveCompleteResponse(t *testing.T) {
	path := "/api/v4/channels/" + model.NewId() + "/posts"
	for _, flush := range []bool{false, true} {
		t.Run(map[bool]string{false: "overflow", true: "flush"}[flush], func(t *testing.T) {
			data := bytes.Repeat([]byte("s"), historyBufferLimit+17)
			handler := (&Service{}).historyETag(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(model.HeaderEtagServer, "native-validator")
				_, err := w.Write(data[:10])
				require.NoError(t, err)
				if flush {
					require.NoError(t, http.NewResponseController(w).Flush())
				}
				_, err = w.Write(data[10:])
				require.NoError(t, err)
			}))
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set(model.HeaderEtagClient, historyETagPrefix+`old"`)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code)
			require.Equal(t, data, response.Body.Bytes())
			require.Empty(t, response.Header().Get(model.HeaderEtagServer))
			require.Equal(t, "private, no-store", response.Header().Get("Cache-Control"))
		})
	}
}

type failedHistoryWriter struct{ *httptest.ResponseRecorder }

func (failedHistoryWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestHistoryETagStreamingPropagatesWriteFailure(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v4/channels/"+model.NewId()+"/posts", nil)
	request.Header.Set(model.HeaderEtagClient, historyETagPrefix+`old"`)
	var observed error
	(&Service{}).historyETag(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, observed = w.Write(bytes.Repeat([]byte("s"), historyBufferLimit+1))
	})).ServeHTTP(failedHistoryWriter{httptest.NewRecorder()}, request)
	require.True(t, errors.Is(observed, io.ErrClosedPipe))
}
