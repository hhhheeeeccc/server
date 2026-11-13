package autotranslation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

// Opt-in: the runner owns the real provider and database. The only texts sent
// outside the test process are synthetic fixtures declared in this file.
func TestLibreTranslatePostLifecycle(t *testing.T) {
	providerURL := os.Getenv("WORKSPACE_TEST_LIBRETRANSLATE_URL")
	if providerURL == "" {
		t.Skip("use deployment/autotranslation/run_test.py for the real-provider test")
	}
	upstream, err := url.Parse(providerURL)
	require.NoError(t, err)
	ip := net.ParseIP(upstream.Hostname())
	require.True(t, upstream.Scheme == "http" && ip != nil && ip.IsLoopback() && upstream.Port() != "" &&
		upstream.User == nil && upstream.Path == "" && upstream.RawQuery == "" && upstream.Fragment == "")
	require.NotEmpty(t, os.Getenv("WORKSPACE_TEST_TRANSLATION_OUTPUT"))
	apiKey := os.Getenv("WORKSPACE_TEST_LIBRETRANSLATE_API_KEY")
	require.NotEmpty(t, apiKey)

	var detectCalls, translateCalls, blockedCalls atomic.Int32
	var disconnected atomic.Bool
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	// This gateway forwards actual inference requests. Its switch represents a
	// network interruption; no successful provider responses are synthesized.
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if disconnected.Load() {
			blockedCalls.Add(1)
			http.Error(w, "test network unavailable", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/detect":
			detectCalls.Add(1)
		case "/translate":
			translateCalls.Add(1)
		default:
			t.Errorf("unexpected provider route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gateway.Close)
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.EmailSettings.SendPushNotifications = new(false)
		cfg.LocalizationSettings.DefaultClientLocale = new("en")
		cfg.LocalizationSettings.DefaultServerLocale = new("en")
	}).InitBasic(t)
	ctx := context.Background()
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.AutoTranslation = new(true)
	require.True(t, th.Server.SetLicense(license))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.FeatureFlags.AutoTranslation = true
		cfg.AutoTranslationSettings.Enable = new(true)
		cfg.AutoTranslationSettings.Provider = new(providerName)
		cfg.AutoTranslationSettings.LibreTranslate.URL = new(gateway.URL)
		cfg.AutoTranslationSettings.LibreTranslate.APIKey = new(apiKey)
		cfg.AutoTranslationSettings.TimeoutMs = new(30000)
	})
	require.IsType(t, &Service{}, th.Server.AutoTranslation, "must use central registration, not a replaced server field")
	service := th.Server.AutoTranslation
	reader := th.CreateClient()
	th.LoginBasic2WithClient(t, reader)
	_, _, err = th.Client.PatchUser(ctx, th.BasicUser.Id, &model.UserPatch{Locale: new("en")})
	require.NoError(t, err)
	_, _, err = reader.PatchUser(ctx, th.BasicUser2.Id, &model.UserPatch{Locale: new("ar")})
	require.NoError(t, err)
	// The creator is a channel admin and is allowed to enable translation.
	// Use a separate creator so the author is an ordinary channel member.
	channel := th.CreateChannelWithClient(t, th.SystemAdminClient, model.ChannelTypePrivate)
	th.AddUserToChannel(t, th.BasicUser, channel)
	th.AddUserToChannel(t, th.BasicUser2, channel)

	checks := make([]string, 0)
	samples := make(map[string]string)
	t.Cleanup(func() {
		report := map[string]any{"passed": !t.Failed(), "checks": checks, "samples": samples,
			"real_detect_requests": detectCalls.Load(), "real_translate_requests": translateCalls.Load(),
			"blocked_requests": blockedCalls.Load(), "outage": "controlled HTTP gateway interruption"}
		data, marshalErr := json.MarshalIndent(report, "", "  ")
		require.NoError(t, marshalErr)
		require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("WORKSPACE_TEST_TRANSLATION_OUTPUT"), "post-lifecycle.json"), append(data, '\n'), 0o600))
	})
	phase := func(name string, run func(*testing.T)) {
		if !t.Run(name, run) {
			t.FailNow()
		}
		checks = append(checks, name)
	}
	var englishPost, arabicPost *model.Post
	var firstTranslation string
	readTranslation := func(t *testing.T, client *model.Client4, postID, language string, state model.TranslationState) string {
		t.Helper()
		list, _, readErr := client.GetPostsForChannel(ctx, channel.Id, 0, 50, "", false, false)
		require.NoError(t, readErr)
		post := list.Posts[postID]
		require.NotNil(t, post)
		require.NotNil(t, post.Metadata)
		translation := post.Metadata.Translations[language]
		require.NotNil(t, translation, "API must deliver the reader's translation")
		require.Equal(t, string(state), translation.State)
		require.Len(t, post.Metadata.Translations, 1, "history contains only the reader's language")
		return translation.Text
	}
	assertArabic := func(t *testing.T, text string) {
		t.Helper()
		require.True(t, strings.ContainsFunc(text, func(r rune) bool { return unicode.Is(unicode.Arabic, r) }), "expected actual Arabic output: %q", text)
	}
	phase("channel_permissions", func(t *testing.T) {
		_, response, patchErr := th.Client.PatchChannel(ctx, channel.Id, &model.ChannelPatch{AutoTranslation: new(true)})
		require.Error(t, patchErr)
		require.Equal(t, http.StatusForbidden, response.StatusCode)
		var patchErr2 error
		channel, _, patchErr2 = th.SystemAdminClient.PatchChannel(ctx, channel.Id, &model.ChannelPatch{AutoTranslation: new(true)})
		require.NoError(t, patchErr2)
		require.True(t, channel.AutoTranslation)
	})
	phase("real_language_detection", func(t *testing.T) {
		th.App.UpdateConfig(func(cfg *model.Config) {
			cfg.AutoTranslationSettings.LibreTranslate.APIKey = new("invalid-test-key")
		})
		_, _, appErr := service.DetectRemote(ctx, "Good morning. The project meeting starts at ten tomorrow.")
		require.NotNil(t, appErr)
		require.Equal(t, http.StatusBadGateway, appErr.StatusCode)
		require.NotContains(t, appErr.ToJSON(), apiKey)
		th.App.UpdateConfig(func(cfg *model.Config) {
			cfg.AutoTranslationSettings.LibreTranslate.APIKey = new(apiKey)
		})
		for _, fixture := range []struct{ language, text string }{
			{"en", "Good morning. The project meeting starts at ten tomorrow."},
			{"ar", "صباح الخير. سيبدأ اجتماع المشروع في الساعة العاشرة غدًا."},
		} {
			language, confidence, detectErr := service.DetectRemote(ctx, fixture.text)
			require.Nil(t, detectErr)
			require.Equal(t, fixture.language, language)
			require.NotNil(t, confidence)
		}
	})
	phase("post_creation_and_arabic_history", func(t *testing.T) {
		var createErr error
		englishPost, _, createErr = th.Client.CreatePost(ctx, &model.Post{ChannelId: channel.Id,
			Message: "Good morning. The project meeting starts at ten tomorrow."})
		require.NoError(t, createErr)
		firstTranslation = readTranslation(t, reader, englishPost.Id, "ar", model.TranslationStateReady)
		assertArabic(t, firstTranslation)
		require.NotEqual(t, englishPost.Message, firstTranslation)
		samples["english_source"], samples["arabic_translation"] = englishPost.Message, firstTranslation
		stored, storeErr := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, englishPost.Id, "ar")
		require.NoError(t, storeErr)
		require.Equal(t, firstTranslation, stored.Text)
		readTranslation(t, th.Client, englishPost.Id, "en", model.TranslationStateSkipped)
	})
	phase("stored_result_reused_without_provider", func(t *testing.T) {
		beforeDetect, beforeTranslate := detectCalls.Load(), translateCalls.Load()
		for range 2 {
			reused, appErr := service.Translate(ctx, model.TranslationObjectTypePost, englishPost.Id, channel.Id, th.BasicUser2.Id, englishPost.Message)
			require.Nil(t, appErr)
			require.Equal(t, firstTranslation, reused.Text)
			require.Equal(t, firstTranslation, readTranslation(t, reader, englishPost.Id, "ar", model.TranslationStateReady))
		}
		require.Equal(t, beforeDetect, detectCalls.Load())
		require.Equal(t, beforeTranslate, translateCalls.Load())
	})
	phase("post_edit_replaces_stale_translation", func(t *testing.T) {
		beforeTranslate := translateCalls.Load()
		var editErr error
		englishPost, _, editErr = th.Client.PatchPost(ctx, englishPost.Id, &model.PostPatch{Message: new("The meeting has been cancelled. Please stay at home today.")})
		require.NoError(t, editErr)
		updated := readTranslation(t, reader, englishPost.Id, "ar", model.TranslationStateReady)
		assertArabic(t, updated)
		require.NotEqual(t, firstTranslation, updated)
		require.Greater(t, translateCalls.Load(), beforeTranslate)
		stored, storeErr := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, englishPost.Id, "ar")
		require.NoError(t, storeErr)
		digest := sha256.Sum256([]byte(englishPost.Message))
		require.Equal(t, hex.EncodeToString(digest[:]), stored.NormHash)
		samples["edited_arabic_translation"] = updated
	})
	phase("arabic_post_delivered_in_english", func(t *testing.T) {
		var createErr error
		arabicPost, _, createErr = reader.CreatePost(ctx, &model.Post{ChannelId: channel.Id,
			Message: "صباح الخير. سيبدأ اجتماع المشروع في الساعة العاشرة غدًا."})
		require.NoError(t, createErr)
		translated := readTranslation(t, th.Client, arabicPost.Id, "en", model.TranslationStateReady)
		samples["arabic_source"], samples["english_translation"] = arabicPost.Message, translated
		require.NotEmpty(t, translated)
		require.NotEqual(t, arabicPost.Message, translated)
		require.Regexp(t, `[A-Za-z]{3,}`, translated)
		require.Contains(t, strings.ToLower(translated), "meeting", "translation must preserve the subject of the synthetic message")
		require.Contains(t, strings.ToLower(translated), "tomorrow", "translation must preserve the synthetic message's time")
		readTranslation(t, reader, arabicPost.Id, "ar", model.TranslationStateSkipped)
	})
	phase("private_channel_and_member_preferences", func(t *testing.T) {
		outsider := th.CreateClient()
		user := th.CreateUser(t)
		_, _, loginErr := outsider.Login(ctx, user.Email, user.Password)
		require.NoError(t, loginErr)
		_, response, readErr := outsider.GetPostsForChannel(ctx, channel.Id, 0, 50, "", false, false)
		require.Error(t, readErr)
		require.Equal(t, http.StatusForbidden, response.StatusCode)
		response, readErr = reader.UpdateChannelMemberAutotranslation(ctx, channel.Id, th.BasicUser.Id, true)
		require.Error(t, readErr)
		require.Equal(t, http.StatusForbidden, response.StatusCode)
		_, readErr = reader.UpdateChannelMemberAutotranslation(ctx, channel.Id, th.BasicUser2.Id, true)
		require.NoError(t, readErr)
		beforeDetect, beforeTranslate := detectCalls.Load(), translateCalls.Load()
		post, _, createErr := th.Client.CreatePost(ctx, &model.Post{ChannelId: channel.Id, Message: "Please send the project report before the next meeting."})
		require.NoError(t, createErr)
		list, _, readErr := reader.GetPostsForChannel(ctx, channel.Id, 0, 50, "", false, false)
		require.NoError(t, readErr)
		require.Empty(t, list.Posts[post.Id].Metadata.Translations)
		_, storeErr := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
		require.Error(t, storeErr)
		require.Equal(t, beforeTranslate, translateCalls.Load(), "the opted-out reader must not cause an Arabic request")
		require.Equal(t, beforeDetect+1, detectCalls.Load(), "only the English author's language remains")
		_, readErr = reader.UpdateChannelMemberAutotranslation(ctx, channel.Id, th.BasicUser2.Id, false)
		require.NoError(t, readErr)
	})
	phase("network_interruption_and_automatic_recovery", func(t *testing.T) {
		disconnected.Store(true)
		post, _, createErr := th.Client.CreatePost(ctx, &model.Post{ChannelId: channel.Id,
			Message: "The office will reopen on Monday morning. Thank you for your patience."})
		require.NoError(t, createErr, "translation failure must preserve successful posting")
		readTranslation(t, reader, post.Id, "ar", model.TranslationStateUnavailable)
		require.Positive(t, blockedCalls.Load())
		disconnected.Store(false)
		// Advance only the test row's eligibility time; run the actual job worker
		// with a fresh service instead of calling Translate explicitly.
		_, ageErr := th.Store.GetInternalMasterDB().Exec(`UPDATE translations
			SET updateat=floor(extract(epoch FROM clock_timestamp())*1000)::bigint-$2
			WHERE objectid=$1`, post.Id, (recoveryAge + time.Minute).Milliseconds())
		require.NoError(t, ageErr)
		runRecoveryWorker(t, th, New(th.Server))
		translation, storeErr := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
		require.NoError(t, storeErr)
		require.Equal(t, model.TranslationStateReady, translation.State)
		assertArabic(t, translation.Text)
		require.Equal(t, translation.Text, readTranslation(t, reader, post.Id, "ar", model.TranslationStateReady))
		samples["recovered_arabic_translation"] = translation.Text
	})
	phase("license_revocation_disables_api_and_provider", func(t *testing.T) {
		th.Server.SetLicense(nil)
		require.Nil(t, th.Server.License())
		require.False(t, service.IsFeatureAvailable())
		beforeDetect, beforeTranslate := detectCalls.Load(), translateCalls.Load()
		_, response, patchErr := th.SystemAdminClient.PatchChannel(ctx, channel.Id, &model.ChannelPatch{AutoTranslation: new(false)})
		require.Error(t, patchErr)
		require.Equal(t, http.StatusForbidden, response.StatusCode)
		response, patchErr = reader.UpdateChannelMemberAutotranslation(ctx, channel.Id, th.BasicUser2.Id, true)
		require.Error(t, patchErr)
		require.Equal(t, http.StatusForbidden, response.StatusCode)
		post, _, createErr := th.Client.CreatePost(ctx, &model.Post{ChannelId: channel.Id, Message: "This synthetic post must not be sent to the provider without a license."})
		require.NoError(t, createErr)
		list, _, readErr := reader.GetPostsForChannel(ctx, channel.Id, 0, 50, "", false, false)
		require.NoError(t, readErr)
		require.Empty(t, list.Posts[post.Id].Metadata.Translations)
		require.Empty(t, list.Posts[englishPost.Id].Metadata.Translations, "stored translations are hidden after revocation")
		_, appErr := service.Translate(ctx, model.TranslationObjectTypePost, post.Id, channel.Id, th.BasicUser2.Id, post.Message)
		require.NotNil(t, appErr)
		require.Equal(t, http.StatusNotImplemented, appErr.StatusCode)
		require.Equal(t, beforeDetect, detectCalls.Load())
		require.Equal(t, beforeTranslate, translateCalls.Load())
	})
}
