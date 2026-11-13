package autotranslation

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProviderTimeoutDoesNotOverflow(t *testing.T) {
	require.Equal(t, 5*time.Second, boundedProviderTimeout(nil))
	require.Equal(t, 1500*time.Millisecond, boundedProviderTimeout(new(1500)))
	require.Equal(t, time.Duration(math.MaxInt64), boundedProviderTimeout(new(math.MaxInt)))
}

func TestProviderRedirectKeepsRequestOnOriginalOrigin(t *testing.T) {
	var crossOriginRequests atomic.Int32
	crossOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		crossOriginRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer crossOrigin.Close()

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, crossOrigin.URL+"/capture", http.StatusTemporaryRedirect)
			return
		}
		http.NotFound(w, r)
	}))
	defer provider.Close()
	origin, err := url.Parse(provider.URL)
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, provider.URL+"/redirect", strings.NewReader(`{"q":"private message","api_key":"secret"}`))
	require.NoError(t, err)
	response, err := providerHTTPClient(origin).Do(request)
	if response != nil {
		response.Body.Close()
	}
	require.ErrorContains(t, err, "different origin")
	require.Zero(t, crossOriginRequests.Load())
}

func TestProviderRedirectAllowsSameOrigin(t *testing.T) {
	requests := make(chan struct{ method, body string }, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "unable to read request", http.StatusBadRequest)
			return
		}
		requests <- struct{ method, body string }{r.Method, string(body)}
		w.WriteHeader(http.StatusOK)
	}))
	defer provider.Close()
	origin, err := url.Parse(provider.URL)
	require.NoError(t, err)
	payload := `{"q":"private message","api_key":"secret"}`
	request, err := http.NewRequest(http.MethodPost, provider.URL+"/redirect", strings.NewReader(payload))
	require.NoError(t, err)
	response, err := providerHTTPClient(origin).Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	received := <-requests
	require.Equal(t, http.MethodPost, received.method)
	require.Equal(t, payload, received.body)
}

func TestProviderFailureDoesNotExposeEchoedMessageOrAPIKey(t *testing.T) {
	recorder := httptest.NewRecorder()
	recorder.WriteHeader(http.StatusBadRequest)
	_, err := recorder.WriteString(`{"error":"private message; api_key=secret"}`)
	require.NoError(t, err)
	response := recorder.Result()
	defer response.Body.Close()

	var result translateResponse
	appErr := decodeProviderResponse(response, &result)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusBadGateway, appErr.StatusCode)
	require.Contains(t, appErr.DetailedError, "400")
	require.NotContains(t, appErr.DetailedError, "private message")
	require.NotContains(t, appErr.DetailedError, "secret")
}

func TestProviderResponseRejectsOversizedAndTrailingJSON(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"oversized valid JSON", `{"translatedText":"` + strings.Repeat("x", maxProviderPayload) + `"}`},
		{"trailing JSON", `{"translatedText":"translated"} {"translatedText":"second"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			recorder.WriteHeader(http.StatusOK)
			_, err := recorder.WriteString(tc.body)
			require.NoError(t, err)
			response := recorder.Result()
			defer response.Body.Close()
			var result translateResponse
			appErr := decodeProviderResponse(response, &result)
			require.NotNil(t, appErr)
			require.Equal(t, http.StatusBadGateway, appErr.StatusCode)
			require.Empty(t, result.TranslatedText)
		})
	}
}
