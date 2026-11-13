package autotranslation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func saveReadTranslation(t *testing.T, th *api4.TestHelper, post *model.Post, source, translated string) {
	t.Helper()
	digest := sha256.Sum256([]byte(strings.TrimSpace(source)))
	require.NoError(t, th.Store.AutoTranslation().Save(&model.Translation{
		ObjectType: model.TranslationObjectTypePost, ObjectID: post.Id, ChannelID: post.ChannelId,
		Lang: "ar", Provider: providerName, Type: model.TranslationTypeString,
		NormHash: hex.EncodeToString(digest[:]), Text: translated, State: model.TranslationStateReady,
	}))
}

func TestReadBatchInvalidatesHistoricalTextAndRecoveryUsesCurrentSource(t *testing.T) {
	var received atomic.Value
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
			return
		}
		var body translateRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "bad fixture request", 400)
			return
		}
		received.Store(body.Text)
		_, _ = w.Write([]byte(`{"translatedText":"recovered canonical translation"}`))
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	post := saveForegroundPost(t, th, "The current canonical source.")
	// Raw store fixtures reproduce a row retained before the edit protection.
	saveReadTranslation(t, th, post, "The previous source.", "retained private old text")
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET confidence=90,
		meta=meta || '{"error":"private old error","src_lang":"en","workspace_retry":{"attempts":9}}'::jsonb WHERE objectid=$1`, post.Id)
	require.NoError(t, err)
	translations, appErr := New(th.Server).GetBatch(model.TranslationObjectTypePost, []string{post.Id}, "ar")
	require.Nil(t, appErr)
	require.NotNil(t, translations[post.Id])
	require.Empty(t, translations[post.Id].Text, "batch reads must not expose a payload for a previous source")
	require.Equal(t, model.TranslationStateProcessing, translations[post.Id].State)
	for _, key := range []string{"error", "src_lang", "workspace_retry"} {
		require.NotContains(t, translations[post.Id].Meta, key, "response metadata must match the cleaned stored row")
	}
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Empty(t, stored.Text)
	require.Empty(t, strings.TrimSpace(stored.NormHash), "the CHAR(64) hash is semantically empty")
	require.Nil(t, stored.Confidence)
	for _, key := range []string{"error", "src_lang", "workspace_retry"} {
		require.NotContains(t, stored.Meta, key)
	}
	aged := time.Now().Add(-time.Hour).UnixMilli()
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET updateat=$1,
		meta=jsonb_set(meta,'{workspace_retry}','{"lease":"in-flight-lease","attempts":1}') WHERE objectid=$2`, aged, post.Id)
	require.NoError(t, err)
	// Repeated readers must not postpone recovery or erase a worker's lease.
	translations, appErr = New(th.Server).GetBatch(model.TranslationObjectTypePost, []string{post.Id}, "ar")
	require.Nil(t, appErr)
	again, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	// The raw core Get helper does not populate UpdateAt; inspect both the
	// actual batch response and PostgreSQL instead of assuming that helper does.
	require.Equal(t, aged, translations[post.Id].UpdateAt)
	var storedAge int64
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT updateat FROM translations
		WHERE objecttype='post' AND objectid=$1 AND dstlang='ar'`, post.Id).Scan(&storedAge))
	require.Equal(t, aged, storedAge)
	require.Equal(t, "in-flight-lease", again.Meta["workspace_retry"].(map[string]any)["lease"])
	runRecoveryWorker(t, th, New(th.Server))
	require.Equal(t, post.Message, received.Load())
	translations, appErr = New(th.Server).GetBatch(model.TranslationObjectTypePost, []string{post.Id}, "ar")
	require.Nil(t, appErr)
	require.Equal(t, "recovered canonical translation", translations[post.Id].Text)
}

func TestReadBatchPreservesCurrentRowsAndIndependentObjects(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := saveForegroundPost(t, th, "\u2003Current source with outer Unicode spaces.\u00a0")
	saveReadTranslation(t, th, post, post.Message, "valid current translation")
	saveReadTranslation(t, th, th.BasicPost, th.BasicPost.Message, "undecodable metadata payload")
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET meta='[]'::jsonb WHERE objectid=$1`, th.BasicPost.Id)
	require.NoError(t, err)
	missing := model.NewId()
	service := New(th.Server)
	translations, appErr := service.GetBatch(model.TranslationObjectTypePost, []string{post.Id, missing, post.Id, th.BasicPost.Id}, "ar")
	require.Nil(t, appErr)
	require.Len(t, translations, 1)
	require.Equal(t, "valid current translation", translations[post.Id].Text)
	require.Equal(t, providerName, translations[post.Id].Provider)
	translations, appErr = service.GetBatch(model.TranslationObjectTypePost, nil, "ar")
	require.Nil(t, appErr)
	require.Empty(t, translations)
	translations, appErr = service.GetBatch(model.TranslationObjectTypePost, []string{post.Id}, "fr")
	require.Nil(t, appErr)
	require.Empty(t, translations)
	require.NoError(t, th.Store.AutoTranslation().Save(&model.Translation{
		ObjectType: "independent", ObjectID: missing, Lang: "ar", Provider: providerName,
		Type: model.TranslationTypeString, Text: "independent payload", State: model.TranslationStateReady,
	}))
	translations, appErr = service.GetBatch("independent", []string{missing}, "ar")
	require.Nil(t, appErr)
	require.Equal(t, "independent payload", translations[missing].Text)
	th.Server.SetLicense(nil)
	translations, appErr = service.GetBatch(model.TranslationObjectTypePost, []string{post.Id}, "ar")
	require.Nil(t, translations)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusNotImplemented, appErr.StatusCode)
}

func TestReadBatchBlankSourceRecoveryNeedsNoProvider(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := seedRecoveryPost(t, th, "   ", model.TranslationStateProcessing)
	runRecoveryWorker(t, th, New(th.Server))
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, model.TranslationStateSkipped, stored.State, "blank queue items must finish even without a foreground call")
	require.Empty(t, stored.Text)
}

func TestReadBatchHistoryAPIHidesHistoricalPayloadWithoutProvider(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := saveForegroundPost(t, th, "Canonical source for the history API.")
	saveReadTranslation(t, th, post, "Superseded source for the history API.", "private obsolete translated payload")
	posts, _, err := th.Client.GetPostsForChannel(th.Context.Context(), post.ChannelId, 0, 50, "", false, false)
	require.NoError(t, err)
	require.NotNil(t, posts.Posts[post.Id])
	require.Equal(t, post.Message, posts.Posts[post.Id].Message)
	translation := posts.Posts[post.Id].Metadata.Translations["ar"]
	require.NotNil(t, translation)
	require.Empty(t, translation.Text)
	require.Equal(t, string(model.TranslationStateProcessing), translation.State)
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Empty(t, stored.Text)
}

func TestReadBatchReconciliationPreservesConcurrentCurrentResult(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := saveForegroundPost(t, th, "Canonical source during read reconciliation.")
	saveReadTranslation(t, th, post, "Obsolete source before read reconciliation.", "old historical payload")
	stale, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	stale.ChannelID = post.ChannelId
	db := th.Store.GetInternalMasterDB()
	ctx, cancel := context.WithTimeout(th.Context.Context(), 10*time.Second)
	defer cancel()
	writer, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer writer.Rollback()
	var writerPID int
	require.NoError(t, writer.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID))
	_, err = writer.ExecContext(ctx, `UPDATE posts SET updateat=updateat+1 WHERE id=$1`, post.Id)
	require.NoError(t, err)
	type result struct {
		read *postTranslationRead
		err  error
	}
	finished := make(chan result, 1)
	go func() {
		read, readErr := New(th.Server).reconcilePostTranslation(ctx, &postTranslationRead{translation: stale, sourceText: post.Message})
		finished <- result{read, readErr}
	}()
	require.Eventually(t, func() bool {
		var blocked bool
		return db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE $1=ANY(pg_blocking_pids(pid)) AND query LIKE 'SELECT id FROM posts%')`, writerPID).Scan(&blocked) == nil && blocked
	}, 3*time.Second, 10*time.Millisecond)
	digest := sha256.Sum256([]byte(post.Message))
	_, err = writer.ExecContext(ctx, `UPDATE translations SET normhash=$1,text='concurrent current result',state='ready'
		WHERE objecttype='post' AND objectid=$2`, hex.EncodeToString(digest[:]), post.Id)
	require.NoError(t, err)
	require.NoError(t, writer.Commit())
	select {
	case response := <-finished:
		require.NoError(t, response.err)
		require.NotNil(t, response.read)
		require.Equal(t, "concurrent current result", response.read.translation.Text)
		require.Equal(t, model.TranslationStateReady, response.read.translation.State)
	case <-ctx.Done():
		t.Fatal("read reconciliation did not finish after the writer committed")
	}
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, "concurrent current result", stored.Text)
	require.Equal(t, model.TranslationStateReady, stored.State)
}
