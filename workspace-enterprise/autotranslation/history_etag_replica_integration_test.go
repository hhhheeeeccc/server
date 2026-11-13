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

func TestHistoryETagWithDelayedPhysicalReplica(t *testing.T) {
	raw := os.Getenv("WORKSPACE_TEST_TRANSLATION_REPLICA_DSN")
	if raw == "" {
		t.Skip("use deployment/autotranslation/run_replica_test.py for an isolated physical standby")
	}
	base, err := url.Parse(raw)
	require.NoError(t, err)
	ip := net.ParseIP(base.Hostname())
	require.True(t, base.Scheme == "postgres" && ip != nil && ip.IsLoopback() && base.Port() != "")
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := saveForegroundPost(t, th, "Source retained while conditional history uses a delayed standby.")
	saveReadTranslation(t, th, post, post.Message, "translation on both databases")
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
	require.True(t, recovery)
	th.Server.SetStore(readStore)
	first, posts := getConditionalHistory(t, th, "", "")
	require.Equal(t, "translation on both databases", posts.Posts[post.Id].Metadata.Translations["ar"].Text)
	catchUpTranslationReplica(t, primary, control)
	_, err = control.Exec(`SELECT pg_wal_replay_pause()`)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var paused bool
		return control.QueryRow(`SELECT pg_is_wal_replay_paused()`).Scan(&paused) == nil && paused
	}, 5*time.Second, 10*time.Millisecond)
	_, err = primary.Exec(`UPDATE translations SET text='fresh translation only on the primary' WHERE objectid=$1`, post.Id)
	require.NoError(t, err)
	stale, err := readStore.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, "translation on both databases", stale.Text, "the standby must retain the old payload")
	second, updated := getConditionalHistory(t, th, "", first.Header.Get(model.HeaderEtagServer))
	require.Equal(t, http.StatusOK, second.StatusCode, "stale native ETag must not skip the primary translation read")
	require.Equal(t, "fresh translation only on the primary", updated.Posts[post.Id].Metadata.Translations["ar"].Text)
	third, cached := getConditionalHistory(t, th, "", second.Header.Get(model.HeaderEtagServer))
	require.Equal(t, http.StatusNotModified, third.StatusCode)
	require.Nil(t, cached)
	_, err = primary.Exec(`UPDATE posts SET message='New source awaiting recovery on the primary.' WHERE id=$1`, post.Id)
	require.NoError(t, err)
	fourth, pending := getConditionalHistory(t, th, "", second.Header.Get(model.HeaderEtagServer))
	require.Equal(t, http.StatusOK, fourth.StatusCode)
	require.Empty(t, pending.Posts[post.Id].Metadata.Translations["ar"].Text)
	require.Equal(t, string(model.TranslationStateProcessing), pending.Posts[post.Id].Metadata.Translations["ar"].State)
	_, err = primary.Exec(`UPDATE posts SET deleteat=1 WHERE id=$1`, post.Id)
	require.NoError(t, err)
	fifth, deleted := getConditionalHistory(t, th, "", fourth.Header.Get(model.HeaderEtagServer))
	require.Equal(t, http.StatusOK, fifth.StatusCode)
	// The native post body can still come from the delayed standby. Only the
	// translation's primary snapshot and the actual validator are guaranteed.
	require.NotNil(t, deleted.Posts[post.Id])
	require.Empty(t, deleted.Posts[post.Id].Metadata.Translations)
	_, err = control.Exec(`SELECT pg_wal_replay_resume()`)
	require.NoError(t, err)
	catchUpTranslationReplica(t, primary, control)
}
