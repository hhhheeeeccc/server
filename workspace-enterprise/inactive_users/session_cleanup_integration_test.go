package inactive_users

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestRetryPendingInactiveUserSessionCleanup(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	service := &Service{app: th.App}
	require.NoError(t, service.initSessionCleanup())

	userID := th.BasicUser.Id
	sessions, err := th.Store.Session().GetSessions(th.Context, userID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions)

	cutoff := model.GetMillis()
	_, err = th.Store.GetInternalMasterDB().ExecContext(th.Context.Context(),
		`INSERT INTO WorkspaceInactiveUserSessionCleanup (UserId,CutoffAt,RetryAt) VALUES ($1,$2,0)`, userID, cutoff)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = th.Store.GetInternalMasterDB().Exec(`DELETE FROM WorkspaceInactiveUserSessionCleanup WHERE UserId=$1`, userID)
	})
	newSession, err := th.Store.Session().Save(th.Context, &model.Session{
		UserId: userID, ExpiresAt: model.GetMillis() + 3600000,
	})
	require.NoError(t, err)
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE Sessions SET CreateAt=$1 WHERE Id=$2`, cutoff+1, newSession.Id)
	require.NoError(t, err)
	candidates, err := service.sessionsToRevoke(th.Context, userID, cutoff)
	require.NoError(t, err)
	require.Len(t, candidates, len(sessions))
	originalByID := make(map[string]*model.Session, len(sessions))
	for _, session := range sessions {
		originalByID[session.Id] = session
	}
	for _, candidate := range candidates {
		require.LessOrEqual(t, candidate.CreateAt, cutoff)
		require.Equal(t, userID, candidate.UserId)
		require.NotEqual(t, newSession.Id, candidate.Id)
		original, ok := originalByID[candidate.Id]
		require.True(t, ok)
		require.Equal(t, original.Token, candidate.Token)
		require.Equal(t, original.DeviceId, candidate.DeviceId)
		require.Equal(t, original.IsOAuth, candidate.IsOAuth)
		require.Equal(t, original.Props, candidate.Props)
	}

	require.NoError(t, service.retryPendingSessionCleanup(context.Background()))
	sessions, err = th.Store.Session().GetSessions(th.Context, userID)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, newSession.Id, sessions[0].Id, "retry must not revoke sessions created after deactivation")
	var pending int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRowContext(th.Context.Context(),
		`SELECT count(*) FROM WorkspaceInactiveUserSessionCleanup WHERE UserId=$1`, userID).Scan(&pending))
	require.Zero(t, pending)
}

func TestDeactivationKeepsCleanupIntentAfterSessionStoreFailure(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	service := &Service{app: th.App}
	userID := th.BasicUser2.Id
	client := th.CreateClient()
	th.LoginBasic2WithClient(t, client)
	require.NoError(t, th.Store.Status().SaveOrUpdate(&model.Status{
		UserId: userID, Status: model.StatusOffline, LastActivityAt: 1,
	}))
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec(fmt.Sprintf(`CREATE FUNCTION workspace_inactive_test_delete() RETURNS trigger AS $$
		BEGIN IF OLD.UserId = '%s' THEN RAISE EXCEPTION 'fixture session deletion failure'; END IF;
		RETURN OLD; END; $$ LANGUAGE plpgsql`, userID))
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER workspace_inactive_test_delete BEFORE DELETE ON Sessions
		FOR EACH ROW EXECUTE FUNCTION workspace_inactive_test_delete()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS workspace_inactive_test_delete ON Sessions`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS workspace_inactive_test_delete()`)
		_, _ = db.Exec(`DELETE FROM WorkspaceInactiveUserSessionCleanup WHERE UserId=$1`, userID)
	})

	response, err := th.SystemAdminClient.DoAPIPost(th.Context.Context(), "/users/deactivate/inactive", `{"days":30}`)
	require.Error(t, err)
	require.Equal(t, http.StatusInternalServerError, response.StatusCode)
	require.NoError(t, response.Body.Close())
	var deleteAt, cutoff int64
	require.NoError(t, db.QueryRow(`SELECT DeleteAt FROM Users WHERE Id=$1`, userID).Scan(&deleteAt))
	require.Positive(t, deleteAt, "deactivation has already committed")
	require.NoError(t, db.QueryRow(`SELECT CutoffAt FROM WorkspaceInactiveUserSessionCleanup WHERE UserId=$1`, userID).Scan(&cutoff))
	require.Equal(t, deleteAt, cutoff, "the durable intent and deactivation commit together")
	var liveSessions int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM Sessions WHERE UserId=$1 AND (ExpiresAt=0 OR ExpiresAt>$2)`,
		userID, model.GetMillis()).Scan(&liveSessions))
	require.Zero(t, liveSessions, "the same transaction expires existing sessions")
	_, getMeResponse, err := client.GetMe(th.Context.Context(), "")
	require.Error(t, err)
	require.Equal(t, http.StatusUnauthorized, getMeResponse.StatusCode)
	_, err = db.Exec(`DROP TRIGGER workspace_inactive_test_delete ON Sessions`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE WorkspaceInactiveUserSessionCleanup SET RetryAt=0 WHERE UserId=$1`, userID)
	require.NoError(t, err)
	require.NoError(t, service.retryPendingSessionCleanup(context.Background()))
	sessions, err := th.Store.Session().GetSessions(th.Context, userID)
	require.NoError(t, err)
	require.Empty(t, sessions)
	var pending int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM WorkspaceInactiveUserSessionCleanup WHERE UserId=$1`, userID).Scan(&pending))
	require.Zero(t, pending)
}

func TestDeactivationRollsBackWhenSessionExpiryFails(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	userID := th.BasicUser2.Id
	client := th.CreateClient()
	th.LoginBasic2WithClient(t, client)
	require.NoError(t, th.Store.Status().SaveOrUpdate(&model.Status{
		UserId: userID, Status: model.StatusOffline, LastActivityAt: 1,
	}))
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec(fmt.Sprintf(`CREATE FUNCTION workspace_inactive_test_expiry() RETURNS trigger AS $$
		BEGIN IF OLD.UserId = '%s' THEN RAISE EXCEPTION 'fixture session expiry failure'; END IF;
		RETURN NEW; END; $$ LANGUAGE plpgsql`, userID))
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER workspace_inactive_test_expiry BEFORE UPDATE OF ExpiresAt ON Sessions
		FOR EACH ROW EXECUTE FUNCTION workspace_inactive_test_expiry()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS workspace_inactive_test_expiry ON Sessions`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS workspace_inactive_test_expiry()`)
	})

	response, err := th.SystemAdminClient.DoAPIPost(th.Context.Context(), "/users/deactivate/inactive", `{"days":30}`)
	require.Error(t, err)
	require.Equal(t, http.StatusInternalServerError, response.StatusCode)
	require.NoError(t, response.Body.Close())
	var deleteAt, pending int64
	require.NoError(t, db.QueryRow(`SELECT DeleteAt FROM Users WHERE Id=$1`, userID).Scan(&deleteAt))
	require.Zero(t, deleteAt, "the deactivation must not commit when sessions cannot expire")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM WorkspaceInactiveUserSessionCleanup WHERE UserId=$1`, userID).Scan(&pending))
	require.Zero(t, pending)
	_, getMeResponse, err := client.GetMe(th.Context.Context(), "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, getMeResponse.StatusCode)
}
