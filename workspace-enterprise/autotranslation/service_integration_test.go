package autotranslation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if path := os.Getenv(publicationProcessEnv); path != "" {
		if err := runPublicationTestProcess(path); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	app.RegisterComplianceInterface(func(a *app.App) einterfaces.ComplianceInterface {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		if err := InstallPostLifecycle(ctx, a.Srv().Store().GetInternalMasterDB()); err != nil {
			panic(err)
		}
		RegisterHistory(a)
		return nil
	})
	// Use the same registration as main.go so API tests exercise the real service.
	app.RegisterAutoTranslationInterface(func(server *app.Server) einterfaces.AutoTranslationInterface {
		RegisterPostWritesBeforeStart(server)
		return New(server)
	})
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestProviderTransportFailureDoesNotExposeConfiguredURL(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	provider.Close()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	const marker = "private-provider-query-value"
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.AutoTranslationSettings.LibreTranslate.URL = model.NewPointer(provider.URL + "/?token=" + marker)
		cfg.AutoTranslationSettings.TimeoutMs = model.NewPointer(1000)
	})

	var result []detection
	appErr := New(th.Server).callProvider(th.Context.Context(), "/detect", map[string]string{"q": "private message"}, &result)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusBadGateway, appErr.StatusCode)
	require.NotContains(t, appErr.ToJSON(), marker)
	require.NotContains(t, appErr.ToJSON(), "private message")
}

func TestTranslateDistinguishesMissingTranslationFromStoreFailure(t *testing.T) {
	var detectCalls atomic.Int32
	var translateCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/detect":
			detectCalls.Add(1)
			_, _ = w.Write([]byte(`[{"confidence":0.99,"language":"en"}]`))
		case "/translate":
			translateCalls.Add(1)
			_, _ = w.Write([]byte(`{"translatedText":"نص مترجم"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.AutoTranslation = model.NewPointer(true)
	require.True(t, th.Server.SetLicense(license))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.FeatureFlags.AutoTranslation = true
		cfg.AutoTranslationSettings.Enable = model.NewPointer(true)
		cfg.AutoTranslationSettings.Provider = model.NewPointer(providerName)
		cfg.AutoTranslationSettings.LibreTranslate.URL = model.NewPointer(provider.URL)
	})
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE Users SET Locale='ar' WHERE Id=$1`, th.BasicUser.Id)
	require.NoError(t, err)
	channel := th.BasicChannel.DeepCopy()
	channel.AutoTranslation = true
	channel, appErr := th.App.UpdateChannel(th.Context, channel)
	require.Nil(t, appErr)
	service := New(th.Server)
	content := "private text"
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET message=$1 WHERE id=$2`, content, th.BasicPost.Id)
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(content))
	require.NoError(t, th.Store.AutoTranslation().Save(&model.Translation{
		ObjectID: th.BasicPost.Id, ObjectType: model.TranslationObjectTypePost,
		ChannelID: channel.Id, Lang: "ar", Provider: providerName,
		Type: model.TranslationTypeString, Text: "previous translation",
		State: model.TranslationStateReady, NormHash: hex.EncodeToString(digest[:]),
	}))
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE Translations SET Meta='[]'::jsonb WHERE ObjectId=$1 AND ObjectType=$2 AND DstLang='ar'`, th.BasicPost.Id, model.TranslationObjectTypePost)
	require.NoError(t, err)
	_, appErr = service.Translate(th.Context.Context(), model.TranslationObjectTypePost, th.BasicPost.Id, channel.Id, th.BasicUser.Id, content)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusInternalServerError, appErr.StatusCode)
	require.Zero(t, detectCalls.Load(), "a storage failure must not send the text to the detector")
	require.Zero(t, translateCalls.Load(), "a storage failure must not resend the text to the provider")

	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE Translations SET Meta='{"type":"string"}'::jsonb WHERE ObjectId=$1 AND ObjectType=$2 AND DstLang='ar'`, th.BasicPost.Id, model.TranslationObjectTypePost)
	require.NoError(t, err)
	languages, err := th.Store.AutoTranslation().GetActiveDestinationLanguages(channel.Id, th.BasicUser.Id, nil)
	require.NoError(t, err)
	for _, language := range uniqueLanguages(languages) {
		if language == "ar" {
			continue
		}
		require.NoError(t, th.Store.AutoTranslation().Save(&model.Translation{
			ObjectID: th.BasicPost.Id, ObjectType: model.TranslationObjectTypePost,
			ChannelID: channel.Id, Lang: language, Provider: providerName,
			Type: model.TranslationTypeString, Text: "previous translation",
			State: model.TranslationStateReady, NormHash: hex.EncodeToString(digest[:]),
		}))
	}
	_, appErr = service.Translate(th.Context.Context(), model.TranslationObjectTypePost, th.BasicPost.Id, channel.Id, th.BasicUser.Id, content)
	require.Nil(t, appErr)
	require.Zero(t, detectCalls.Load(), "an unchanged ready translation does not need language detection")
	require.Zero(t, translateCalls.Load(), "an unchanged ready translation is reused")

	newPost, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: channel.Id, UserId: th.BasicUser.Id, Message: content})
	require.NoError(t, err)
	translated, appErr := service.Translate(th.Context.Context(), model.TranslationObjectTypePost, newPost.Id, channel.Id, th.BasicUser.Id, content)
	require.Nil(t, appErr)
	require.NotNil(t, translated)
	require.Equal(t, int32(1), detectCalls.Load(), "a missing translation needs language detection")
	require.Equal(t, int32(1), translateCalls.Load(), "a missing translation can be generated")
}

func TestTranslateStopsAfterLicenseRevocationDuringDetection(t *testing.T) {
	detectStarted := make(chan struct{}, 1)
	releaseDetect := make(chan struct{})
	var translateCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			detectStarted <- struct{}{}
			<-releaseDetect
			_, _ = w.Write([]byte(`[{"confidence":0.99,"language":"en"}]`))
			return
		}
		translateCalls.Add(1)
		_, _ = w.Write([]byte(`{"translatedText":"translated"}`))
	}))
	defer provider.Close()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.AutoTranslation = model.NewPointer(true)
	require.True(t, th.Server.SetLicense(license))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.FeatureFlags.AutoTranslation = true
		cfg.AutoTranslationSettings.Enable = model.NewPointer(true)
		cfg.AutoTranslationSettings.Provider = model.NewPointer(providerName)
		cfg.AutoTranslationSettings.LibreTranslate.URL = model.NewPointer(provider.URL)
	})
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE users SET locale='ar' WHERE id=$1`, th.BasicUser.Id)
	require.NoError(t, err)
	channel := th.BasicChannel.DeepCopy()
	channel.AutoTranslation = true
	channel, appErr := th.App.UpdateChannel(th.Context, channel)
	require.Nil(t, appErr)
	post, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: channel.Id, UserId: th.BasicUser.Id, Message: "message"})
	require.NoError(t, err)
	objectID := post.Id
	result := make(chan *model.AppError, 1)
	go func() {
		_, translateErr := New(th.Server).Translate(context.Background(), model.TranslationObjectTypePost, objectID, channel.Id, th.BasicUser.Id, "message")
		result <- translateErr
	}()
	select {
	case <-detectStarted:
	case <-time.After(5 * time.Second):
		close(releaseDetect)
		t.Fatal("language detection did not start")
	}
	th.Server.SetLicense(nil)
	close(releaseDetect)
	require.Nil(t, th.Server.License())
	select {
	case appErr = <-result:
		require.NotNil(t, appErr)
		require.Equal(t, http.StatusNotImplemented, appErr.StatusCode)
	case <-time.After(5 * time.Second):
		t.Fatal("translation did not stop after license revocation")
	}
	require.Zero(t, translateCalls.Load())
	var count int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM translations WHERE objectid=$1`, objectID).Scan(&count))
	require.Zero(t, count)
}

func TestTranslateDoesNotSaveAfterLicenseRevocationDuringProviderRequest(t *testing.T) {
	translateStarted := make(chan struct{}, 1)
	releaseTranslate := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			_, _ = w.Write([]byte(`[{"confidence":0.99,"language":"en"}]`))
			return
		}
		translateStarted <- struct{}{}
		<-releaseTranslate
		_, _ = w.Write([]byte(`{"translatedText":"translated"}`))
	}))
	defer provider.Close()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.AutoTranslation = model.NewPointer(true)
	require.True(t, th.Server.SetLicense(license))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.FeatureFlags.AutoTranslation = true
		cfg.AutoTranslationSettings.Enable = model.NewPointer(true)
		cfg.AutoTranslationSettings.Provider = model.NewPointer(providerName)
		cfg.AutoTranslationSettings.LibreTranslate.URL = model.NewPointer(provider.URL)
	})
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE users SET locale='ar' WHERE id=$1`, th.BasicUser.Id)
	require.NoError(t, err)
	channel := th.BasicChannel.DeepCopy()
	channel.AutoTranslation = true
	channel, appErr := th.App.UpdateChannel(th.Context, channel)
	require.Nil(t, appErr)
	post, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: channel.Id, UserId: th.BasicUser.Id, Message: "message"})
	require.NoError(t, err)
	objectID := post.Id
	result := make(chan *model.AppError, 1)
	go func() {
		_, translateErr := New(th.Server).Translate(context.Background(), model.TranslationObjectTypePost, objectID, channel.Id, th.BasicUser.Id, "message")
		result <- translateErr
	}()
	select {
	case <-translateStarted:
	case <-time.After(5 * time.Second):
		close(releaseTranslate)
		t.Fatal("translation request did not start")
	}
	th.Server.SetLicense(nil)
	close(releaseTranslate)
	require.Nil(t, th.Server.License())
	select {
	case appErr = <-result:
		require.NotNil(t, appErr)
		require.Equal(t, http.StatusNotImplemented, appErr.StatusCode)
	case <-time.After(5 * time.Second):
		t.Fatal("translation did not stop after license revocation")
	}
	var count int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM translations WHERE objectid=$1 AND dstlang='ar'`, objectID).Scan(&count))
	require.Zero(t, count)
}

func TestTranslateStopsWithoutSavingWhenRequestCanceled(t *testing.T) {
	translateStarted := make(chan struct{}, 1)
	releaseTranslate := make(chan struct{})
	var translateCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			_, _ = w.Write([]byte(`[{"confidence":0.99,"language":"en"}]`))
			return
		}
		translateCalls.Add(1)
		translateStarted <- struct{}{}
		<-releaseTranslate
		_, _ = w.Write([]byte(`{"translatedText":"late result"}`))
	}))
	defer provider.Close()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.AutoTranslation = model.NewPointer(true)
	require.True(t, th.Server.SetLicense(license))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.FeatureFlags.AutoTranslation = true
		cfg.AutoTranslationSettings.Enable = model.NewPointer(true)
		cfg.AutoTranslationSettings.Provider = model.NewPointer(providerName)
		cfg.AutoTranslationSettings.LibreTranslate.URL = model.NewPointer(provider.URL)
	})
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE users SET locale='ar' WHERE id=$1`, th.BasicUser.Id)
	require.NoError(t, err)
	channel := th.BasicChannel.DeepCopy()
	channel.AutoTranslation = true
	channel, appErr := th.App.UpdateChannel(th.Context, channel)
	require.Nil(t, appErr)
	post, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: channel.Id, UserId: th.BasicUser.Id, Message: "message"})
	require.NoError(t, err)
	objectID := post.Id
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan *model.AppError, 1)
	go func() {
		_, translateErr := New(th.Server).Translate(ctx, model.TranslationObjectTypePost, objectID, channel.Id, th.BasicUser.Id, "message")
		result <- translateErr
	}()
	select {
	case <-translateStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("translation request did not start")
	}
	cancel()
	close(releaseTranslate)
	select {
	case appErr = <-result:
		require.NotNil(t, appErr)
		require.Equal(t, http.StatusRequestTimeout, appErr.StatusCode)
	case <-time.After(5 * time.Second):
		t.Fatal("translation did not stop after request cancellation")
	}
	require.Equal(t, int32(1), translateCalls.Load())
	var count int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM translations WHERE objectid=$1 AND dstlang='ar'`, objectID).Scan(&count))
	require.Zero(t, count)
}
