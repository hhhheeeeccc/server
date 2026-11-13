package push_proxy

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestSQLTokenStoreRequiresKeyAndDatabase(t *testing.T) {
	_, err := NewSQLTokenStore(nil, make([]byte, 32))
	require.Error(t, err)
	_, err = NewSQLTokenStore(&sql.DB{}, make([]byte, 16))
	require.Error(t, err)
}

func TestSQLTokenStoreLifecycle(t *testing.T) {
	dsn := os.Getenv("WORKSPACE_PUSH_PROXY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set WORKSPACE_PUSH_PROXY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	ctx := context.Background()
	suffix := make([]byte, 8)
	_, err = rand.Read(suffix)
	require.NoError(t, err)
	schema := "workspace_push_test_" + hex.EncodeToString(suffix)
	_, err = db.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	defer db.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`)
	// A dedicated single connection keeps search_path scoped to this test.
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `SET search_path TO `+schema)
	require.NoError(t, err)
	key := make([]byte, 32)
	_, err = rand.Read(key)
	require.NoError(t, err)
	store, err := NewSQLTokenStore(db, key)
	require.NoError(t, err)
	require.NoError(t, store.Init(ctx))
	// A second connection pool models another server process using the same
	// database and encryption key, without sharing in-memory store state.
	secondURL, err := url.Parse(dsn)
	require.NoError(t, err)
	query := secondURL.Query()
	query.Set("options", "-csearch_path="+schema)
	secondURL.RawQuery = query.Encode()
	secondDB, err := sql.Open("postgres", secondURL.String())
	require.NoError(t, err)
	defer secondDB.Close()
	otherNode, err := NewSQLTokenStore(secondDB, key)
	require.NoError(t, err)
	require.NoError(t, otherNode.Init(ctx))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	gateway, err := NewGateway(upstreamURL, store)
	require.NoError(t, err)
	requestStatus := func(token string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/send_push", nil)
		req.Header.Set("X-Mattermost-ServerID", testServerID)
		req.Header.Set("X-Mattermost-Auth", token)
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, req)
		return response.Code
	}
	start := make(chan struct{})
	initialErrors := make(chan error, 2)
	for _, node := range []*SQLTokenStore{store, otherNode} {
		go func(node *SQLTokenStore) {
			<-start
			initialErrors <- node.EnsureInitialToken(ctx)
		}(node)
	}
	close(start)
	require.NoError(t, <-initialErrors)
	require.NoError(t, <-initialErrors)
	first, err := store.Current(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, first)
	require.NoError(t, otherNode.EnsureInitialToken(ctx))
	current, err := store.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, first, current)
	current, err = otherNode.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, first, current)
	wrongKey := make([]byte, 32)
	_, err = rand.Read(wrongKey)
	require.NoError(t, err)
	otherStore, err := NewSQLTokenStore(db, wrongKey)
	require.NoError(t, err)
	require.Error(t, otherStore.EnsureInitialToken(ctx))
	current, err = store.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, first, current)
	var previousHash []byte
	require.NoError(t, db.QueryRowContext(ctx, `SELECT PreviousHash FROM WorkspacePushProxyTokens WHERE Id=1`).Scan(&previousHash))
	require.Empty(t, previousHash, "concurrent initialization must not rotate the token")
	_, err = otherStore.Generate(ctx)
	require.Error(t, err, "a node with a different key must not rotate the shared token")
	current, err = store.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, first, current)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT PreviousHash FROM WorkspacePushProxyTokens WHERE Id=1`).Scan(&previousHash))
	require.Empty(t, previousHash, "failed rotation must leave no previous token")
	accepted, err := store.Verify(ctx, testServerID, first)
	require.NoError(t, err)
	require.True(t, accepted)
	require.Equal(t, http.StatusAccepted, requestStatus(first))
	require.Equal(t, http.StatusUnauthorized, requestStatus("wrong"))
	accepted, err = store.Verify(ctx, testServerID, "wrong")
	require.NoError(t, err)
	require.False(t, accepted)

	second, err := store.Generate(ctx)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	current, err = otherNode.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, second, current)
	accepted, err = store.Verify(ctx, testServerID, first)
	require.NoError(t, err)
	require.True(t, accepted)
	require.Equal(t, http.StatusAccepted, requestStatus(first))
	require.Equal(t, http.StatusAccepted, requestStatus(second))
	accepted, err = store.Verify(ctx, testServerID, second)
	require.NoError(t, err)
	require.True(t, accepted)
	_, err = store.Generate(ctx)
	require.ErrorIs(t, err, errRotationPending)
	require.Error(t, otherStore.ConfirmRotation(ctx), "a node with a different key must not revoke the old token")
	accepted, err = store.Verify(ctx, testServerID, first)
	require.NoError(t, err)
	require.True(t, accepted)
	require.Error(t, otherStore.Delete(ctx), "a node with a different key must not delete the shared token")
	current, err = store.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, second, current)
	require.NoError(t, otherNode.ConfirmRotation(ctx))
	require.ErrorIs(t, store.ConfirmRotation(ctx), errNoRotationPending)
	accepted, err = store.Verify(ctx, testServerID, first)
	require.NoError(t, err)
	require.False(t, accepted)
	require.Equal(t, http.StatusUnauthorized, requestStatus(first))
	require.NoError(t, store.Delete(ctx))
	current, err = store.Current(ctx)
	require.NoError(t, err)
	require.Empty(t, current)
	accepted, err = store.Verify(ctx, testServerID, second)
	require.NoError(t, err)
	require.False(t, accepted)
	require.Equal(t, http.StatusUnauthorized, requestStatus(second))
	require.NoError(t, store.Delete(ctx), "deleting an absent token remains idempotent")
}
