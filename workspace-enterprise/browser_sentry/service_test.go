package browser_sentry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	app.RegisterComplianceInterface(func(a *app.App) einterfaces.ComplianceInterface { Register(a); return nil })
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestPolicyValidation(t *testing.T) {
	require.NoError(t, validate(Policy{Environment: "production"}))
	require.Error(t, validate(Policy{Environment: "production", Enabled: true}))
	require.Error(t, validate(Policy{Environment: "production", Enabled: true, DSN: "http://key@example.test/1"}))
	require.Error(t, validate(Policy{Environment: "production", Enabled: true, DSN: "https://key:secret@example.test/1"}))
	require.Error(t, validate(Policy{Environment: "production", TracesSampleRate: 1.1}))
	require.NoError(t, validate(Policy{Environment: "staging", Enabled: true, DSN: "https://key@example.test/1", TracesSampleRate: 0.1}))
}

func TestPolicyAPIAndPublicConfig(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicense()
	require.True(t, th.Server.SetLicense(license))
	ctx := context.Background()

	denied, err := th.Client.DoAPIGet(ctx, "/workspace/browser_sentry/policy", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, denied.StatusCode)
	denied.Body.Close()

	response, err := th.SystemAdminClient.DoAPIGet(ctx, "/workspace/browser_sentry/policy", "")
	require.NoError(t, err)
	var current Policy
	require.NoError(t, json.NewDecoder(response.Body).Decode(&current))
	response.Body.Close()
	require.False(t, current.Enabled)
	for _, oversized := range []string{
		`{"environment":"` + strings.Repeat("a", 4096) + `"}`,
		`{"environment":"production"}` + strings.Repeat(" ", 4096),
	} {
		response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut,
			"/workspace/browser_sentry/policy", oversized, nil)
		require.Error(t, err)
		require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
	storedBeforeSave, err := th.SystemAdminClient.DoAPIGet(ctx, "/workspace/browser_sentry/policy", "")
	require.NoError(t, err)
	var unchanged Policy
	require.NoError(t, json.NewDecoder(storedBeforeSave.Body).Decode(&unchanged))
	require.NoError(t, storedBeforeSave.Body.Close())
	require.Equal(t, current, unchanged)

	requested := current
	requested.Enabled = true
	requested.DSN = "https://public-key@example.test/1"
	requested.Environment = "staging"
	requested.TracesSampleRate = 0.2
	requested.EnableSessionReplay = true
	body, err := json.Marshal(requested)
	require.NoError(t, err)
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut,
		"/workspace/browser_sentry/policy", string(body), nil)
	require.NoError(t, err)
	var saved Policy
	require.NoError(t, json.NewDecoder(response.Body).Decode(&saved))
	response.Body.Close()
	require.Equal(t, current.Version+1, saved.Version)

	response, err = th.Client.DoAPIGet(ctx, "/workspace/browser_sentry/config", "")
	require.NoError(t, err)
	var public Policy
	require.NoError(t, json.NewDecoder(response.Body).Decode(&public))
	response.Body.Close()
	require.Equal(t, saved.DSN, public.DSN)
	require.True(t, public.EnableSessionReplay)

	conflict, err := th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut,
		"/workspace/browser_sentry/policy", string(body), nil)
	require.Error(t, err)
	require.Equal(t, http.StatusConflict, conflict.StatusCode)
	conflict.Body.Close()

	invalid := saved
	invalid.DSN = "http://public-key@example.test/1"
	invalidBody, err := json.Marshal(invalid)
	require.NoError(t, err)
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut,
		"/workspace/browser_sentry/policy", string(invalidBody), nil)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	response.Body.Close()

	disabledPolicy := saved
	disabledPolicy.Enabled = false
	disabledBody, err := json.Marshal(disabledPolicy)
	require.NoError(t, err)
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut,
		"/workspace/browser_sentry/policy", string(disabledBody), nil)
	require.NoError(t, err)
	response.Body.Close()
	response, err = th.Client.DoAPIGet(ctx, "/workspace/browser_sentry/config", "")
	require.NoError(t, err)
	var hidden Policy
	require.NoError(t, json.NewDecoder(response.Body).Decode(&hidden))
	response.Body.Close()
	require.False(t, hidden.Enabled)
	require.Empty(t, hidden.DSN)

	th.Server.SetLicense(nil)
	response, err = th.Client.DoAPIGet(ctx, "/workspace/browser_sentry/config", "")
	require.NoError(t, err)
	var disabled Policy
	require.NoError(t, json.NewDecoder(response.Body).Decode(&disabled))
	response.Body.Close()
	require.False(t, disabled.Enabled)
	require.Empty(t, disabled.DSN)

	require.True(t, th.Server.SetLicense(license))
	db := th.Store.GetInternalMasterDB()
	_, err = db.ExecContext(ctx, `DELETE FROM WorkspaceBrowserSentryPolicy WHERE Id=1`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, restoreErr := db.ExecContext(context.Background(), `INSERT INTO WorkspaceBrowserSentryPolicy (Id, Policy, Version)
			VALUES (1, $1, $2) ON CONFLICT (Id) DO NOTHING`, string(disabledBody), saved.Version+1)
		require.NoError(t, restoreErr)
	})

	unavailableResponse, err := th.Client.DoAPIGet(ctx, "/workspace/browser_sentry/config", "")
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, unavailableResponse.StatusCode)
	unavailableResponse.Body.Close()

	unavailableResponse, err = th.SystemAdminClient.DoAPIGet(ctx, "/workspace/browser_sentry/policy", "")
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, unavailableResponse.StatusCode)
	unavailableResponse.Body.Close()
}

func TestPolicyReadAndSaveHaveDeadlinesWhenDatabasePoolIsExhausted(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	})
	db, err := sql.Open("postgres", *th.App.Config().SqlSettings.DataSource)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.Ping())
	held, err := db.Conn(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, held.Close()) })

	s := &Service{app: th.App, db: db}
	saveResult := make(chan error, 1)
	go func() {
		_, saveErr := s.savePolicy(context.Background(), Policy{Environment: "production"})
		saveResult <- saveErr
	}()
	start := time.Now()
	_, readErr := s.read(context.Background())
	require.True(t, errors.Is(readErr, context.DeadlineExceeded))
	require.Less(t, time.Since(start), 7*time.Second)
	select {
	case saveErr := <-saveResult:
		require.True(t, errors.Is(saveErr, context.DeadlineExceeded))
	case <-time.After(2 * time.Second):
		t.Fatal("browser Sentry policy save did not respect its deadline")
	}
}

func TestPolicyWithBinaryParameters(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	parsed, err := url.Parse(*th.App.Config().SqlSettings.DataSource)
	require.NoError(t, err)
	query := parsed.Query()
	query.Set("binary_parameters", "yes")
	parsed.RawQuery = query.Encode()
	binaryDB, err := sql.Open("postgres", parsed.String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, binaryDB.Close()) })
	require.NoError(t, binaryDB.Ping())

	s := &Service{app: th.App, db: binaryDB}
	ctx := context.Background()
	require.NoError(t, s.initPolicy(ctx))
	current, err := s.read(ctx)
	require.NoError(t, err)
	updated := current
	updated.TracesSampleRate = 0.5
	saved, err := s.savePolicy(ctx, updated)
	require.NoError(t, err)
	require.Equal(t, current.Version+1, saved.Version)
	stored, err := s.read(ctx)
	require.NoError(t, err)
	require.Equal(t, 0.5, stored.TracesSampleRate)
	t.Cleanup(func() {
		latest, readErr := s.read(context.Background())
		if readErr == nil {
			current.Version = latest.Version
			_, _ = s.savePolicy(context.Background(), current)
		}
	})
}
