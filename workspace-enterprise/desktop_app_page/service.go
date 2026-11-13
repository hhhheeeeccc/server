package desktop_app_page

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

//go:embed page.html
var pageHTML string

//go:embed regular.otf
var regularFont []byte

//go:embed bold.otf
var boldFont []byte

//go:embed tawn_logo.svg
var brandLogo []byte

//go:embed tawn_logo_white.svg
var whiteBrandLogo []byte

//go:embed heroImage.png
var heroImage []byte

const (
	linuxDownloadURL   = "https://play.sofa.ye/taawon/repo/7.0.0/taeawun-platform_7.0.0-1_amd64.deb"
	windowsDownloadURL = "https://play.sofa.ye/taawon/repo/7.0.0/taeawun-platform-7.0.0-setup-x64.exe"
)

var pageTemplate = template.Must(template.New("desktop-app-update").Parse(pageHTML))

type Service struct {
	app *app.App
}

type pageData struct {
	BrandName           string
	CurrentVersion      string
	MinimumVersion      string
	DownloadLink        string
	LinuxDownloadLink   string
	WindowsDownloadLink string
	HelpLink            string
	LogoURL             string
	WhiteLogoURL        string
	HeroImageURL        string
	RegularFontURL      string
	BoldFontURL         string
	CopyrightYear       int
}

func Register(a *app.App) {
	s := &Service{app: a}
	a.Srv().Router.Use(s.middleware)
}

func (s *Service) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.app.Config()
		subpath, err := utils.GetSubpathFromConfig(cfg)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		root := strings.TrimRight(subpath, "/") + "/"
		if r.Method == http.MethodGet && s.app.License() != nil {
			switch r.URL.Path {
			case root + "workspace-desktop-brand.svg":
				serveAsset(w, brandLogo, "image/svg+xml")
				return
			case root + "workspace-desktop-brand-white.svg":
				serveAsset(w, whiteBrandLogo, "image/svg+xml")
				return
			case root + "workspace-desktop-hero.png":
				serveAsset(w, heroImage, "image/png")
				return
			case root + "workspace-desktop-font/regular.otf":
				serveFont(w, regularFont)
				return
			case root + "workspace-desktop-font/bold.otf":
				serveFont(w, boldFont)
				return
			}
		}
		if r.Method != http.MethodGet || r.URL.Path != root || s.app.License() == nil ||
			!web.CheckClientCompatibility(r.UserAgent()) ||
			web.CheckDesktopAppCompatibility(r.UserAgent(), cfg.ServiceSettings.MinimumDesktopAppVersion) {
			next.ServeHTTP(w, r)
			return
		}

		version, ok := app.GetDesktopAppVersion(r.UserAgent())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		data := pageData{
			BrandName:           "تعاون",
			CurrentVersion:      version,
			MinimumVersion:      model.SafeDereference(cfg.ServiceSettings.MinimumDesktopAppVersion),
			DownloadLink:        configuredDownloadLink(model.SafeDereference(cfg.NativeAppSettings.AppDownloadLink)),
			LinuxDownloadLink:   linuxDownloadURL,
			WindowsDownloadLink: windowsDownloadURL,
			HelpLink:            configuredDownloadLink(model.SafeDereference(cfg.SupportSettings.HelpLink)),
			LogoURL:             root + "workspace-desktop-brand.svg",
			WhiteLogoURL:        root + "workspace-desktop-brand-white.svg",
			HeroImageURL:        root + "workspace-desktop-hero.png",
			RegularFontURL:      root + "workspace-desktop-font/regular.otf",
			BoldFontURL:         root + "workspace-desktop-font/bold.otf",
			CopyrightYear:       time.Now().Year(),
		}
		var output bytes.Buffer
		if err := pageTemplate.Execute(&output, data); err != nil {
			s.app.Log().Error("Unable to render desktop update page")
			http.Error(w, "Unable to render update page", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Language", "ar")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(output.Bytes())
	})
}

// The upstream default downloads Mattermost, which is not the Cooperation desktop app.
func configuredDownloadLink(link string) string {
	if strings.Contains(strings.ToLower(link), "mattermost.com") {
		return ""
	}
	return link
}

func serveFont(w http.ResponseWriter, font []byte) {
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", "font/otf")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(font)
}

func serveAsset(w http.ResponseWriter, asset []byte, contentType string) {
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(asset)
}
