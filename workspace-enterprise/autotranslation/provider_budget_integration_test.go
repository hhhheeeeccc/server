package autotranslation

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func addBudgetLanguages(t *testing.T, th *api4.TestHelper) []string {
	t.Helper()
	for _, language := range []string{"fr", "de", "es"} {
		user := th.CreateUser(t)
		th.LinkUserToTeam(t, user, th.BasicTeam)
		th.AddUserToChannel(t, user, th.BasicChannel)
		_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE users SET locale=$1 WHERE id=$2`, language, user.Id)
		require.NoError(t, err)
	}
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE users SET locale='en' WHERE id=$1`, th.BasicUser2.Id)
	require.NoError(t, err)
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.AutoTranslationSettings.TimeoutMs = new(200)
	})
	return []string{"ar", "en", "fr", "de", "es"}
}

func TestForegroundProviderBudgetPersistsUnfinishedLanguages(t *testing.T) {
	for _, phase := range []string{"detection", "translation"} {
		t.Run(phase, func(t *testing.T) {
			var calls, translations atomic.Int32
			var healthy atomic.Bool
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/detect" {
					if phase == "detection" && !healthy.Load() {
						<-r.Context().Done()
						return
					}
					_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
					return
				}
				attempt := translations.Add(1)
				if phase == "detection" || (attempt > 1 && !healthy.Load()) {
					if !healthy.Load() {
						<-r.Context().Done()
						return
					}
				}
				_, _ = w.Write([]byte(`{"translatedText":"A completed synthetic translation."}`))
			}))
			t.Cleanup(provider.Close)
			th := setupRecovery(t, provider.URL)
			languages := addBudgetLanguages(t, th)
			post := saveForegroundPost(t, th, "A synthetic message with five reader languages.")
			service := New(th.Server)
			started := time.Now()
			_, appErr := service.Translate(th.Context.Context(), model.TranslationObjectTypePost, post.Id, post.ChannelId, post.UserId, post)
			t.Logf("foreground provider phase %s: %s, requests=%d", phase, time.Since(started), calls.Load())
			// The author language may be the first successful translation, so the
			// service need not return a provider error for another reader's failure.
			if appErr != nil {
				require.Equal(t, http.StatusBadGateway, appErr.StatusCode)
			}
			expectedCalls := int32(1)
			if phase == "translation" {
				expectedCalls = 3 // detection, one success, one canceled request
			}
			require.Equal(t, expectedCalls, calls.Load(), "exhaustion must stop new provider requests for the remaining languages")
			require.Len(t, post.Metadata.Translations, len(languages))
			unfinished, ready := 0, 0
			for _, language := range languages {
				stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, language)
				require.NoError(t, err, language)
				require.Equal(t, string(stored.State), post.Metadata.Translations[language].State)
				if stored.State == model.TranslationStateUnavailable {
					unfinished++
					require.Empty(t, stored.Text)
				} else if stored.State == model.TranslationStateReady {
					ready++
					require.Equal(t, "A completed synthetic translation.", stored.Text)
				}
			}
			if phase == "detection" {
				require.Equal(t, len(languages), unfinished)
			} else {
				require.Equal(t, 1, ready, "a completed language must survive another language exhausting the budget")
				require.Equal(t, len(languages)-2, unfinished) // one ready, English skipped
			}

			// Exercise the actual recovery Job after its normal minimum age,
			// using a new service so retry does not depend on request memory.
			healthy.Store(true)
			_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET updateat=$2 WHERE objectid=$1 AND state='unavailable'`, post.Id, time.Now().Add(-time.Hour).UnixMilli())
			require.NoError(t, err)
			runRecoveryWorker(t, th, New(th.Server))
			for _, language := range languages {
				stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, language)
				require.NoError(t, err, language)
				if language == "en" {
					require.Equal(t, model.TranslationStateSkipped, stored.State)
				} else {
					require.Equal(t, model.TranslationStateReady, stored.State, language)
				}
			}
		})
	}
}

func TestForegroundProviderBudgetNativeCreateAndEdit(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		<-r.Context().Done()
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	languages := addBudgetLanguages(t, th)
	_, _, err := th.SystemAdminClient.PatchChannel(th.Context.Context(), th.BasicChannel.Id, &model.ChannelPatch{AutoTranslation: new(true)})
	require.NoError(t, err)
	socket := th.CreateConnectedWebSocketClientWithClient(t, th.Client)
	var post *model.Post
	for _, operation := range []string{"create", "edit"} {
		t.Run(operation, func(t *testing.T) {
			before := calls.Load()
			started := time.Now()
			kind := model.WebsocketEventPosted
			if operation == "create" {
				post, _, err = th.Client.CreatePost(th.Context.Context(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "A synthetic native create."})
			} else {
				post, _, err = th.Client.PatchPost(th.Context.Context(), post.Id, &model.PostPatch{Message: new("A synthetic native edit.")})
				kind = model.WebsocketEventPostEdited
			}
			require.NoError(t, err, "provider exhaustion must not fail the saved post")
			t.Logf("native %s: %s, requests=%d", operation, time.Since(started), calls.Load()-before)
			require.EqualValues(t, 1, calls.Load()-before)
			eventPost := nextLivePost(t, socket, kind, post.Id)
			require.Equal(t, post.Message, eventPost.Message)
			require.Len(t, eventPost.Metadata.Translations, len(languages))
			for _, language := range languages {
				require.Equal(t, string(model.TranslationStateUnavailable), eventPost.Metadata.Translations[language].State)
			}
		})
	}
}
