package autotranslation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func saveForegroundPost(t *testing.T, th *api4.TestHelper, text string) *model.Post {
	t.Helper()
	post, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: th.BasicChannel.Id, UserId: th.BasicUser.Id, Message: text})
	require.NoError(t, err)
	return post
}

func TestForegroundEditDiscardsOldProviderResponse(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	releaseOld := sync.OnceFunc(func() { close(release) })
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
		if body.Text == "Original meeting tomorrow." {
			close(started)
			<-release
			_, _ = w.Write([]byte(`{"translatedText":"OLD response"}`))
			return
		}
		_, _ = w.Write([]byte(`{"translatedText":"CURRENT response"}`))
	}))
	t.Cleanup(provider.Close)
	t.Cleanup(releaseOld)
	th := setupRecovery(t, provider.URL)
	post := saveForegroundPost(t, th, "Original meeting tomorrow.")
	type result struct {
		translation *model.Translation
		err         *model.AppError
	}
	oldFinished := make(chan result, 1)
	go func() {
		translation, err := New(th.Server).Translate(th.Context.Context(), model.TranslationObjectTypePost, post.Id, post.ChannelId, post.UserId, post.Message)
		oldFinished <- result{translation, err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("old translation did not reach the provider")
	}
	const current = "Updated meeting next week."
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET message=$1,updateat=updateat+1 WHERE id=$2`, current, post.Id)
	require.NoError(t, err)
	translated, appErr := New(th.Server).Translate(th.Context.Context(), model.TranslationObjectTypePost, post.Id, post.ChannelId, post.UserId, current)
	require.Nil(t, appErr)
	require.NotNil(t, translated)
	require.Equal(t, "CURRENT response", translated.Text)
	releaseOld()
	select {
	case response := <-oldFinished:
		require.Nil(t, response.err)
		require.Nil(t, response.translation, "the caller must not receive a superseded translation")
	case <-time.After(5 * time.Second):
		t.Fatal("old translation did not finish")
	}
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, "CURRENT response", stored.Text, "a delayed old response must not replace the latest translation")
	digest := sha256.Sum256([]byte(current))
	require.Equal(t, hex.EncodeToString(digest[:]), stored.NormHash)
}

func TestForegroundEditInvalidatesOldPayloadImmediately(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := seedRecoveryPost(t, th, "Old source text.", model.TranslationStateUnavailable)
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET state='ready',text='old private translation' WHERE objectid=$1`, post.Id)
	require.NoError(t, err)
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET message='Changed source text.' WHERE id=$1`, post.Id)
	require.NoError(t, err)
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Empty(t, stored.Text, "the old payload must be hidden in the editing transaction")
	require.Equal(t, model.TranslationStateProcessing, stored.State, "keep a durable retry item for the current source")
}

func TestForegroundEditDuringDetectionStopsOldTranslationRequest(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	releaseOld := sync.OnceFunc(func() { close(release) })
	var translationCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			close(started)
			<-release
			_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
			return
		}
		translationCalls.Add(1)
		_, _ = w.Write([]byte(`{"translatedText":"old translation"}`))
	}))
	t.Cleanup(provider.Close)
	t.Cleanup(releaseOld)
	th := setupRecovery(t, provider.URL)
	post := saveForegroundPost(t, th, "Old source text.")
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = New(th.Server).Translate(th.Context.Context(), model.TranslationObjectTypePost, post.Id, post.ChannelId, post.UserId, post.Message)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("detection did not start")
	}
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET message='New text after detection started.' WHERE id=$1`, post.Id)
	require.NoError(t, err)
	releaseOld()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("old translation attempt did not stop")
	}
	require.Zero(t, translationCalls.Load(), "a superseded detector result must not cause another provider request")
}

func TestForegroundStaleInputDoesNotReachProvider(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
		} else {
			_, _ = w.Write([]byte(`{"translatedText":"stale translation"}`))
		}
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	post := saveForegroundPost(t, th, "The current source text.")
	_, _ = New(th.Server).Translate(th.Context.Context(), model.TranslationObjectTypePost, post.Id, post.ChannelId, post.UserId, "The old source text.")
	require.Zero(t, calls.Load(), "stale input must be rejected before sending its content to the provider")
}

func TestForegroundConcurrentFailureDoesNotReplaceReadyResult(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	releaseOld := sync.OnceFunc(func() { close(release) })
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
			return
		}
		if requests.Add(1) == 1 {
			close(started)
			<-release
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"translatedText":"successful concurrent translation"}`))
	}))
	t.Cleanup(provider.Close)
	t.Cleanup(releaseOld)
	th := setupRecovery(t, provider.URL)
	post := saveForegroundPost(t, th, "The same current source for both calls.")
	type result struct {
		translation *model.Translation
		err         *model.AppError
	}
	finished := make(chan result, 1)
	go func() {
		translation, err := New(th.Server).Translate(th.Context.Context(), model.TranslationObjectTypePost, post.Id, post.ChannelId, post.UserId, post.Message)
		finished <- result{translation, err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not start")
	}
	ready, appErr := New(th.Server).Translate(th.Context.Context(), model.TranslationObjectTypePost, post.Id, post.ChannelId, post.UserId, post.Message)
	require.Nil(t, appErr)
	require.NotNil(t, ready)
	require.Equal(t, model.TranslationStateReady, ready.State)
	releaseOld()
	select {
	case response := <-finished:
		require.Nil(t, response.err, "the concurrent ready result wins over this call's provider failure")
		require.NotNil(t, response.translation)
		require.Equal(t, ready.Text, response.translation.Text)
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not finish")
	}
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, model.TranslationStateReady, stored.State)
	require.Equal(t, ready.Text, stored.Text)
}

func TestForegroundSaveRechecksAfterWaitingForSourceEdit(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := seedRecoveryPost(t, th, "Source before a locked edit.", model.TranslationStateUnavailable)
	db := th.Store.GetInternalMasterDB()
	ctx, cancel := context.WithTimeout(th.Context.Context(), 10*time.Second)
	defer cancel()
	edit, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer edit.Rollback()
	var editorPID int
	require.NoError(t, edit.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&editorPID))
	_, err = edit.ExecContext(ctx, `UPDATE posts SET message='Source after the locked edit.' WHERE id=$1`, post.Id)
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(post.Message))
	old := &model.Translation{ObjectType: model.TranslationObjectTypePost, ObjectID: post.Id,
		ChannelID: post.ChannelId, Lang: "ar", Provider: providerName, Type: model.TranslationTypeString,
		Text: "old response waiting to save", NormHash: hex.EncodeToString(digest[:]), State: model.TranslationStateReady}
	type result struct {
		translation *model.Translation
		err         error
	}
	finished := make(chan result, 1)
	go func() {
		stored, saveErr := New(th.Server).savePostTranslation(ctx, old, post.Message)
		finished <- result{stored, saveErr}
	}()
	require.Eventually(t, func() bool {
		var blocked bool
		return db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE $1=ANY(pg_blocking_pids(pid)) AND query LIKE 'SELECT id FROM posts%')`, editorPID).Scan(&blocked) == nil && blocked
	}, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, edit.Commit())
	select {
	case response := <-finished:
		require.NoError(t, response.err)
		require.Nil(t, response.translation, "a source changed while waiting must discard the old result")
	case <-ctx.Done():
		t.Fatal("save did not finish after the edit committed")
	}
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Empty(t, stored.Text)
	require.Equal(t, model.TranslationStateProcessing, stored.State)
}

func TestForegroundEditRollbackAndNonTextUpdatesPreserveTranslation(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := seedRecoveryPost(t, th, "Source that will remain current.", model.TranslationStateUnavailable)
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec(`UPDATE translations SET state='ready',text='preserved payload' WHERE objectid=$1`, post.Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE posts SET ispinned=true,updateat=updateat+1 WHERE id=$1`, post.Id)
	require.NoError(t, err)
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, "preserved payload", stored.Text)
	edit, err := db.BeginTx(th.Context.Context(), nil)
	require.NoError(t, err)
	defer edit.Rollback()
	_, err = edit.Exec(`UPDATE posts SET message='This edit is rolled back.' WHERE id=$1`, post.Id)
	require.NoError(t, err)
	var text string
	require.NoError(t, edit.QueryRow(`SELECT text FROM translations WHERE objecttype='post' AND objectid=$1 AND dstlang='ar'`, post.Id).Scan(&text))
	require.Empty(t, text)
	require.NoError(t, edit.Rollback())
	stored, err = th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, "preserved payload", stored.Text)
	require.Equal(t, model.TranslationStateReady, stored.State)
}

func TestForegroundBlankEditFinishesWithoutProvider(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := seedRecoveryPost(t, th, "Source becoming blank.", model.TranslationStateUnavailable)
	const blank = "\u2003\t\n"
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET message=$1 WHERE id=$2`, blank, post.Id)
	require.NoError(t, err)
	translation, appErr := New(th.Server).Translate(th.Context.Context(), model.TranslationObjectTypePost, post.Id, post.ChannelId, post.UserId, blank)
	require.Nil(t, appErr, "blank content must not contact the unreachable provider")
	require.Nil(t, translation)
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, model.TranslationStateSkipped, stored.State)
	require.Empty(t, stored.Text)
}

func TestForegroundInvalidatedEditIsRecoveredWithoutForegroundCall(t *testing.T) {
	var receivedText atomic.Value
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
		receivedText.Store(body.Text)
		_, _ = w.Write([]byte(`{"translatedText":"recovered edited text"}`))
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	post := seedRecoveryPost(t, th, "Original text before an offline edit.", model.TranslationStateUnavailable)
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET state='ready',text='old translation before the edit' WHERE objectid=$1`, post.Id)
	require.NoError(t, err)
	const current = "Current edited text while foreground translation was absent."
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET message=$1 WHERE id=$2`, current, post.Id)
	require.NoError(t, err)
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET updateat=$1 WHERE objectid=$2`, time.Now().Add(-time.Hour).UnixMilli(), post.Id)
	require.NoError(t, err)
	// The new service has no in-memory retry state and Translate is never called.
	runRecoveryWorker(t, th, New(th.Server))
	require.Equal(t, current, receivedText.Load())
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, model.TranslationStateReady, stored.State)
	require.Equal(t, "recovered edited text", stored.Text)
	digest := sha256.Sum256([]byte(current))
	require.Equal(t, hex.EncodeToString(digest[:]), stored.NormHash)
}
