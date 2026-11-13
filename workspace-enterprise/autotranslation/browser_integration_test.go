//go:build integration

package autotranslation

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestBrowserAutoTranslation(t *testing.T) {
	staticDir := os.Getenv("WORKSPACE_TEST_TRANSLATION_STATIC_DIR")
	if staticDir == "" {
		t.Skip("use deployment/autotranslation/run_test.py --static-dir with an isolated webapp build")
	}
	require.True(t, filepath.IsAbs(staticDir))
	_, err := os.Stat(filepath.Join(staticDir, "root.html"))
	require.NoError(t, err)
	providerURL, err := url.Parse(os.Getenv("WORKSPACE_TEST_LIBRETRANSLATE_URL"))
	require.NoError(t, err)
	providerProxy := httputil.NewSingleHostReverseProxy(providerURL)
	var providerUnavailable atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if providerUnavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		providerProxy.ServeHTTP(w, r)
	}))
	t.Cleanup(provider.Close)
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableOnboardingFlow = new(false)
		cfg.ServiceSettings.EnableDesktopLandingPage = new(false)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.EmailSettings.SendPushNotifications = new(false)
		cfg.LocalizationSettings.DefaultClientLocale = new("ar")
		cfg.LocalizationSettings.DefaultServerLocale = new("ar")
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.AutoTranslation = new(true)
	require.True(t, th.Server.SetLicense(license))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.FeatureFlags.AutoTranslation = true
		cfg.AutoTranslationSettings.Enable = new(true)
		cfg.AutoTranslationSettings.Provider = new(providerName)
		cfg.AutoTranslationSettings.TargetLanguages = &[]string{"en", "ar"}
		cfg.AutoTranslationSettings.LibreTranslate.URL = new(provider.URL)
		cfg.AutoTranslationSettings.LibreTranslate.APIKey = new(os.Getenv("WORKSPACE_TEST_LIBRETRANSLATE_API_KEY"))
		cfg.AutoTranslationSettings.TimeoutMs = new(30000)
	})
	require.IsType(t, &Service{}, th.Server.AutoTranslation)
	reader := th.CreateClient()
	th.LoginBasic2WithClient(t, reader)
	ctx := th.Context.Context()
	_, _, err = th.Client.PatchUser(ctx, th.BasicUser.Id, &model.UserPatch{Locale: new("en")})
	require.NoError(t, err)
	_, _, err = reader.PatchUser(ctx, th.BasicUser2.Id, &model.UserPatch{Locale: new("ar")})
	require.NoError(t, err)
	// Keep Arabic active for another member while the browser reader opts out.
	// This proves that the reader ignores a translation present in the live event.
	_, _, err = th.SystemAdminClient.PatchUser(ctx, th.SystemAdminUser.Id, &model.UserPatch{Locale: new("ar")})
	require.NoError(t, err)
	channel := th.CreateChannelWithClient(t, th.SystemAdminClient, model.ChannelTypePrivate)
	th.AddUserToChannel(t, th.BasicUser, channel)
	th.AddUserToChannel(t, th.BasicUser2, channel)
	channel, _, err = th.SystemAdminClient.PatchChannel(ctx, channel.Id, &model.ChannelPatch{AutoTranslation: new(true)})
	require.NoError(t, err)
	post, _, err := th.Client.CreatePost(ctx, &model.Post{ChannelId: channel.Id,
		Message: "The office will reopen on Monday morning. Thank you for your patience."})
	require.NoError(t, err)
	posts, _, err := reader.GetPostsForChannel(ctx, channel.Id, 0, 50, "", false, false)
	require.NoError(t, err)
	translation := posts.Posts[post.Id].Metadata.Translations["ar"]
	require.NotNil(t, translation)
	require.Equal(t, "ready", translation.State)
	require.NotEmpty(t, translation.Text)
	backend, err := url.Parse(th.Client.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(backend)
	director := proxy.Director
	proxy.Director = func(r *http.Request) { director(r); r.Header.Set("X-Forwarded-Proto", "https") }
	static := http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir)))
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/workspace-test/translation-provider":
			var input struct {
				Enabled bool `json:"enabled"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&input) != nil {
				http.Error(w, "invalid test input", http.StatusBadRequest)
				return
			}
			providerUnavailable.Store(!input.Enabled)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/workspace-test/recover-translation":
			var input struct {
				PostID string `json:"post_id"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&input) != nil || !model.IsValidId(input.PostID) {
				http.Error(w, "invalid test post", http.StatusBadRequest)
				return
			}
			providerUnavailable.Store(false)
			_, ageErr := th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET updateat=0,
				meta=meta-'workspace_retry' WHERE objectid=$1 AND channelid=$2 AND state IN ('processing','unavailable')`, input.PostID, channel.Id)
			if ageErr != nil {
				http.Error(w, "test aging failed", http.StatusInternalServerError)
				return
			}
			job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeAutoTranslationRecovery, nil)
			if appErr != nil {
				http.Error(w, "test job creation failed", http.StatusInternalServerError)
				return
			}
			New(th.Server).MakeWorker().(interface{ DoJob(*model.Job) }).DoJob(job)
			stored, jobErr := th.Store.Job().Get(th.Context, job.Id)
			if jobErr != nil || stored.Status != model.JobStatusSuccess {
				http.Error(w, "test recovery job failed", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/api/"):
			proxy.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/static/"):
			static.ServeHTTP(w, r)
		default:
			http.ServeFile(w, r, filepath.Join(staticDir, "root.html"))
		}
	}))
	t.Cleanup(front.Close)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ServiceSettings.SiteURL = new(front.URL) })
	session, appErr := th.App.GetSession(reader.AuthToken)
	require.Nil(t, appErr)
	senderSession, appErr := th.App.GetSession(th.Client.AuthToken)
	require.Nil(t, appErr)
	input, err := json.Marshal(map[string]any{
		"appOrigin": front.URL, "userId": th.BasicUser2.Id, "session": map[string]string{"token": reader.AuthToken, "csrf": session.GetCSRF()},
		"sender":   map[string]any{"userId": th.BasicUser.Id, "session": map[string]string{"token": th.Client.AuthToken, "csrf": senderSession.GetCSRF()}},
		"teamName": th.BasicTeam.Name, "channelName": channel.Name, "channelId": channel.Id,
		"postId": post.Id, "original": post.Message, "translated": translation.Text,
		"executable": os.Getenv("WORKSPACE_TEST_CHROME_EXECUTABLE"), "outputDir": os.Getenv("WORKSPACE_TEST_TRANSLATION_OUTPUT"),
	})
	require.NoError(t, err)
	script := os.Getenv("WORKSPACE_TEST_TRANSLATION_BROWSER_SCRIPT")
	require.True(t, filepath.IsAbs(script))
	browserCtx, cancel := context.WithTimeout(context.Background(), 270*time.Second)
	defer cancel()
	command := exec.CommandContext(browserCtx, "node", script)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	require.NoError(t, err, "Chromium translation verification: %s", stderr.String())
	type verifiedPost struct {
		ID       string `json:"id"`
		Original string `json:"original"`
	}
	var result struct {
		Status string `json:"status"`
		Live   struct {
			Posts []verifiedPost `json:"posts"`
		} `json:"live"`
		Recovery struct {
			Posts []verifiedPost `json:"posts"`
		} `json:"recovery"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	require.Equal(t, "passed", result.Status)
	require.Len(t, result.Live.Posts, 3, "three messages must be sent from the real editor")
	require.Len(t, result.Recovery.Posts, 2, "recovery must reach both enabled and opted-out readers")
	for _, live := range append(result.Live.Posts, result.Recovery.Posts...) {
		stored, appErr := th.App.GetSinglePost(th.Context, live.ID, false)
		require.Nil(t, appErr)
		require.Equal(t, th.BasicUser.Id, stored.UserId)
		require.Equal(t, channel.Id, stored.ChannelId)
		require.Equal(t, live.Original, stored.Message, "browser submission and edit must persist the source")
	}
	stored, appErr := th.App.GetSinglePost(th.Context, post.Id, false)
	require.Nil(t, appErr)
	require.Equal(t, post.Message, stored.Message, "display preferences must preserve the canonical source")
	member, _, err := reader.GetChannelMember(ctx, channel.Id, th.BasicUser2.Id, "")
	require.NoError(t, err)
	require.False(t, member.AutoTranslationDisabled)
	t.Logf("CHROME_TRANSLATION_RESULT %s", strings.TrimSpace(stdout.String()))
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("WORKSPACE_TEST_TRANSLATION_OUTPUT"), "browser-verification.json"), stdout.Bytes(), 0o600))
}
