package saml

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
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

// Supplying an isolated build renders the full webapp. The optional lightweight
// mode retains its synthetic landing page for protocol-only browser checks.
func samlBrowserOrigin(t *testing.T, f *fixture) string {
	t.Helper()
	backend, err := url.Parse(f.th.Client.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(backend)
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		director(r)
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-Host", r.Host)
	}
	staticDir := os.Getenv("WORKSPACE_TEST_SAML_STATIC_DIR")
	var static http.Handler
	if staticDir != "" {
		require.True(t, filepath.IsAbs(staticDir))
		_, err := os.Stat(filepath.Join(staticDir, "root.html"))
		require.NoError(t, err)
		static = http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir)))
		f.th.App.UpdateConfig(func(cfg *model.Config) {
			cfg.ServiceSettings.EnableOnboardingFlow = new(false)
			cfg.ServiceSettings.EnableDesktopLandingPage = new(false)
			cfg.EmailSettings.SendPushNotifications = new(false)
			cfg.LocalizationSettings.DefaultClientLocale = new("ar")
			cfg.LocalizationSettings.DefaultServerLocale = new("ar")
			cfg.SamlSettings.LoginButtonText = new("دخول SAML للاختبار")
		})
		require.Equal(t, "ar", *f.th.App.Config().LocalizationSettings.DefaultClientLocale)
		require.Equal(t, "ar", *f.th.App.Config().LocalizationSettings.DefaultServerLocale)
		team := *f.th.BasicTeam
		team.AllowOpenInvite = true
		team.DisplayName = "مجتمع اختبار SAML"
		updated, appErr := f.th.App.UpdateTeam(&team)
		require.Nil(t, appErr)
		f.th.BasicTeam = updated
	}
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if static != nil && !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/login/sso/") {
			if strings.HasPrefix(r.URL.Path, "/static/") {
				static.ServeHTTP(w, r)
			} else {
				http.ServeFile(w, r, filepath.Join(staticDir, "root.html"))
			}
			return
		}
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!doctype html><title>SAML browser fixture</title><main id='saml-test-landing'>Temporary SAML test landing</main>"))
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	// Separate the browser's app and provider hostnames as well as schemes.
	// The runner maps localhost to loopback and blocks every other origin.
	return strings.Replace(front.URL, "127.0.0.1", "localhost", 1)
}

func verifySAMLBrowser(t *testing.T, f *fixture, origin, idp, username, password, userID string) {
	verifySAMLBrowserForAccount(t, f, origin, idp, username, password, userID, nil)
}

type samlBrowserMigration struct {
	Mode        string `json:"-"`
	UserID      string `json:"userId"`
	ChannelName string `json:"channelName"`
	ChannelID   string `json:"channelId"`
	PriorPostID string `json:"priorPostId"`
	PriorText   string `json:"priorText"`
}

func verifySAMLBrowserForAccount(t *testing.T, f *fixture, origin, idp, username, password, userID string, migration *samlBrowserMigration) {
	t.Helper()
	fullWebapp := os.Getenv("WORKSPACE_TEST_SAML_STATIC_DIR") != ""
	const message = "رسالة اختبار SAML بالواجهة الكاملة"
	output := os.Getenv("WORKSPACE_TEST_SAML_BROWSER_OUTPUT")
	if migration != nil && output != "" {
		output = filepath.Join(output, "migration-"+migration.Mode)
		require.NoError(t, os.Mkdir(output, 0o700))
	}
	input, err := json.Marshal(map[string]any{"appOrigin": origin, "idpOrigin": idp,
		"username": username, "password": password, "authData": userID,
		"fullWebapp": fullWebapp, "teamName": f.th.BasicTeam.Name,
		"teamDisplayName": f.th.BasicTeam.DisplayName, "message": message,
		"migration": migration, "outputDir": output,
		"executable": os.Getenv("WORKSPACE_TEST_CHROME_EXECUTABLE")})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	script := os.Getenv("WORKSPACE_TEST_SAML_BROWSER_SCRIPT")
	require.True(t, filepath.IsAbs(script), "the isolated runner must supply an absolute browser script path")
	command := exec.CommandContext(ctx, "node", script)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	require.NoError(t, err, "Chromium SAML verification: %s", stderr.String())
	var result struct {
		Status              string `json:"status"`
		NativeSAMLPostCount int    `json:"nativeSAMLPostCount"`
		SessionAPIStatus    int    `json:"sessionApiStatus"`
		RealWebappRendered  bool   `json:"realWebappRendered"`
		MessageCreated      bool   `json:"messageCreatedThroughUI"`
		OriginalAccount     bool   `json:"originalAccountReused"`
		PriorPostReadable   bool   `json:"priorPostReadable"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	require.Equal(t, "passed", result.Status)
	require.Equal(t, 1, result.NativeSAMLPostCount)
	require.Equal(t, http.StatusOK, result.SessionAPIStatus)
	require.Equal(t, fullWebapp, result.RealWebappRendered)
	if fullWebapp {
		require.True(t, result.MessageCreated)
		user, appErr := f.th.App.GetUserByAuth(&userID, "saml")
		require.Nil(t, appErr)
		require.Equal(t, "ar", user.Locale)
		if migration != nil {
			require.Equal(t, migration.UserID, user.Id)
			require.True(t, result.OriginalAccount)
			require.True(t, result.PriorPostReadable)
			var count int
			require.NoError(t, f.th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM Posts
				WHERE ChannelId=$1 AND UserId=$2 AND Message=$3 AND DeleteAt=0`, migration.ChannelID, user.Id, message).Scan(&count))
			require.Equal(t, 1, count, "the migrated browser user must persist exactly one message")
			prior, err := f.th.Store.Post().GetSingle(f.th.Context, migration.PriorPostID, true)
			require.NoError(t, err)
			require.Equal(t, migration.UserID, prior.UserId)
			require.Equal(t, migration.PriorText, prior.Message)
		} else {
			channel, _, err := f.th.SystemAdminClient.GetChannelByName(f.th.Context.Context(), "town-square", f.th.BasicTeam.Id, "")
			require.NoError(t, err)
			posts, _, err := f.th.SystemAdminClient.GetPostsForChannel(f.th.Context.Context(), channel.Id, 0, 100, "", false, false)
			require.NoError(t, err)
			found := 0
			for _, post := range posts.Posts {
				if post.Message == message {
					require.Equal(t, user.Id, post.UserId)
					found++
				}
			}
			require.Equal(t, 1, found, "the actual webapp must create exactly one post as the SAML user")
		}
	}
	// The script emits only public status/cookie attributes, never cookie values.
	t.Logf("CHROME_SAML_RESULT %s", strings.TrimSpace(stdout.String()))
	if output != "" {
		require.NoError(t, os.WriteFile(filepath.Join(output, "browser-verification.json"), stdout.Bytes(), 0o600))
	}
}
