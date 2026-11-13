package intune

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func keyTestResponse(t *testing.T, request *http.Request, key *rsa.PublicKey) *http.Response {
	t.Helper()
	var payload any = map[string]string{"issuer": issuerFor(fixtureTenant, "2.0"), "jwks_uri": "https://login.microsoftonline.com/keys"}
	if strings.HasSuffix(request.URL.Path, "/keys") {
		payload = map[string]any{"keys": []map[string]string{{
			"kid": "new-key", "kty": "RSA", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB",
		}}}
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}
}

func TestIntuneColdKeyCacheWaitsForCurrentRefresh(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(finish)
	var requests atomic.Int32
	s := &Service{keys: make(map[string]*keyCache), client: &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return keyTestResponse(t, r, &key.PublicKey), nil
	})}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ownerDone, followerDone := make(chan error, 1), make(chan error, 1)
	getKey := func(done chan<- error) {
		got, err := s.signingKey(ctx, fixtureTenant, "2.0", "new-key")
		if err == nil && (got == nil || got.N.Cmp(key.N) != 0) {
			err = errKeyService
		}
		done <- err
	}
	go getKey(ownerDone)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("refresh did not start")
	}
	go getKey(followerDone)
	select {
	case err := <-followerDone:
		t.Fatalf("a cold-cache follower returned before the current refresh completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	finish()
	require.NoError(t, <-ownerDone)
	require.NoError(t, <-followerDone)
	require.EqualValues(t, 2, requests.Load(), "one shared discovery/JWKS fetch")
}

func TestIntuneCanceledKeyRefreshCanBeRetriedImmediately(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	started := make(chan struct{})
	var requests atomic.Int32
	s := &Service{keys: make(map[string]*keyCache), client: &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return keyTestResponse(t, r, &key.PublicKey), nil
	})}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.signingKey(ctx, fixtureTenant, "2.0", "new-key"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("canceled request did not finish")
	}
	got, err := s.signingKey(context.Background(), fixtureTenant, "2.0", "new-key")
	require.NoError(t, err, "canceling one client must not poison discovery for five minutes")
	require.NotNil(t, got)
	require.EqualValues(t, 3, requests.Load())
}
