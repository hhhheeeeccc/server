//go:build integration

package phone_password_reset

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/channels/web"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func setupPhoneRecovery(t *testing.T) (*api4.TestHelper, *Service) {
	t.Helper()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	s := &Service{app: th.App, db: th.Store.GetInternalMasterDB()}
	require.NoError(t, s.init(context.Background()))
	group, err := th.Store.PropertyGroup().Register(&model.PropertyGroup{Name: "access_control"})
	require.NoError(t, err)
	field, err := th.Store.PropertyField().Create(&model.PropertyField{
		GroupID: group.ID, Name: "Recovery test phone", Type: model.PropertyFieldTypeText, ObjectType: "user", TargetType: "system",
		Attrs: model.StringInterface{"value_type": "phone"},
	})
	require.NoError(t, err)
	_, err = th.Store.PropertyValue().Create(&model.PropertyValue{
		GroupID: group.ID, FieldID: field.ID, TargetID: th.BasicUser.Id, TargetType: "user", Value: json.RawMessage(`"737328812"`),
	})
	require.NoError(t, err)
	hash, err := bcrypt.GenerateFromPassword([]byte("123456"), bcrypt.MinCost)
	require.NoError(t, err)
	now := model.GetMillis()
	_, err = s.db.Exec(`INSERT INTO WorkspacePhonePasswordReset (Phone,ChallengeId,UserId,CodeHash,ExpiresAt,RequestedAt)
		VALUES ('737328812',$1,$2,$3,$4,$5)`, model.NewId(), th.BasicUser.Id, string(hash), now+codeLifetime.Milliseconds(), now)
	require.NoError(t, err)
	return th, s
}

func completePhoneRecovery(ctx context.Context, th *api4.TestHelper, s *Service, code string) (*web.Context, *httptest.ResponseRecorder) {
	c := &web.Context{App: th.App, AppContext: request.EmptyContext(th.App.Log()).WithContext(ctx)}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, basePath+"/complete", strings.NewReader(`{"phone":"737328812","code":"`+code+`","new_password":"Recovery123!"}`)).WithContext(ctx)
	s.complete(c, w, r)
	return c, w
}

func TestPhonePasswordRecoveryCompletesWithOneDatabaseConnection(t *testing.T) {
	th, s := setupPhoneRecovery(t)
	th.App.InvalidateCacheForUser(th.BasicUser.Id)
	previousLimit := s.db.Stats().MaxOpenConnections
	s.db.SetMaxOpenConns(1)
	defer s.db.SetMaxOpenConns(previousLimit)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, w := completePhoneRecovery(ctx, th, s, "123456")
	require.Nil(t, c.Err, "valid recovery must not wait for another connection while holding the challenge transaction")
	require.Equal(t, http.StatusOK, w.Code)
	var used bool
	require.NoError(t, s.db.QueryRow(`SELECT Used FROM WorkspacePhonePasswordReset WHERE Phone='737328812'`).Scan(&used))
	require.True(t, used)
	client := th.CreateClient()
	_, _, err := client.Login(context.Background(), th.BasicUser.Username, "Recovery123!")
	require.NoError(t, err, "the native password update must have completed")
}

func TestPhonePasswordRecoveryRejectsInvalidChallenges(t *testing.T) {
	for _, tc := range []struct {
		name, mutation, code string
		attempts             int
		used                 bool
	}{
		{name: "wrong_code", code: "654321", attempts: 1},
		{name: "expired", mutation: "UPDATE WorkspacePhonePasswordReset SET ExpiresAt=1"},
		{name: "attempt_limit", mutation: "UPDATE WorkspacePhonePasswordReset SET Attempts=5", attempts: 5},
		{name: "already_used", mutation: "UPDATE WorkspacePhonePasswordReset SET Used=true", used: true},
		{name: "phone_removed", mutation: "UPDATE PropertyValues SET DeleteAt=1"},
		{name: "account_deactivated", mutation: "UPDATE Users SET DeleteAt=1 WHERE Username=$1"},
		{name: "external_auth", mutation: "UPDATE Users SET AuthService='saml',AuthData=Id WHERE Username=$1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			th, s := setupPhoneRecovery(t)
			var oldPassword string
			require.NoError(t, s.db.QueryRow(`SELECT Password FROM Users WHERE Id=$1`, th.BasicUser.Id).Scan(&oldPassword))
			if tc.mutation != "" {
				var err error
				if strings.Contains(tc.mutation, "$1") {
					_, err = s.db.Exec(tc.mutation, th.BasicUser.Username)
				} else {
					_, err = s.db.Exec(tc.mutation)
				}
				require.NoError(t, err)
			}
			th.App.InvalidateCacheForUser(th.BasicUser.Id)
			code := tc.code
			if code == "" {
				code = "123456"
			}
			c, _ := completePhoneRecovery(context.Background(), th, s, code)
			require.NotNil(t, c.Err)
			require.Equal(t, "workspace.password.invalid_code", c.Err.Id)
			var currentPassword string
			require.NoError(t, s.db.QueryRow(`SELECT Password FROM Users WHERE Id=$1`, th.BasicUser.Id).Scan(&currentPassword))
			require.Equal(t, oldPassword, currentPassword)
			var attempts int
			var used bool
			require.NoError(t, s.db.QueryRow(`SELECT Attempts,Used FROM WorkspacePhonePasswordReset WHERE Phone='737328812'`).Scan(&attempts, &used))
			require.Equal(t, tc.attempts, attempts)
			require.Equal(t, tc.used, used)
		})
	}
}

func TestPhonePasswordRecoveryAllowsOnlyOneConcurrentUse(t *testing.T) {
	th, s := setupPhoneRecovery(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan *model.AppError, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	for range 2 {
		go func() {
			defer workers.Done()
			<-start
			c, _ := completePhoneRecovery(ctx, th, s, "123456")
			results <- c.Err
		}()
	}
	t.Cleanup(func() {
		cancel()
		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("concurrent recovery workers did not finish")
		}
	})
	close(start)
	successes := 0
	for range 2 {
		select {
		case err := <-results:
			if err == nil {
				successes++
			} else {
				require.Equal(t, "workspace.password.invalid_code", err.Id)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent recovery did not finish")
		}
	}
	require.Equal(t, 1, successes)
	_, _, err := th.CreateClient().Login(context.Background(), th.BasicUser.Username, "Recovery123!")
	require.NoError(t, err)
}

func TestPhonePasswordRecoveryRejectsChallengeReplacedWhileLoadingProfile(t *testing.T) {
	for _, changed := range []string{"challenge_account", "remote_account"} {
		t.Run(changed, func(t *testing.T) { testPhoneRecoveryChangedWhileLoadingProfile(t, changed) })
	}
}

func testPhoneRecoveryChangedWhileLoadingProfile(t *testing.T, changed string) {
	t.Helper()
	th, s := setupPhoneRecovery(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	blocker, err := s.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = blocker.ExecContext(ctx, `SELECT 1 FROM WorkspacePhonePasswordReset WHERE Phone='737328812' FOR UPDATE`)
	require.NoError(t, err)
	var oldPassword string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT Password FROM Users WHERE Id=$1`, th.BasicUser.Id).Scan(&oldPassword))
	done := make(chan struct{})
	var response *web.Context
	t.Cleanup(func() {
		cancel()
		_ = blocker.Rollback()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("recovery did not finish after releasing its row blocker")
		}
	})
	go func() {
		defer close(done)
		response, _ = completePhoneRecovery(ctx, th, s, "123456")
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		return s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			AND query LIKE 'SELECT UserId,CodeHash,ExpiresAt,Attempts,Used FROM WorkspacePhonePasswordReset%')`).Scan(&waiting) == nil && waiting
	}, 3*time.Second, 10*time.Millisecond)
	if changed == "challenge_account" {
		// Keep the code unchanged so the ID recheck, rather than a hash
		// mismatch, must reject the request holding the old profile.
		_, err = blocker.ExecContext(ctx, `UPDATE PropertyValues SET TargetId=$1 WHERE TargetId=$2`, th.BasicUser2.Id, th.BasicUser.Id)
		require.NoError(t, err)
		_, err = blocker.ExecContext(ctx, `UPDATE WorkspacePhonePasswordReset SET UserId=$1,ChallengeId=$2 WHERE Phone='737328812'`, th.BasicUser2.Id, model.NewId())
		require.NoError(t, err)
	} else {
		// The challenge ID still matches, but the previously local profile is
		// no longer eligible. Its transaction read must reject the stale copy.
		_, err = blocker.ExecContext(ctx, `UPDATE Users SET RemoteId=$1 WHERE Id=$2`, model.NewId(), th.BasicUser.Id)
		require.NoError(t, err)
	}
	require.NoError(t, blocker.Commit())
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("recovery did not finish after replacing the challenge")
	}
	require.NotNil(t, response.Err)
	require.Equal(t, "workspace.password.invalid_code", response.Err.Id)
	var used bool
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT Used FROM WorkspacePhonePasswordReset WHERE Phone='737328812'`).Scan(&used))
	require.False(t, used, "old request must not consume a different account's challenge")
	var currentPassword string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT Password FROM Users WHERE Id=$1`, th.BasicUser.Id).Scan(&currentPassword))
	require.Equal(t, oldPassword, currentPassword)
	if changed == "remote_account" {
		return
	}
	c, _ := completePhoneRecovery(ctx, th, s, "123456")
	require.Nil(t, c.Err, "the replacement account must still be able to recover")
	_, _, err = th.CreateClient().Login(context.Background(), th.BasicUser2.Username, "Recovery123!")
	require.NoError(t, err)
}
