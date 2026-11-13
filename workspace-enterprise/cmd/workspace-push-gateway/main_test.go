package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/mattermost/enterprise/push_proxy"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestRejectsUnknownCommand(t *testing.T) {
	require.ErrorContains(t, run([]string{"rotate"}), "usage:")
}

func TestConfirmRotationCommand(t *testing.T) {
	dsn := os.Getenv("WORKSPACE_PUSH_PROXY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set WORKSPACE_PUSH_PROXY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	suffix := make([]byte, 8)
	_, err = rand.Read(suffix)
	require.NoError(t, err)
	schema := "workspace_push_command_test_" + hex.EncodeToString(suffix)
	_, err = db.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	defer db.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`)

	testURL, err := url.Parse(dsn)
	require.NoError(t, err)
	query := testURL.Query()
	query.Set("options", "-csearch_path="+schema)
	testURL.RawQuery = query.Encode()
	isolatedDB, err := sql.Open("postgres", testURL.String())
	require.NoError(t, err)
	defer isolatedDB.Close()
	key := make([]byte, 32)
	_, err = rand.Read(key)
	require.NoError(t, err)
	store, err := push_proxy.NewSQLTokenStore(isolatedDB, key)
	require.NoError(t, err)
	require.NoError(t, store.Init(ctx))
	first, err := store.Generate(ctx)
	require.NoError(t, err)
	second, err := store.Generate(ctx)
	require.NoError(t, err)

	t.Setenv("WORKSPACE_PUSH_GATEWAY_DATABASE_URL", testURL.String())
	t.Setenv("WORKSPACE_PUSH_GATEWAY_KEY", base64.RawURLEncoding.EncodeToString(key))
	t.Setenv("WORKSPACE_PUSH_GATEWAY_LISTEN", "")
	t.Setenv("WORKSPACE_PUSH_GATEWAY_UPSTREAM", "")
	wrongKey := make([]byte, 32)
	_, err = rand.Read(wrongKey)
	require.NoError(t, err)
	t.Setenv("WORKSPACE_PUSH_GATEWAY_KEY", base64.RawURLEncoding.EncodeToString(wrongKey))
	require.ErrorContains(t, run([]string{"confirm-rotation"}), "verify current push token")
	t.Setenv("WORKSPACE_PUSH_GATEWAY_LISTEN", "127.0.0.1:0")
	t.Setenv("WORKSPACE_PUSH_GATEWAY_UPSTREAM", "http://127.0.0.1:8066")
	require.ErrorContains(t, run(nil), "verify current push token", "serving must reject a key that cannot decrypt the existing token")
	t.Setenv("WORKSPACE_PUSH_GATEWAY_LISTEN", "")
	t.Setenv("WORKSPACE_PUSH_GATEWAY_UPSTREAM", "")
	accepted, err := store.Verify(ctx, model.NewId(), first)
	require.NoError(t, err)
	require.True(t, accepted)
	t.Setenv("WORKSPACE_PUSH_GATEWAY_KEY", base64.RawURLEncoding.EncodeToString(key))
	require.NoError(t, run([]string{"confirm-rotation"}))
	require.ErrorContains(t, run([]string{"confirm-rotation"}), "no push token rotation")
	serverID := model.NewId()
	accepted, err = store.Verify(ctx, serverID, first)
	require.NoError(t, err)
	require.False(t, accepted)
	accepted, err = store.Verify(ctx, serverID, second)
	require.NoError(t, err)
	require.True(t, accepted)
}
