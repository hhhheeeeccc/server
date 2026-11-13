package desktop_app_page

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestOldDesktopAppUsesBrandedArabicPage(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ServiceSettings.MinimumDesktopAppVersion = new("7.0.0")
		cfg.NativeAppSettings.AppDownloadLink = new("https://mattermost.com/pl/download-apps")
		cfg.TeamSettings.SiteName = new("تعاون")
	})

	s := &Service{app: th.App}
	called := false
	handler := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("User-Agent", "Mattermost/6.4.0 Chrome/120.0.0")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.False(t, called)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "ar", response.Header().Get("Content-Language"))
	require.Contains(t, response.Body.String(), `<html lang="ar" dir="rtl">`)
	require.Contains(t, response.Body.String(), "يلزم تحديث تطبيق سطح المكتب")
	require.Contains(t, response.Body.String(), "6.4.0")
	require.Contains(t, response.Body.String(), "7.0.0")
	require.Contains(t, response.Body.String(), linuxDownloadURL)
	require.Contains(t, response.Body.String(), windowsDownloadURL)
	require.NotContains(t, response.Body.String(), "mattermost.com/pl/download-apps")
	require.Contains(t, response.Body.String(), "/workspace-desktop-font/regular.otf")
	require.Contains(t, response.Body.String(), "/workspace-desktop-brand.svg")
	require.Contains(t, response.Body.String(), "/workspace-desktop-brand-white.svg")
	require.Contains(t, response.Body.String(), "/workspace-desktop-hero.png")
	require.Contains(t, response.Body.String(), "كل مجتمع ناجح يبدأ بتواصل أفضل")
	require.Contains(t, response.Body.String(), "تعاون")
	require.NotContains(t, response.Body.String(), "SofaChat")
	require.NotContains(t, response.Body.String(), "/static/images/logo.svg")
	require.Contains(t, response.Body.String(), "#00987e")
	require.NotContains(t, response.Body.String(), "Update Required")
	require.NotContains(t, response.Body.String(), "ZgotmplZ")

	for _, ua := range []string{"Mattermost/7.0.0", "Mozilla/5.0 Chrome/120.0.0"} {
		called = false
		request = httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("User-Agent", ua)
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		require.True(t, called)
		require.Equal(t, http.StatusNoContent, response.Code)
	}

	called = false
	th.Server.SetLicense(nil)
	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("User-Agent", "Mattermost/6.4.0")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	require.True(t, called)
}

func TestDesktopAppPageServesEmbeddedFont(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))
	s := &Service{app: th.App}
	handler := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	request := httptest.NewRequest(http.MethodGet, "/workspace-desktop-font/regular.otf", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "font/otf", response.Header().Get("Content-Type"))
	require.Greater(t, len(response.Body.Bytes()), 1000)
	require.True(t, strings.Contains(response.Header().Get("Cache-Control"), "immutable"))

	request = httptest.NewRequest(http.MethodGet, "/workspace-desktop-brand.svg", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "image/svg+xml", response.Header().Get("Content-Type"))
	require.Contains(t, response.Body.String(), "<svg")

	request = httptest.NewRequest(http.MethodGet, "/workspace-desktop-brand-white.svg", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "image/svg+xml", response.Header().Get("Content-Type"))
	require.Contains(t, response.Body.String(), "<svg")

	request = httptest.NewRequest(http.MethodGet, "/workspace-desktop-hero.png", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "image/png", response.Header().Get("Content-Type"))
	require.Greater(t, len(response.Body.Bytes()), 1000)
}

func TestMattermostDownloadLinkUsesCooperationReleases(t *testing.T) {
	require.Empty(t, configuredDownloadLink("https://mattermost.com/pl/download-apps"))
	require.Equal(t, "https://example.test/download", configuredDownloadLink("https://example.test/download"))
	require.Contains(t, pageHTML, "{{.LinuxDownloadLink}}")
	require.Contains(t, pageHTML, "{{.WindowsDownloadLink}}")
}
