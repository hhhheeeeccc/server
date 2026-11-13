package push_proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

type testVerifier struct {
	err error
}

type verifierFunc func(context.Context, string, string) (bool, error)

func (f verifierFunc) Verify(ctx context.Context, serverID, token string) (bool, error) {
	return f(ctx, serverID, token)
}

func (v testVerifier) Verify(_ context.Context, serverID, token string) (bool, error) {
	return serverID == testServerID && token == "valid-token", v.err
}

var testServerID = model.NewId()

func TestGatewayRequiresAuthAndForwardsOnlyPushRoutes(t *testing.T) {
	forwarded := make(chan *http.Request, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded <- r
		_, _ = io.WriteString(w, "forwarded")
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	gateway, err := NewGateway(upstreamURL, testVerifier{})
	require.NoError(t, err)

	request := func(method, path, serverID string, tokens ...string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-Mattermost-ServerID", serverID)
		for _, token := range tokens {
			req.Header.Add("X-Mattermost-Auth", token)
		}
		req.Header.Set("Authorization", "Bearer must-not-forward")
		req.Header.Set("Proxy-Authorization", "Basic must-not-forward")
		req.Header.Set("Cookie", "session=must-not-forward")
		req.Header.Set("X-Forwarded-For", "198.51.100.7")
		req.Header.Set("X-Real-IP", "198.51.100.8")
		req.Header.Set("X-Custom-Secret", "must-not-forward")
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, req)
		return response.Code
	}

	require.Equal(t, http.StatusNotFound, request(http.MethodGet, "/api/v1/send_push", testServerID, "valid-token"))
	require.Equal(t, http.StatusNotFound, request(http.MethodPost, "/admin", testServerID, "valid-token"))
	require.Equal(t, http.StatusUnauthorized, request(http.MethodPost, "/api/v1/send_push", testServerID))
	require.Equal(t, http.StatusUnauthorized, request(http.MethodPost, "/api/v1/send_push", "invalid", "valid-token"))
	require.Equal(t, http.StatusUnauthorized, request(http.MethodPost, "/api/v1/send_push", testServerID, "wrong-token"))
	require.Equal(t, http.StatusUnauthorized, request(http.MethodPost, "/api/v1/send_push", testServerID,
		"valid-token", "valid-token"))
	require.Equal(t, http.StatusUnauthorized, request(http.MethodPost, "/api/v1/send_push", testServerID,
		"valid-token,wrong-token"))
	require.Empty(t, forwarded)

	require.Equal(t, http.StatusOK, request(http.MethodPost, "/api/v1/send_push", testServerID, "valid-token"))
	require.Equal(t, http.StatusOK, request(http.MethodPost, "/api/v1/ack", testServerID, "valid-token"))
	require.Len(t, forwarded, 2)
	for _, path := range []string{"/api/v1/send_push", "/api/v1/ack"} {
		received := <-forwarded
		require.Equal(t, http.MethodPost, received.Method)
		require.Equal(t, path, received.URL.Path)
		require.Empty(t, received.Header.Get("X-Mattermost-Auth"))
		require.Empty(t, received.Header.Get("X-Mattermost-ServerID"))
		require.Empty(t, received.Header.Get("Authorization"))
		require.Empty(t, received.Header.Get("Proxy-Authorization"))
		require.Empty(t, received.Header.Get("Cookie"))
		require.Empty(t, received.Header.Get("X-Forwarded-For"))
		require.Empty(t, received.Header.Get("X-Real-IP"))
		require.Empty(t, received.Header.Get("X-Custom-Secret"))
		require.Equal(t, "application/json", received.Header.Get("Content-Type"))
	}
}

func TestGatewayRejectsQueryAndOversizedBodiesBeforeForwarding(t *testing.T) {
	forwarded := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		forwarded <- struct{}{}
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	gateway, err := NewGateway(upstreamURL, testVerifier{})
	require.NoError(t, err)

	request := func(path string, body []byte, contentLength int64) int {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.ContentLength = contentLength
		req.Header.Set("X-Mattermost-ServerID", testServerID)
		req.Header.Set("X-Mattermost-Auth", "valid-token")
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, req)
		return response.Code
	}

	require.Equal(t, http.StatusBadRequest, request("/api/v1/ack?token=secret", nil, 0))
	oversized := bytes.Repeat([]byte{'x'}, maxPushRequestBytes+1)
	require.Equal(t, http.StatusRequestEntityTooLarge, request("/api/v1/send_push", oversized, int64(len(oversized))))
	require.Equal(t, http.StatusRequestEntityTooLarge, request("/api/v1/send_push", oversized, -1))
	require.Empty(t, forwarded)
}

func TestGatewayFailsClosedOnVerifierError(t *testing.T) {
	forwarded := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		forwarded <- struct{}{}
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	gateway, err := NewGateway(upstreamURL, testVerifier{err: errors.New("database unavailable")})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ack", nil)
	req.Header.Set("X-Mattermost-ServerID", testServerID)
	req.Header.Set("X-Mattermost-Auth", "valid-token")
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, req)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.NotContains(t, response.Body.String(), "database unavailable")
	require.Empty(t, forwarded)
}

func TestGatewayStopsWaitingForUnresponsiveVerifier(t *testing.T) {
	forwarded := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		forwarded <- struct{}{}
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	verifier := verifierFunc(func(ctx context.Context, _, _ string) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	})
	gateway, err := newGateway(upstreamURL, verifier, 50*time.Millisecond)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ack", nil)
	req.Header.Set("X-Mattermost-ServerID", testServerID)
	req.Header.Set("X-Mattermost-Auth", "valid-token")
	response := httptest.NewRecorder()
	started := time.Now()
	gateway.ServeHTTP(response, req)
	require.Less(t, time.Since(started), 2*time.Second)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Empty(t, forwarded)
}

func TestGatewayRejectsLateSuccessfulVerification(t *testing.T) {
	forwarded := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		forwarded <- struct{}{}
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	verifier := verifierFunc(func(ctx context.Context, _, _ string) (bool, error) {
		<-ctx.Done()
		return true, nil
	})
	gateway, err := newGateway(upstreamURL, verifier, 50*time.Millisecond)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ack", nil)
	req.Header.Set("X-Mattermost-ServerID", testServerID)
	req.Header.Set("X-Mattermost-Auth", "valid-token")
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, req)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Empty(t, forwarded)
}

func TestGatewayStopsWaitingForUnresponsiveUpstream(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	defer close(release)
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	gateway, err := newGateway(upstreamURL, testVerifier{}, 50*time.Millisecond)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ack", nil)
	req.Header.Set("X-Mattermost-ServerID", testServerID)
	req.Header.Set("X-Mattermost-Auth", "valid-token")
	response := httptest.NewRecorder()
	started := time.Now()
	gateway.ServeHTTP(response, req)
	require.Less(t, time.Since(started), 2*time.Second)
	require.Equal(t, http.StatusBadGateway, response.Code)
	require.Contains(t, response.Body.String(), "push upstream unavailable")
}

func TestGatewayDoesNotRelayUpstreamRedirect(t *testing.T) {
	var redirectedRequests atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get("X-Mattermost-Auth"))
		http.Redirect(w, r, redirectTarget.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	gateway, err := NewGateway(upstreamURL, testVerifier{})
	require.NoError(t, err)
	gatewayServer := httptest.NewServer(gateway)
	defer gatewayServer.Close()

	req, err := http.NewRequest(http.MethodPost, gatewayServer.URL+"/api/v1/send_push", bytes.NewBufferString("{}"))
	require.NoError(t, err)
	req.Header.Set("X-Mattermost-ServerID", testServerID)
	req.Header.Set("X-Mattermost-Auth", "valid-token")
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusBadGateway, response.StatusCode)
	require.Empty(t, response.Header.Get("Location"))
	require.Zero(t, redirectedRequests.Load())
}

func TestGatewayRejectsIncompleteConfiguration(t *testing.T) {
	_, err := NewGateway(nil, testVerifier{})
	require.Error(t, err)
	_, err = NewGateway(&url.URL{Scheme: "http", Host: "localhost", User: url.User("secret")}, testVerifier{})
	require.Error(t, err)
	_, err = NewGateway(&url.URL{Scheme: "http", Host: "localhost", Path: "/proxy"}, testVerifier{})
	require.Error(t, err, "an upstream path would silently change the push routes")
	_, err = NewGateway(&url.URL{Scheme: "http", Host: "localhost", ForceQuery: true}, testVerifier{})
	require.Error(t, err, "an upstream query must not change forwarded requests")
	_, err = NewGateway(&url.URL{Scheme: "http", Host: "localhost"}, nil)
	require.Error(t, err)
}

// Run with WORKSPACE_PUSH_GATEWAY_TEST_UPSTREAM=http://127.0.0.1:8066 to
// check compatibility with a locally running push proxy. An incomplete ACK
// cannot acknowledge a real delivery or trigger a device notification.
func TestGatewayAgainstLocalPushProxy(t *testing.T) {
	upstreamValue := os.Getenv("WORKSPACE_PUSH_GATEWAY_TEST_UPSTREAM")
	if upstreamValue == "" {
		t.Skip("local push proxy is not configured")
	}
	upstream, err := url.Parse(upstreamValue)
	require.NoError(t, err)
	require.Equal(t, "http", upstream.Scheme)
	require.Equal(t, "127.0.0.1", upstream.Hostname())
	gateway, err := NewGateway(upstream, testVerifier{})
	require.NoError(t, err)
	client := &http.Client{Timeout: 3 * time.Second}
	direct, err := client.Post(upstreamValue+"/api/v1/ack", "application/json", bytes.NewBufferString("{}"))
	require.NoError(t, err)
	defer direct.Body.Close()
	directBody, err := io.ReadAll(direct.Body)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/ack", bytes.NewBufferString("{}"))
	req.Header.Set("X-Mattermost-ServerID", testServerID)
	req.Header.Set("X-Mattermost-Auth", "valid-token")
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, req)
	require.Equal(t, direct.StatusCode, response.Code)
	require.JSONEq(t, string(directBody), response.Body.String())
}
