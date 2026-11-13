package step_up_mfa

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dgryski/dgoogauth"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/config"
	"github.com/stretchr/testify/require"
)

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
	require.NoError(t, s.initPolicy(ctx, loadPolicySettings()))
	current, err := s.readPolicy(ctx)
	require.NoError(t, err)
	updated := current
	updated.GraceMinutes = (current.GraceMinutes + 1) % 1441
	saved, err := s.savePolicy(ctx, updated)
	require.NoError(t, err)
	require.Equal(t, current.Version+1, saved.Version)
	stored, err := s.readPolicy(ctx)
	require.NoError(t, err)
	require.Equal(t, updated.GraceMinutes, stored.GraceMinutes)
	t.Cleanup(func() {
		latest, readErr := s.readPolicy(context.Background())
		if readErr == nil {
			current.Version = latest.Version
			_, _ = s.savePolicy(context.Background(), current)
		}
	})
}

func TestPolicyReadAndSaveHaveDeadlinesWhenDatabasePoolIsExhausted(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))

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
		_, saveErr := s.savePolicy(context.Background(), PolicyResponse{PolicySettings: loadPolicySettings(), Version: 0})
		saveResult <- saveErr
	}()
	request := httptest.NewRequest(http.MethodDelete, "/api/v4/teams/"+model.NewId(), nil)
	request.Header.Set("Authorization", "Bearer "+th.SystemAdminClient.AuthToken)
	called := false
	response := httptest.NewRecorder()
	start := time.Now()
	s.enforce(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(response, request)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.False(t, called)
	require.Less(t, time.Since(start), 7*time.Second)
	select {
	case saveErr := <-saveResult:
		require.ErrorIs(t, saveErr, context.DeadlineExceeded)
	case <-time.After(2 * time.Second):
		t.Fatal("policy save did not respect its deadline")
	}
}

func TestLegacyProcessGateMigratesOnceToSharedPolicy(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	s := &Service{app: th.App, db: th.Store.GetInternalMasterDB()}
	ctx := th.Context.Context()
	legacy := PolicySettings{
		TeamDeletion: true, UserDeletion: true, UserPasswordChange: true,
		UserMfaRemoval: true, ConfigDisable: true, GraceMinutes: 30,
	}
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	restoreLegacy := func() {
		_, execErr := s.db.ExecContext(ctx, `UPDATE WorkspaceStepUpMFAPolicy
			SET Policy=$1, Version=0, MigrationVersion=0 WHERE Id=1`, string(raw))
		require.NoError(t, execErr)
	}

	restoreLegacy()
	t.Setenv("WORKSPACE_STEP_UP_MFA_ENABLED", "false")
	require.NoError(t, s.initPolicy(ctx, legacy))
	migrated, err := s.readPolicy(ctx)
	require.NoError(t, err)
	require.False(t, migrated.TeamDeletion)
	require.False(t, migrated.UserDeletion)
	require.False(t, migrated.UserPasswordChange)
	require.False(t, migrated.UserMfaRemoval)
	require.False(t, migrated.ConfigDisable)
	require.Equal(t, int64(1), migrated.Version)
	t.Setenv("WORKSPACE_STEP_UP_MFA_ENABLED", "true")
	require.NoError(t, s.initPolicy(ctx, legacy))
	again, err := s.readPolicy(ctx)
	require.NoError(t, err)
	require.Equal(t, migrated, again, "changing the old environment gate cannot override the shared policy")

	restoreLegacy()
	require.NoError(t, s.initPolicy(ctx, legacy))
	migratedOn, err := s.readPolicy(ctx)
	require.NoError(t, err)
	require.True(t, migratedOn.TeamDeletion)
	require.True(t, migratedOn.UserDeletion)
	require.Zero(t, migratedOn.Version, "an enabled legacy installation keeps its policy")
}

func TestIncompleteStoredPolicyFailsClosed(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	s := &Service{app: th.App, db: th.Store.GetInternalMasterDB()}
	ctx := th.Context.Context()
	original, err := s.readPolicy(ctx)
	require.NoError(t, err)
	raw, err := json.Marshal(original.PolicySettings)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, restoreErr := s.db.Exec(`UPDATE WorkspaceStepUpMFAPolicy SET Policy=$1::jsonb WHERE Id=1`, string(raw))
		require.NoError(t, restoreErr)
	})
	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodDelete, "/api/v4/teams/"+model.NewId(), nil)
	request.Header.Set("Authorization", "Bearer "+th.SystemAdminClient.AuthToken)
	for _, corrupted := range []string{`null`, `{}`, `{"team_deletion":true}`, `{"team_deletion":null,"user_deletion":true,"user_password_change":true,"user_mfa_removal":true,"config_disable":true,"grace_minutes":0}`} {
		_, err = s.db.ExecContext(ctx, `UPDATE WorkspaceStepUpMFAPolicy SET Policy=$1::jsonb WHERE Id=1`, corrupted)
		require.NoError(t, err)
		_, err = s.readPolicy(ctx)
		require.Error(t, err, corrupted)
		response := httptest.NewRecorder()
		s.enforce(next).ServeHTTP(response, request)
		require.Equal(t, http.StatusServiceUnavailable, response.Code, corrupted)
		require.False(t, nextCalled)
	}
}

func TestPolicyAPIRequiresPermissionAndFreshMFAAndEnforcesSavedPolicy(t *testing.T) {
	t.Setenv("WORKSPACE_STEP_UP_MFA_ENABLED", "true")
	t.Setenv("WORKSPACE_STEP_UP_MFA_GRACE_MINUTES", "5")
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	s := &Service{app: th.App, db: th.Store.GetInternalMasterDB()}
	secret := enableAdminMFA(t, th)
	ctx := th.Context.Context()

	response, err := th.Client.DoAPIGet(ctx, "/workspace/step_up_mfa/policy", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()

	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/workspace/step_up_mfa/policy", "")
	require.NoError(t, err)
	var current PolicyResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&current))
	response.Body.Close()
	require.True(t, current.UserDeletion)

	initial := current.PolicySettings
	t.Cleanup(func() {
		latest, readErr := s.readPolicy(context.Background())
		if readErr == nil {
			latest.PolicySettings = initial
			_, _ = s.savePolicy(context.Background(), latest)
		}
	})

	requested := current
	requested.TeamDeletion = false
	requested.GraceMinutes = 60
	body, err := json.Marshal(requested)
	require.NoError(t, err)

	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut,
		"/workspace/step_up_mfa/policy", string(body), nil)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()

	code := fmt.Sprintf("%06d", dgoogauth.ComputeCode(secret, time.Now().UTC().Unix()/30))
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut,
		"/workspace/step_up_mfa/policy", string(body), map[string]string{codeHeader: code})
	require.NoError(t, err)
	var saved PolicyResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&saved))
	response.Body.Close()
	require.False(t, saved.TeamDeletion)
	require.Equal(t, 60, saved.GraceMinutes)
	require.Equal(t, current.Version+1, saved.Version)

	// Changing the policy itself always needs a fresh code, even after this
	// session received a grace-period grant from the successful update.
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut,
		"/workspace/step_up_mfa/policy", string(body), nil)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()

	_, err = s.savePolicy(ctx, requested)
	require.True(t, errors.Is(err, errPolicyConflict))

	team := th.CreateTeam(t)
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodDelete, "/teams/"+team.Id, "", nil)
	require.NoError(t, err, "the saved policy should take effect without restart")
	response.Body.Close()

	// A missing policy must not silently restore the old environment defaults
	// or permit a sensitive operation.
	_, err = s.db.ExecContext(ctx, `DELETE FROM WorkspaceStepUpMFAPolicy WHERE Id=1`)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, s.initPolicy(context.Background(), initial))
	})
	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/workspace/step_up_mfa/policy", "")
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	response.Body.Close()

	secondTeam := th.CreateTeam(t)
	response, err = th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodDelete, "/teams/"+secondTeam.Id, "", nil)
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	response.Body.Close()
}

func TestParsePolicyUpdateRejectsIncompleteAndInvalidRequests(t *testing.T) {
	valid := `{"team_deletion":true,"user_deletion":true,"user_password_change":true,"user_mfa_removal":true,"config_disable":true,"grace_minutes":5,"version":1}`
	for name, body := range map[string]string{
		"partial":          `{"team_deletion":false,"version":1}`,
		"null":             strings.Replace(valid, `"team_deletion":true`, `"team_deletion":null`, 1),
		"grace too high":   strings.Replace(valid, `"grace_minutes":5`, `"grace_minutes":1441`, 1),
		"negative grace":   strings.Replace(valid, `"grace_minutes":5`, `"grace_minutes":-1`, 1),
		"negative version": strings.Replace(valid, `"version":1`, `"version":-1`, 1),
		"unknown field":    strings.Replace(valid, `"version":1`, `"version":1,"unexpected":true`, 1),
		"multiple objects": valid + valid,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parsePolicyUpdate(strings.NewReader(body))
			require.Error(t, err)
		})
	}
	parsed, err := parsePolicyUpdate(strings.NewReader(valid))
	require.NoError(t, err)
	require.True(t, parsed.TeamDeletion)
	require.Equal(t, 5, parsed.GraceMinutes)
	_, err = parsePolicyUpdate(strings.NewReader(strings.Replace(valid, `"version":1`, `"version":1,"enabled":false`, 1)))
	require.NoError(t, err, "older webapp bundles may still send the retired field")
}

func TestPolicyAPIRejectsPartialUpdateWithoutChangingSavedPolicy(t *testing.T) {
	t.Setenv("WORKSPACE_STEP_UP_MFA_ENABLED", "true")
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	s := &Service{app: th.App, db: th.Store.GetInternalMasterDB()}
	secret := enableAdminMFA(t, th)
	ctx := th.Context.Context()
	before, err := s.readPolicy(ctx)
	require.NoError(t, err)
	code := fmt.Sprintf("%06d", dgoogauth.ComputeCode(secret, time.Now().UTC().Unix()/30))
	response, err := th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut,
		"/workspace/step_up_mfa/policy", fmt.Sprintf(`{"version":%d,"team_deletion":false}`, before.Version),
		map[string]string{codeHeader: code})
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	response.Body.Close()
	after, err := s.readPolicy(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestPolicyAPIRejectsOversizedUpdateWithoutChangingSavedPolicy(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	s := &Service{app: th.App, db: th.Store.GetInternalMasterDB()}
	ctx := th.Context.Context()
	before, err := s.readPolicy(ctx)
	require.NoError(t, err)
	body, err := json.Marshal(before)
	require.NoError(t, err)
	response, err := th.SystemAdminClient.DoAPIRequestWithHeaders(ctx, http.MethodPut,
		"/workspace/step_up_mfa/policy", string(body)+strings.Repeat(" ", 4096), nil)
	require.Error(t, err)
	require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)
	require.NoError(t, response.Body.Close())
	after, err := s.readPolicy(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestPolicyChangeIsVisibleToAnotherAppInstance(t *testing.T) {
	t.Setenv("WORKSPACE_STEP_UP_MFA_ENABLED", "true")
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableMultifactorAuthentication = new(true)
	}).InitBasic(t)
	license := model.NewTestLicense()
	require.True(t, th.Server.SetLicense(license))
	enableAdminMFA(t, th)

	first := &Service{app: th.App, db: th.Store.GetInternalMasterDB()}
	ctx := th.Context.Context()
	original, err := first.readPolicy(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		latest, readErr := first.readPolicy(context.Background())
		if readErr == nil {
			latest.PolicySettings = original.PolicySettings
			_, _ = first.savePolicy(context.Background(), latest)
		}
	})

	peerConfig := th.App.Config().Clone()
	peerConfig.ServiceSettings.ListenAddress = model.NewPointer("localhost:0")
	memory, err := config.NewMemoryStoreWithOptions(&config.MemoryStoreOptions{IgnoreEnvironmentOverrides: true})
	require.NoError(t, err)
	require.NoError(t, memory.Set(peerConfig))
	configStore, err := config.NewStoreFromBacking(memory, nil, false)
	require.NoError(t, err)
	peerServer, err := app.NewServer(app.ConfigStore(configStore))
	require.NoError(t, err)
	t.Cleanup(func() { peerServer.Shutdown(); _ = configStore.Close() })
	_, err = api4.Init(peerServer)
	require.NoError(t, err)
	require.True(t, peerServer.SetLicense(license))
	require.NoError(t, peerServer.Start())
	peer := &Service{app: app.New(app.ServerConnector(peerServer.Channels())), db: peerServer.Store().GetInternalMasterDB()}

	team := th.CreateTeam(t)
	request := httptest.NewRequest(http.MethodDelete, "/api/v4/teams/"+team.Id, nil)
	request.Header.Set("Authorization", "Bearer "+th.SystemAdminClient.AuthToken)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	serveOnPeer := func() int {
		response := httptest.NewRecorder()
		peer.enforce(next).ServeHTTP(response, request)
		return response.Code
	}

	changed := original
	changed.TeamDeletion = false
	changed.GraceMinutes = 0
	changed, err = first.savePolicy(ctx, changed)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, serveOnPeer())

	changed.TeamDeletion = true
	_, err = first.savePolicy(ctx, changed)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, serveOnPeer(), "the peer must enforce the committed policy without a restart")
}
