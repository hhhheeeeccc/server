//go:build physicalreplica

package autotranslation

import (
	"database/sql"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/store/sqlstore"
	"github.com/stretchr/testify/require"
)

func catchUpTranslationReplica(t *testing.T, primary, replica *sql.DB) {
	t.Helper()
	var lsn string
	require.NoError(t, primary.QueryRow(`SELECT pg_current_wal_lsn()::text`).Scan(&lsn))
	require.Eventually(t, func() bool {
		var caughtUp bool
		return replica.QueryRow(`SELECT COALESCE(pg_last_wal_replay_lsn()>=$1::pg_lsn,false)`, lsn).Scan(&caughtUp) == nil && caughtUp
	}, 30*time.Second, 50*time.Millisecond)
}

func TestReadBatchWithDelayedPhysicalReplica(t *testing.T) {
	raw := os.Getenv("WORKSPACE_TEST_TRANSLATION_REPLICA_DSN")
	if raw == "" {
		t.Skip("use deployment/autotranslation/run_replica_test.py for an isolated physical standby")
	}
	base, err := url.Parse(raw)
	require.NoError(t, err)
	ip := net.ParseIP(base.Hostname())
	require.True(t, base.Scheme == "postgres" && ip != nil && ip.IsLoopback() && base.Port() != "")
	th := setupRecovery(t, "http://127.0.0.1:1")
	changed := saveForegroundPost(t, th, "Source before replica replay stopped.")
	deleted := saveForegroundPost(t, th, "Source deleted after replica replay stopped.")
	pending := saveForegroundPost(t, th, "Source pending after replica replay stopped.")
	for _, post := range []*model.Post{changed, deleted, pending} {
		saveReadTranslation(t, th, post, post.Message, "old payload on the standby")
	}
	primary := th.Store.GetInternalMasterDB()
	control, err := sql.Open("postgres", base.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = control.Close() })
	catchUpTranslationReplica(t, primary, control)
	settings := th.App.Config().SqlSettings
	primaryURL, err := url.Parse(*settings.DataSource)
	require.NoError(t, err)
	replicaURL := *base
	replicaURL.Path = primaryURL.Path
	settings.DataSourceReplicas = []string{replicaURL.String()}
	readStore, err := sqlstore.New(settings, th.App.Log(), nil, sqlstore.SkipMigrations())
	require.NoError(t, err)
	readStore.UpdateLicense(th.App.License())
	original := th.Server.Store()
	t.Cleanup(func() {
		_, _ = control.Exec(`SELECT pg_wal_replay_resume()`)
		th.Server.SetStore(original)
		readStore.Close()
	})
	var recovery bool
	require.NoError(t, readStore.GetInternalReplicaDB().QueryRow(`SELECT pg_is_in_recovery()`).Scan(&recovery))
	require.True(t, recovery, "the store must select a real read-only standby")
	catchUpTranslationReplica(t, primary, control)
	_, err = control.Exec(`SELECT pg_wal_replay_pause()`)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var paused bool
		return control.QueryRow(`SELECT pg_is_wal_replay_paused()`).Scan(&paused) == nil && paused
	}, 5*time.Second, 10*time.Millisecond)
	_, err = primary.Exec(`UPDATE posts SET message='Current canonical source on the primary.' WHERE id=$1`, changed.Id)
	require.NoError(t, err)
	changed.Message = "Current canonical source on the primary."
	saveReadTranslation(t, th, changed, changed.Message, "fresh primary translation")
	_, err = primary.Exec(`UPDATE posts SET deleteat=1 WHERE id=$1`, deleted.Id)
	require.NoError(t, err)
	_, err = primary.Exec(`UPDATE posts SET message='Current text awaiting recovery.' WHERE id=$1`, pending.Id)
	require.NoError(t, err)
	created := saveForegroundPost(t, th, "New source absent from the standby.")
	saveReadTranslation(t, th, created, created.Message, "new primary translation")
	stale, err := readStore.AutoTranslation().GetBatch(model.TranslationObjectTypePost, []string{changed.Id, deleted.Id, pending.Id, created.Id}, "ar")
	require.NoError(t, err)
	require.Len(t, stale, 3)
	require.Equal(t, "old payload on the standby", stale[changed.Id].Text)
	require.NotNil(t, stale[deleted.Id], "the standby must really retain the deleted source's translation")
	th.Server.SetStore(readStore)
	service := New(th.Server)
	t.Run("fresh_primary", func(t *testing.T) {
		rows, appErr := service.GetBatch(model.TranslationObjectTypePost, []string{changed.Id}, "ar")
		require.Nil(t, appErr)
		require.NotNil(t, rows[changed.Id])
		require.Equal(t, "fresh primary translation", rows[changed.Id].Text)
	})
	t.Run("deleted_primary", func(t *testing.T) {
		rows, appErr := service.GetBatch(model.TranslationObjectTypePost, []string{deleted.Id}, "ar")
		require.Nil(t, appErr)
		require.Empty(t, rows, "a stale standby must not resurrect deleted translations")
	})
	t.Run("pending_primary", func(t *testing.T) {
		rows, appErr := service.GetBatch(model.TranslationObjectTypePost, []string{pending.Id}, "ar")
		require.Nil(t, appErr)
		require.NotNil(t, rows[pending.Id])
		require.Empty(t, rows[pending.Id].Text)
		require.Equal(t, model.TranslationStateProcessing, rows[pending.Id].State)
	})
	t.Run("new_primary", func(t *testing.T) {
		rows, appErr := service.GetBatch(model.TranslationObjectTypePost, []string{created.Id}, "ar")
		require.Nil(t, appErr)
		require.NotNil(t, rows[created.Id])
		require.Equal(t, "new primary translation", rows[created.Id].Text)
	})
	t.Run("primary_failure_does_not_fall_back", func(t *testing.T) {
		require.NoError(t, readStore.GetInternalMasterDB().Close())
		rows, appErr := service.GetBatch(model.TranslationObjectTypePost, []string{changed.Id}, "ar")
		require.Nil(t, rows)
		require.NotNil(t, appErr, "an available stale standby must not conceal primary failure")
		require.Equal(t, http.StatusInternalServerError, appErr.StatusCode)
	})
	_, err = control.Exec(`SELECT pg_wal_replay_resume()`)
	require.NoError(t, err)
	catchUpTranslationReplica(t, primary, control)
}
