package user_no_email

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/web"
	"github.com/stretchr/testify/require"
)

// Stop a statement before it takes a user row lock. A second connection can
// then commit a real email before the paused prepare/cleanup statement resumes.
func pauseEmailWrite(t *testing.T, db *sql.DB, key int64, pauseAt int, native bool) func() {
	t.Helper()
	name := "email_lifecycle_" + model.NewId()
	predicate := `current_query() ~* 'UPDATE[[:space:]]+users[[:space:]]+SET[[:space:]]+email[[:space:]]*='`
	if native {
		predicate = "TRUE"
	}
	_, err := db.Exec(fmt.Sprintf(`CREATE TABLE %s (calls integer NOT NULL);
        INSERT INTO %s VALUES (0);
        CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
        DECLARE n integer;
        BEGIN
          IF %s THEN
            UPDATE %s SET calls=calls+1 RETURNING calls INTO n;
            IF n=%d THEN PERFORM pg_advisory_xact_lock(%d); END IF;
          END IF;
          RETURN NULL;
        END $$`, name, name, name, predicate, name, pauseAt, key))
	require.NoError(t, err)
	columns := "email"
	if native {
		columns = "nickname"
	}
	_, err = db.Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE OF %s ON users FOR EACH STATEMENT EXECUTE FUNCTION %s()`, name, columns, name))
	require.NoError(t, err)
	blocker, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = blocker.Exec(`SELECT pg_advisory_xact_lock($1)`, key)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = blocker.Rollback()
		_, err := db.Exec(fmt.Sprintf(`DROP TRIGGER %s ON users; DROP FUNCTION %s(); DROP TABLE %s`, name, name, name))
		require.NoError(t, err)
	})
	return func() { _ = blocker.Rollback() }
}

func waitEmailWrite(t *testing.T, db *sql.DB, key int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND classid=0 AND objid=$1 AND NOT granted)`, key).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 5*time.Millisecond, "native email operation did not reach the controlled database barrier")
}

func emailMutationContext(th *api4.TestHelper, ctx context.Context, userID string) *web.Context {
	session := &model.Session{Id: model.NewId(), UserId: th.SystemAdminUser.Id, Roles: th.SystemAdminUser.Roles}
	return &web.Context{App: th.App, AppContext: th.Context.WithContext(ctx).WithSession(session),
		Params: &web.Params{UserId: userID}, Logger: th.App.Log()}
}

func TestEmptyEmailMutationPreservesNewerRealEmail(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pauseAt    int
		fullUpdate bool
	}{
		{"patch_prepare", 1, false}, {"patch_cleanup", 2, false},
		{"update_prepare", 1, true}, {"update_cleanup", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) })
			th.App.Srv().SetLicense(model.NewTestLicense())
			user := th.CreateUser(t)
			db := th.Store.GetInternalMasterDB()
			_, err := db.Exec(`UPDATE users SET email='',emailverified=FALSE WHERE id=$1`, user.Id)
			require.NoError(t, err)
			th.App.InvalidateCacheForUser(user.Id)
			key := int64(17925101 + tc.pauseAt)
			release := pauseEmailWrite(t, db, key, tc.pauseAt, false)
			c := emailMutationContext(th, context.Background(), user.Id)
			body := `{"nickname":"lifecycle"}`
			if tc.fullUpdate {
				snapshot, appErr := th.App.GetUser(th.Context, user.Id)
				require.Nil(t, appErr)
				snapshot.Nickname = "lifecycle"
				encoded, err := json.Marshal(snapshot)
				require.NoError(t, err)
				body = string(encoded)
			}
			r := httptest.NewRequest(http.MethodPut, "/users/"+user.Id, strings.NewReader(body))
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				if tc.fullUpdate {
					(&Service{app: th.App, schemaReady: true}).updateUser(c, w, r)
				} else {
					(&Service{app: th.App, schemaReady: true}).patchUser(c, w, r)
				}
			}()
			waitEmailWrite(t, db, key)
			// Different field order avoids the fixture's temporary-email gate.
			realEmail := "new-real-" + model.NewId() + "@example.com"
			_, err = db.Exec(`UPDATE users SET emailverified=TRUE,email=$2 WHERE id=$1`, user.Id, realEmail)
			require.NoError(t, err)
			release()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("email mutation did not finish")
			}
			var stored string
			var verified bool
			require.NoError(t, db.QueryRow(`SELECT email,emailverified FROM users WHERE id=$1`, user.Id).Scan(&stored, &verified))
			require.Equal(t, realEmail, stored, "stale temporary-email preparation/cleanup must preserve a newer real email")
			require.True(t, verified, "temporary cleanup must preserve newer verification state")
			if tc.pauseAt == 1 {
				require.NotNil(t, c.Err)
				require.Equal(t, http.StatusConflict, c.Err.StatusCode)
			} else {
				require.Nil(t, c.Err)
				var response model.User
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.Equal(t, realEmail, response.Email)
				require.True(t, response.EmailVerified)
			}
		})
	}
}

func TestEmptyEmailMutationCancellationStillRestoresPlaceholder(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) })
	th.App.Srv().SetLicense(model.NewTestLicense())
	user := th.CreateUser(t)
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec(`UPDATE users SET email='',emailverified=FALSE WHERE id=$1`, user.Id)
	require.NoError(t, err)
	th.App.InvalidateCacheForUser(user.Id)
	key := int64(17925105)
	release := pauseEmailWrite(t, db, key, 1, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := emailMutationContext(th, ctx, user.Id)
	r := httptest.NewRequest(http.MethodPut, "/users/"+user.Id+"/patch", strings.NewReader(`{"nickname":"canceled-lifecycle"}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Service{app: th.App, schemaReady: true}).patchUser(c, httptest.NewRecorder(), r)
	}()
	waitEmailWrite(t, db, key)
	var email string
	require.NoError(t, db.QueryRow(`SELECT email FROM users WHERE id=$1`, user.Id).Scan(&email))
	require.True(t, strings.HasPrefix(email, "no-email-"), "native mutation must start after temporary validation email was committed")
	cancel()
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled mutation did not finish")
	}
	require.NoError(t, db.QueryRow(`SELECT email FROM users WHERE id=$1`, user.Id).Scan(&email))
	require.Empty(t, email, "HTTP cancellation must not leave a temporary address in the account")
}

func TestEmptyEmailMutationFailureRestoresPlaceholder(t *testing.T) {
	for _, operation := range []string{"patch_validation", "deactivate", "soft_delete"} {
		t.Run(operation, func(t *testing.T) {
			th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) })
			th.App.Srv().SetLicense(model.NewTestLicense())
			user := th.CreateUser(t)
			db := th.Store.GetInternalMasterDB()
			_, err := db.Exec(`UPDATE users SET email='',emailverified=FALSE WHERE id=$1`, user.Id)
			require.NoError(t, err)
			th.App.InvalidateCacheForUser(user.Id)
			handler := (&Service{app: th.App, schemaReady: true}).patchUser
			body := `{"username":"invalid username"}`
			if operation != "patch_validation" {
				// Native deactivation persists the user before revoking its
				// sessions. A revocation failure must not strand its email.
				_, err = db.Exec(fmt.Sprintf(`CREATE FUNCTION fail_email_lifecycle_session_delete() RETURNS trigger LANGUAGE plpgsql AS $$
                    BEGIN IF OLD.userid='%s' THEN RAISE EXCEPTION 'controlled session revocation failure'; END IF; RETURN OLD; END $$;
                    CREATE TRIGGER fail_email_lifecycle_session_delete BEFORE DELETE ON sessions FOR EACH ROW EXECUTE FUNCTION fail_email_lifecycle_session_delete()`, user.Id))
				require.NoError(t, err)
				t.Cleanup(func() {
					_, err := db.Exec(`DROP TRIGGER fail_email_lifecycle_session_delete ON sessions; DROP FUNCTION fail_email_lifecycle_session_delete()`)
					require.NoError(t, err)
				})
				_, _, err = th.CreateClient().Login(context.Background(), user.Username, user.Password)
				require.NoError(t, err)
				handler = (&Service{app: th.App, schemaReady: true}).updateUserActive
				body = `{"active":false}`
				if operation == "soft_delete" {
					handler = (&Service{app: th.App, schemaReady: true}).deleteUser
					body = ""
				}
			}
			c := emailMutationContext(th, context.Background(), user.Id)
			r := httptest.NewRequest(http.MethodPut, "/users/"+user.Id, strings.NewReader(body))
			handler(c, httptest.NewRecorder(), r)
			require.NotNil(t, c.Err, "controlled native failure must remain an error")
			var email string
			require.NoError(t, db.QueryRow(`SELECT email FROM users WHERE id=$1`, user.Id).Scan(&email))
			require.Empty(t, email, "failed native mutation must not strand its temporary email")
		})
	}
}

func TestEmptyEmailMutationRoleAndActivationCleanupPreservesNewerEmail(t *testing.T) {
	for _, operation := range []string{"roles", "deactivate", "soft_delete"} {
		t.Run(operation, func(t *testing.T) {
			th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) })
			th.App.Srv().SetLicense(model.NewTestLicense())
			user := th.CreateUser(t)
			db := th.Store.GetInternalMasterDB()
			_, err := db.Exec(`UPDATE users SET email='',emailverified=FALSE WHERE id=$1`, user.Id)
			require.NoError(t, err)
			th.App.InvalidateCacheForUser(user.Id)
			key := int64(17925107)
			release := pauseEmailWrite(t, db, key, 1, false)
			handler := (&Service{app: th.App, schemaReady: true}).updateUserRoles
			body := `{"roles":"system_user"}`
			if operation == "deactivate" {
				handler = (&Service{app: th.App, schemaReady: true}).updateUserActive
				body = `{"active":false}`
			}
			if operation == "soft_delete" {
				handler = (&Service{app: th.App, schemaReady: true}).deleteUser
				body = ""
			}
			c := emailMutationContext(th, context.Background(), user.Id)
			r := httptest.NewRequest(http.MethodPut, "/users/"+user.Id, strings.NewReader(body))
			done := make(chan struct{})
			go func() { defer close(done); handler(c, httptest.NewRecorder(), r) }()
			waitEmailWrite(t, db, key)
			realEmail := "newer-role-" + model.NewId() + "@example.com"
			_, err = db.Exec(`UPDATE users SET emailverified=TRUE,email=$2 WHERE id=$1`, user.Id, realEmail)
			require.NoError(t, err)
			release()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("mutation did not finish")
			}
			require.Nil(t, c.Err)
			var email string
			var verified bool
			require.NoError(t, db.QueryRow(`SELECT email,emailverified FROM users WHERE id=$1`, user.Id).Scan(&email, &verified))
			require.Equal(t, realEmail, email)
			require.True(t, verified)
		})
	}
}
