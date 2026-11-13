package autotranslation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func nextLivePost(t *testing.T, socket *model.WebSocketClient, kind model.WebsocketEventType, postID string) *model.Post {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case event, open := <-socket.EventChannel:
			require.True(t, open, "websocket closed before the native post event")
			if event.EventType() != kind {
				continue
			}
			raw, ok := event.GetData()["post"].(string)
			require.True(t, ok, "native event must contain serialized post")
			var post model.Post
			require.NoError(t, json.Unmarshal([]byte(raw), &post))
			if post.Id == postID {
				return &post
			}
		case <-timeout.C:
			t.Fatal("native post event did not reach an authenticated websocket")
			return nil
		}
	}
}

func TestLivePostCachedTranslationsPopulateMetadataWithoutProvider(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
			return
		}
		_, _ = w.Write([]byte(`{"translatedText":"ترجمة محفوظة للرسالة."}`))
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	post := saveForegroundPost(t, th, "A source with reusable translations.")
	service := New(th.Server)
	_, appErr := service.Translate(th.Context.Context(), model.TranslationObjectTypePost, post.Id, post.ChannelId, post.UserId, post)
	require.Nil(t, appErr)
	require.NotEmpty(t, post.Metadata.Translations["ar"].Text)
	before := calls.Load()
	require.Positive(t, before)
	// A new object, as used by a later edit path, starts without metadata.
	cachedPost := post.Clone()
	cachedPost.Metadata = nil
	_, appErr = service.Translate(th.Context.Context(), model.TranslationObjectTypePost, post.Id, post.ChannelId, post.UserId, cachedPost)
	require.Nil(t, appErr)
	require.Equal(t, post.Metadata.Translations, cachedPost.Metadata.Translations)
	require.Equal(t, before, calls.Load(), "reusing ready rows must not send the source to the provider again")
}

func TestLivePostIneligibleResultsClearOldTranslationMetadata(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	post := saveForegroundPost(t, th, "Current live source.")
	license := th.Server.License()
	for _, state := range []string{"license", "configuration", "canceled", "superseded", "blank", "burn_on_read", "system"} {
		t.Run(state, func(t *testing.T) {
			require.True(t, th.Server.SetLicense(license))
			th.App.UpdateConfig(func(cfg *model.Config) { cfg.AutoTranslationSettings.Enable = new(true) })
			_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET message=$1 WHERE id=$2`, post.Message, post.Id)
			require.NoError(t, err)
			candidate := post.Clone()
			candidate.Metadata = &model.PostMetadata{Translations: map[string]*model.PostTranslation{
				"ar": {State: "ready", Text: "prior private translated text"},
			}}
			metadata := candidate.Metadata
			ctx := th.Context.Context()
			switch state {
			case "license":
				th.Server.RemoveLicense()
			case "configuration":
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.AutoTranslationSettings.Enable = new(false) })
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			case "superseded":
				_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET message='Newer source.' WHERE id=$1`, post.Id)
				require.NoError(t, err)
			case "blank":
				candidate.Message = "   "
				_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET message=$1 WHERE id=$2`, candidate.Message, post.Id)
				require.NoError(t, err)
			case "burn_on_read":
				candidate.Type = model.PostTypeBurnOnRead
			case "system":
				candidate.Type = model.PostTypeJoinChannel
			}
			translation, _ := New(th.Server).Translate(ctx, model.TranslationObjectTypePost, post.Id, post.ChannelId, post.UserId, candidate)
			require.Nil(t, translation)
			require.NotSame(t, metadata, candidate.Metadata, "a shallow post copy must not mutate shared metadata")
			require.Equal(t, "prior private translated text", metadata.Translations["ar"].Text)
			require.Empty(t, candidate.Metadata.Translations)
			require.Zero(t, calls.Load(), "ineligible live content must not reach the provider")
		})
	}
}

func TestLivePostProviderFailureHasNoOldPayloadOrProviderDetails(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("private provider failure payload"))
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	post := saveForegroundPost(t, th, "A source whose provider fails.")
	post.Metadata = &model.PostMetadata{Translations: map[string]*model.PostTranslation{
		"ar": {State: "ready", Text: "prior private translation"},
	}}
	_, appErr := New(th.Server).Translate(th.Context.Context(), model.TranslationObjectTypePost, post.Id, post.ChannelId, post.UserId, post)
	require.NotNil(t, appErr)
	require.NotEmpty(t, post.Metadata.Translations)
	for _, translation := range post.Metadata.Translations {
		require.Equal(t, string(model.TranslationStateUnavailable), translation.State)
		require.Empty(t, translation.Text)
	}
	encoded, err := post.ToJSON()
	require.NoError(t, err)
	require.NotContains(t, encoded, "prior private translation")
	require.NotContains(t, encoded, "private provider failure payload")
	require.NotContains(t, encoded, provider.URL)
}

func TestLivePostRevocationDuringProviderRequestClearsMetadata(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	releaseProvider := sync.OnceFunc(func() { close(release) })
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
			return
		}
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"translatedText":"delayed text after revocation"}`))
	}))
	t.Cleanup(provider.Close)
	t.Cleanup(releaseProvider)
	th := setupRecovery(t, provider.URL)
	post := saveForegroundPost(t, th, "A source pending when its license is revoked.")
	post.Metadata = &model.PostMetadata{Translations: map[string]*model.PostTranslation{
		"ar": {State: "ready", Text: "prior private translation"},
	}}
	finished := make(chan *model.AppError, 1)
	go func() {
		_, appErr := New(th.Server).Translate(th.Context.Context(), model.TranslationObjectTypePost, post.Id, post.ChannelId, post.UserId, post)
		finished <- appErr
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("translation did not reach the provider")
	}
	th.Server.RemoveLicense()
	releaseProvider()
	select {
	case appErr := <-finished:
		require.NotNil(t, appErr)
	case <-time.After(5 * time.Second):
		t.Fatal("translation did not stop after license revocation")
	}
	require.Empty(t, post.Metadata.Translations)
}

// Both sockets are authenticated before the write. This covers the live path,
// rather than proving translation availability through a later history read.
func TestLivePostCreationAndEditIncludeTranslations(t *testing.T) {
	for _, operation := range []string{"create", "edit"} {
		t.Run(operation, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/detect" {
					_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
					return
				}
				_, _ = w.Write([]byte(`{"translatedText":"سيبدأ الاجتماع غدًا."}`))
			}))
			t.Cleanup(provider.Close)
			th := setupRecovery(t, provider.URL)
			reader := th.CreateClient()
			th.LoginBasic2WithClient(t, reader)
			_, _, err := th.Client.PatchUser(th.Context.Context(), th.BasicUser.Id, &model.UserPatch{Locale: new("en")})
			require.NoError(t, err)
			_, _, err = reader.PatchUser(th.Context.Context(), th.BasicUser2.Id, &model.UserPatch{Locale: new("ar")})
			require.NoError(t, err)
			_, _, err = th.SystemAdminClient.PatchChannel(th.Context.Context(), th.BasicChannel.Id, &model.ChannelPatch{AutoTranslation: new(true)})
			require.NoError(t, err)

			var post *model.Post
			kind := model.WebsocketEventPosted
			if operation == "edit" {
				post, _, err = th.Client.CreatePost(th.Context.Context(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "Original meeting time."})
				require.NoError(t, err)
				kind = model.WebsocketEventPostEdited
			}
			authorSocket := th.CreateConnectedWebSocketClientWithClient(t, th.Client)
			readerSocket := th.CreateConnectedWebSocketClientWithClient(t, reader)
			if operation == "create" {
				post, _, err = th.Client.CreatePost(th.Context.Context(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "The meeting starts tomorrow."})
			} else {
				post, _, err = th.Client.PatchPost(th.Context.Context(), post.Id, &model.PostPatch{Message: new("The meeting starts tomorrow.")})
			}
			require.NoError(t, err)
			stored, storeErr := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
			require.NoError(t, storeErr)
			require.Equal(t, model.TranslationStateReady, stored.State, "provider and persistence must succeed before checking live delivery")
			require.Equal(t, "سيبدأ الاجتماع غدًا.", stored.Text)
			for _, recipient := range []struct {
				name, language string
				socket         *model.WebSocketClient
			}{
				{"author", "en", authorSocket},
				{"reader", "ar", readerSocket},
			} {
				t.Run(recipient.name, func(t *testing.T) {
					received := nextLivePost(t, recipient.socket, kind, post.Id)
					require.NotNil(t, received.Metadata)
					t.Logf("native %s reached %s; translations=%d; stored Arabic state=%s", kind, recipient.name, len(received.Metadata.Translations), stored.State)
					translation := received.Metadata.Translations[recipient.language]
					if !assert.NotNil(t, translation, "a connected recipient needs its translation in the native live post") {
						return
					}
					if recipient.language == "ar" {
						assert.Equal(t, string(model.TranslationStateReady), translation.State)
						assert.Equal(t, stored.Text, translation.Text)
					} else {
						assert.Equal(t, string(model.TranslationStateSkipped), translation.State)
					}
				})
			}
		})
	}
}
