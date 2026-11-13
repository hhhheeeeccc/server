package outgoing_oauth_connections

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost/server/public/model"
)

func TestAudienceMatchLength(t *testing.T) {
	tests := []struct {
		name     string
		audience string
		target   string
		matches  bool
	}{
		{name: "exact", audience: "https://example.com/hook", target: "https://example.com/hook", matches: true},
		{name: "exact does not accept suffix", audience: "https://example.com/hook", target: "https://example.com/hook/extra", matches: false},
		{name: "wildcard prefix", audience: "https://example.com/hooks/*", target: "https://example.com/hooks/one", matches: true},
		{name: "wildcard rejects another path", audience: "https://example.com/hooks/*", target: "https://example.com/other", matches: false},
		{name: "wildcard rejects a suffix hostname", audience: "https://example.com*", target: "https://example.com.evil/hook", matches: false},
		{name: "wildcard rejects a different port", audience: "https://example.com:4*", target: "https://example.com:443/hook", matches: false},
		{name: "wildcard accepts same host", audience: "https://example.com*", target: "https://example.com/hook", matches: true},
		{name: "wildcard rejects a subdomain", audience: "https://example.com*", target: "https://example.com.attacker.test/hook", matches: false},
		{name: "wildcard rejects parent path", audience: "https://example.com/hooks/*", target: "https://example.com/hooks/../admin", matches: false},
		{name: "wildcard rejects encoded parent path", audience: "https://example.com/hooks/*", target: "https://example.com/hooks/%2e%2e/admin", matches: false},
		{name: "wildcard rejects current path", audience: "https://example.com/hooks/*", target: "https://example.com/hooks/./admin", matches: false},
		{name: "wildcard rejects encoded backslash path", audience: "https://example.com/hooks/*", target: "https://example.com/hooks/%5c..%5cadmin", matches: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, matches := audienceMatchLength(test.audience, test.target)
			require.Equal(t, test.matches, matches)
		})
	}
}

func TestRetrieveToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		require.Equal(t, "client_credentials", r.Form.Get("grant_type"))
		require.Equal(t, "client-id", r.Form.Get("client_id"))
		require.Equal(t, "client-secret", r.Form.Get("client_secret"))
		require.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		require.NoError(t, json.NewEncoder(w).Encode(map[string]string{
			"access_token": "access-token",
			"token_type":   "Bearer",
		}))
	}))
	defer server.Close()

	token, err := retrieveToken(context.Background(), server.Client(), &model.OutgoingOAuthConnection{
		OAuthTokenURL: server.URL,
		GrantType:     model.OutgoingOAuthConnectionGrantTypeClientCredentials,
		ClientId:      "client-id",
		ClientSecret:  "client-secret",
	})
	require.NoError(t, err)
	require.Equal(t, "Bearer access-token", token.AsHeaderValue())
}

func TestValidateTokenConnectionAllowsUnsavedConnection(t *testing.T) {
	connection := &model.OutgoingOAuthConnection{
		OAuthTokenURL: "https://example.com/oauth/token",
		GrantType:     model.OutgoingOAuthConnectionGrantTypeClientCredentials,
		ClientId:      "client-id",
		ClientSecret:  "client-secret",
	}

	require.Nil(t, validateTokenConnection(connection))
}

func TestValidateTokenConnectionRejectsIncompletePasswordGrant(t *testing.T) {
	connection := &model.OutgoingOAuthConnection{
		OAuthTokenURL: "https://example.com/oauth/token",
		GrantType:     model.OutgoingOAuthConnectionGrantTypePassword,
		ClientId:      "client-id",
		ClientSecret:  "client-secret",
	}

	require.NotNil(t, validateTokenConnection(connection))
}

func TestRetrieveTokenRejectsInvalidResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := retrieveToken(context.Background(), server.Client(), &model.OutgoingOAuthConnection{
		OAuthTokenURL: server.URL,
		GrantType:     model.OutgoingOAuthConnectionGrantTypeClientCredentials,
	})
	require.Error(t, err)
}

func TestRetrieveTokenClassifiesRejectedLargeResponseByStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(strings.Repeat("x", maxTokenResponseSize+1)))
	}))
	defer server.Close()
	_, err := retrieveToken(context.Background(), server.Client(), &model.OutgoingOAuthConnection{
		OAuthTokenURL: server.URL,
		GrantType:     model.OutgoingOAuthConnectionGrantTypeClientCredentials,
		ClientId:      "client-id",
		ClientSecret:  "client-secret",
	})
	var statusErr *tokenEndpointStatusError
	require.ErrorAs(t, err, &statusErr)
	require.Equal(t, http.StatusUnauthorized, statusErr.status)
}

func TestRetrieveTokenRedirectStaysOnConfiguredOrigin(t *testing.T) {
	var otherOriginCalled atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherOriginCalled.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer other.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect-local":
			http.Redirect(w, r, "/token", http.StatusTemporaryRedirect)
		case "/redirect-other":
			http.Redirect(w, r, other.URL+"/token", http.StatusTemporaryRedirect)
		case "/token":
			require.NoError(t, r.ParseForm())
			require.Equal(t, "client-secret", r.Form.Get("client_secret"))
			require.NoError(t, json.NewEncoder(w).Encode(map[string]string{"access_token": "token", "token_type": "Bearer"}))
		}
	}))
	defer server.Close()

	connection := &model.OutgoingOAuthConnection{
		OAuthTokenURL: server.URL + "/redirect-local",
		GrantType:     model.OutgoingOAuthConnectionGrantTypeClientCredentials,
		ClientId:      "client-id",
		ClientSecret:  "client-secret",
	}
	token, err := retrieveToken(context.Background(), server.Client(), connection)
	require.NoError(t, err)
	require.Equal(t, "Bearer token", token.AsHeaderValue())

	connection.OAuthTokenURL = server.URL + "/redirect-other"
	_, err = retrieveToken(context.Background(), server.Client(), connection)
	require.ErrorContains(t, err, "another origin")
	require.False(t, otherOriginCalled.Load())
}

func TestSanitizeConnection(t *testing.T) {
	password := "password"
	connection := &model.OutgoingOAuthConnection{ClientSecret: "secret", CredentialsPassword: &password}
	service := &Service{}
	service.SanitizeConnection(connection)
	require.Empty(t, connection.ClientSecret)
	require.Nil(t, connection.CredentialsPassword)
}
