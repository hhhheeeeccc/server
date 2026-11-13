package user_no_email

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

func TestCustomStatusCanceledWhileWaitingForDatabaseConnection(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	})
	db := th.Store.GetInternalMasterDB()
	user := th.CreateUser(t)
	_, err := db.Exec(`UPDATE users SET email='' WHERE id=$1`, user.Id)
	require.NoError(t, err)
	th.App.InvalidateCacheForUser(user.Id)
	warmed, appErr := th.App.GetUser(th.Context, user.Id)
	require.Nil(t, appErr)
	require.Empty(t, warmed.Email)

	previousLimit := db.Stats().MaxOpenConnections
	db.SetMaxOpenConns(1)
	defer db.SetMaxOpenConns(previousLimit)
	blocker, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer blocker.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &web.Context{App: th.App, AppContext: th.Context.WithContext(ctx)}
	r := httptest.NewRequest(http.MethodPut, "/custom-status", nil).WithContext(ctx)
	service := &Service{app: th.App, schemaReady: true}
	waitCount := db.Stats().WaitCount
	done := make(chan *model.AppError, 1)
	go func() {
		done <- service.withValidEmail(c, r, user.Id, func() *model.AppError {
			t.Error("canceled request must not apply the user update")
			return nil
		})
	}()
	require.Eventually(t, func() bool { return db.Stats().WaitCount > waitCount }, 5*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case result := <-done:
		require.NotNil(t, result)
		require.ErrorIs(t, result, context.Canceled)
	case <-time.After(time.Second):
		t.Error("canceled custom status request kept waiting for a database connection")
		// Release the fixture even before the repair so no waiter survives.
		require.NoError(t, blocker.Close())
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("request did not finish after releasing the test connection")
		}
	}
}

func TestCustomStatusCancellationStillRestoresEmailAndReleasesConnection(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	})
	db := th.Store.GetInternalMasterDB()
	user := th.CreateUser(t)
	_, err := db.Exec(`UPDATE users SET email='' WHERE id=$1`, user.Id)
	require.NoError(t, err)
	th.App.InvalidateCacheForUser(user.Id)
	previousLimit := db.Stats().MaxOpenConnections
	db.SetMaxOpenConns(4)
	defer db.SetMaxOpenConns(previousLimit)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &web.Context{App: th.App, AppContext: th.Context.WithContext(ctx)}
	r := httptest.NewRequest(http.MethodPut, "/custom-status", nil).WithContext(ctx)
	service := &Service{app: th.App, schemaReady: true}
	called := false
	appErr := service.withValidEmail(c, r, user.Id, func() *model.AppError {
		called = true
		var email string
		require.NoError(t, db.QueryRow(`SELECT email FROM users WHERE id=$1`, user.Id).Scan(&email))
		require.True(t, isCustomStatusPlaceholder(email))
		cancel()
		return nil
	})
	require.Nil(t, appErr)
	require.True(t, called)

	// Cleanup must release its reserved connection as well as the transaction.
	db.SetMaxOpenConns(1)
	check, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	var email string
	require.NoError(t, db.QueryRowContext(check, `SELECT email FROM users WHERE id=$1`, user.Id).Scan(&email))
	require.Empty(t, email)
}

func TestCustomStatusLockWaitersDoNotExhaustSmallConnectionPool(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	})
	db := th.Store.GetInternalMasterDB()
	users := []*model.User{th.CreateUser(t), th.CreateUser(t)}
	for _, user := range users {
		_, err := db.Exec(`UPDATE users SET email='' WHERE id=$1`, user.Id)
		require.NoError(t, err)
		th.App.InvalidateCacheForUser(user.Id)
		_, appErr := th.App.GetUser(th.Context, user.Id)
		require.Nil(t, appErr)
	}
	previousLimit := db.Stats().MaxOpenConnections
	db.SetMaxOpenConns(2)
	defer db.SetMaxOpenConns(previousLimit)
	blocker, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer blocker.Rollback()
	_, err = blocker.Exec(`SELECT pg_advisory_xact_lock($1)`, customStatusEmailLockKey)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	service := &Service{app: th.App, schemaReady: true}
	done := make(chan *model.AppError, len(users))
	for _, user := range users {
		go func() {
			c := &web.Context{App: th.App, AppContext: th.Context.WithContext(ctx)}
			r := httptest.NewRequest(http.MethodPut, "/custom-status", nil).WithContext(ctx)
			done <- service.withValidEmail(c, r, user.Id, func() *model.AppError { return nil })
		}()
	}
	// As in the concurrent-update integration case, hold the shared recovery
	// guard briefly so both operations enter their connection/lock waits.
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, blocker.Rollback())
	var results []*model.AppError
	for range users {
		select {
		case result := <-done:
			results = append(results, result)
		case <-time.After(5 * time.Second):
			t.Fatal("custom status operations remained stuck after releasing the recovery lock")
		}
	}
	for _, result := range results {
		require.Nil(t, result, "lock waiters must leave a connection available for the lock owner's user reads and restoration")
	}
	for _, user := range users {
		var email string
		require.NoError(t, db.QueryRow(`SELECT email FROM users WHERE id=$1`, user.Id).Scan(&email))
		require.Empty(t, email)
	}
}

func TestCustomStatusPlaceholderDoesNotReplaceAnEmail(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	})
	ctx := context.Background()
	user, _, err := th.SystemAdminClient.CreateUser(ctx, &model.User{
		Email:    "custom-status-real-" + model.NewId() + "@example.com",
		Username: "custom-status-real-" + model.NewId(),
		Password: "Test-password1",
	})
	require.NoError(t, err)

	err = (&Service{app: th.App}).setCustomStatusPlaceholder(ctx, user.Id, customStatusPlaceholderPrefix+model.NewId()+"@users.invalid")
	require.True(t, errors.Is(err, errCustomStatusEmailChanged))
	var email string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRowContext(ctx, `SELECT email FROM users WHERE id=$1`, user.Id).Scan(&email))
	require.Equal(t, user.Email, email)
}

func TestCustomStatusRecoveryWaitsForActiveUpdate(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	})
	ctx := context.Background()
	user, _, err := th.SystemAdminClient.CreateUser(ctx, &model.User{
		Email:    "custom-status-recovery-" + model.NewId() + "@example.com",
		Username: "custom-status-recovery-" + model.NewId(),
		Password: "Test-password1",
	})
	require.NoError(t, err)

	placeholder := customStatusPlaceholderPrefix + model.NewId() + "@users.invalid"
	db := th.Store.GetInternalMasterDB()
	_, err = db.ExecContext(ctx, `UPDATE users SET email=$2 WHERE id=$1`, user.Id, placeholder)
	require.NoError(t, err)

	active, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = active.Rollback() })
	_, err = active.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, customStatusEmailLockKey)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		(&Service{app: th.App}).clearStaleCustomStatusPlaceholderEmails()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("recovery returned while the update lock was held")
	case <-time.After(100 * time.Millisecond):
	}
	var email string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT email FROM users WHERE id=$1`, user.Id).Scan(&email))
	require.Equal(t, placeholder, email)

	require.NoError(t, active.Rollback())
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("recovery did not finish after the update lock was released")
	}
	require.NoError(t, db.QueryRowContext(ctx, `SELECT email FROM users WHERE id=$1`, user.Id).Scan(&email))
	require.Empty(t, email)
}

func TestConcurrentCustomStatusUpdatesRestoreEmptyEmail(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.TeamSettings.EnableCustomUserStatuses = new(true)
	})
	th.App.Srv().SetLicense(model.NewTestLicense())
	ctx := context.Background()
	user, _, err := th.SystemAdminClient.CreateUser(ctx, &model.User{
		Username: "concurrent-status-" + model.NewId(),
		Password: "Test-password1",
	})
	require.NoError(t, err)

	db := th.Store.GetInternalMasterDB()
	active, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = active.Rollback() })
	_, err = active.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, customStatusEmailLockKey)
	require.NoError(t, err)

	type result struct {
		status int
		err    error
	}
	results := make(chan result, 2)
	for _, statusText := range []string{"first", "second"} {
		client := th.CreateClient()
		_, _, err = client.Login(ctx, user.Username, "Test-password1")
		require.NoError(t, err)
		go func() {
			_, response, updateErr := client.UpdateUserCustomStatus(ctx, "me", &model.CustomStatus{Text: statusText})
			code := 0
			if response != nil {
				code = response.StatusCode
			}
			results <- result{code, updateErr}
		}()
	}
	// Both requests see the empty email before either can take the lock.
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, active.Rollback())
	for range 2 {
		select {
		case got := <-results:
			require.NoError(t, got.err)
			require.Equal(t, 200, got.status)
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent custom status update did not finish")
		}
	}
	var email string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT email FROM users WHERE id=$1`, user.Id).Scan(&email))
	require.Empty(t, email)
}
