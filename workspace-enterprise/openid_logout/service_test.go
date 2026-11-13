package openid_logout

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func TestFetchEndSessionEndpoint(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "application/json", r.Header.Get("Accept"))
		_, _ = w.Write([]byte(`{"end_session_endpoint":"` + "http://" + r.Host + `/logout"}`))
	}))
	defer provider.Close()

	endpoint, err := fetchEndSessionEndpoint(context.Background(), provider.Client(), provider.URL+"/.well-known/openid-configuration")
	require.NoError(t, err)
	require.Equal(t, provider.URL+"/logout", endpoint)
}

func TestFetchEndSessionEndpointRejectsTrailingAndOversizedDocuments(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"trailing document", `{"end_session_endpoint":"https://identity.example/logout"} true`},
		{"oversized document", `{"end_session_endpoint":"https://identity.example/logout","padding":"` + strings.Repeat("x", discoveryResponseLimit) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer provider.Close()
			_, err := fetchEndSessionEndpoint(context.Background(), provider.Client(), provider.URL)
			require.Error(t, err)
		})
	}
}

func TestFetchEndSessionEndpointRejectsCrossOriginRedirect(t *testing.T) {
	var received atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		_, _ = w.Write([]byte(`{"end_session_endpoint":"https://identity.example/logout"}`))
	}))
	defer other.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/private", http.StatusFound)
	}))
	defer provider.Close()
	_, err := fetchEndSessionEndpoint(context.Background(), provider.Client(), provider.URL+"/discovery")
	require.ErrorContains(t, err, "outside its origin")
	require.Zero(t, received.Load())
}

func TestFetchEndSessionEndpointAllowsSameOriginRedirect(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/discovery" {
			http.Redirect(w, r, "/document", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{"end_session_endpoint":"https://identity.example/logout"}`))
	}))
	defer provider.Close()
	endpoint, err := fetchEndSessionEndpoint(context.Background(), provider.Client(), provider.URL+"/discovery")
	require.NoError(t, err)
	require.Equal(t, "https://identity.example/logout", endpoint)
}

func TestInjectScriptTag(t *testing.T) {
	original := []byte("<html><head><title>Workspace</title></head><body></body></html>")
	injected := injectScriptTag(original, "/workspace-enterprise/openid-logout.js")
	require.Contains(t, string(injected), `<script defer src="/workspace-enterprise/openid-logout.js"></script></head>`)
	require.Equal(t, injected, injectScriptTag(injected, "/workspace-enterprise/openid-logout.js"))
	mentioned := []byte("<html><head><title>openid-logout.js</title></head><body></body></html>")
	require.Contains(t, string(injectScriptTag(mentioned, "/workspace-enterprise/openid-logout.js")), `<script defer src="/workspace-enterprise/openid-logout.js"></script>`)
}

func TestBufferedHTMLResponseStreamsOversizedBodyUnchanged(t *testing.T) {
	dst := httptest.NewRecorder()
	buffer := &bufferedResponseWriter{header: make(http.Header), dst: dst}
	buffer.Header().Set("Content-Type", "text/html")
	buffer.WriteHeader(http.StatusAccepted)
	first := bytes.Repeat([]byte("a"), maxHTMLResponseBytes)
	_, err := buffer.Write(first)
	require.NoError(t, err)
	require.False(t, buffer.streaming)
	_, err = buffer.Write([]byte("bc"))
	require.NoError(t, err)
	require.True(t, buffer.streaming)
	_, err = buffer.Write([]byte("d"))
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, dst.Code)
	require.Equal(t, "text/html", dst.Header().Get("Content-Type"))
	require.Equal(t, append(first, 'b', 'c', 'd'), dst.Body.Bytes())
}

func TestShouldInjectScriptIncludesCompressedHTML(t *testing.T) {
	header := make(http.Header)
	header.Set("Content-Type", "text/html; charset=utf-8")
	require.True(t, shouldInjectScript(http.StatusOK, header))
	header.Set("Content-Encoding", "gzip")
	require.True(t, shouldInjectScript(http.StatusOK, header))
	header.Del("Content-Encoding")
	require.False(t, shouldInjectScript(http.StatusNotFound, header))
}

func TestDecodeHTMLCompressedRoundTripAndSizeLimit(t *testing.T) {
	original := []byte("<html><head></head><body>Workspace</body></html>")
	for _, test := range []struct {
		name   string
		encode func([]byte) []byte
	}{
		{name: "gzip", encode: func(value []byte) []byte {
			var output bytes.Buffer
			writer := gzip.NewWriter(&output)
			_, _ = writer.Write(value)
			require.NoError(t, writer.Close())
			return output.Bytes()
		}},
		{name: "deflate", encode: func(value []byte) []byte {
			var output bytes.Buffer
			writer := zlib.NewWriter(&output)
			_, _ = writer.Write(value)
			require.NoError(t, writer.Close())
			return output.Bytes()
		}},
		{name: "br", encode: func(value []byte) []byte {
			var output bytes.Buffer
			writer := brotli.NewWriter(&output)
			_, _ = writer.Write(value)
			require.NoError(t, writer.Close())
			return output.Bytes()
		}},
		{name: "zstd", encode: func(value []byte) []byte {
			writer, err := zstd.NewWriter(nil)
			require.NoError(t, err)
			defer writer.Close()
			return writer.EncodeAll(value, nil)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			decoded, encode, err := decodeHTML(test.encode(original), test.name)
			require.NoError(t, err)
			require.Equal(t, original, decoded)
			updated, err := encode(injectScriptTag(decoded, "/workspace-enterprise/openid-logout.js"))
			require.NoError(t, err)
			roundTrip, _, err := decodeHTML(updated, test.name)
			require.NoError(t, err)
			require.Contains(t, string(roundTrip), "openid-logout.js")
			_, _, err = decodeHTML(test.encode(bytes.Repeat([]byte("a"), maxDecodedHTMLBytes+1)), test.name)
			require.ErrorContains(t, err, "decoded size limit")
		})
	}
}

func TestClientScriptPreservesOldProjectLogoutParameters(t *testing.T) {
	script, err := clientScript(logoutSettings{
		DiscoveryEndpoint: "https://identity.example/.well-known/openid-configuration",
		ClientID:          "workspace",
		LogoutAPI:         "/api/v4/users/logout",
	})
	require.NoError(t, err)
	text := string(script)
	for _, expected := range []string{"end_session_endpoint", "post_logout_redirect_uri", "redirect_uri", "client_id", "auth_service === 'openid'"} {
		require.True(t, strings.Contains(text, expected), expected)
	}
}

const nodeLogoutEventHarness = `
const logoutListeners = new Map();
window.addEventListener = (name, listener) => logoutListeners.set(name, listener);
const finishLogout = () => {
    const event = {cancelable: true, detail: {url: 'https://workspace.example/'}, preventDefault() {this.defaultPrevented = true;}};
    logoutListeners.get('workspace:logout-redirect')?.(event);
    return event.defaultPrevented === true;
};
`

func TestClientLogoutOnlyNavigatesToHTTPProvider(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is unavailable")
	}
	script, err := clientScript(logoutSettings{
		DiscoveryEndpoint: "https://identity.example/.well-known/openid-configuration",
		ClientID:          "workspace",
		UserAPI:           "/api/v4/users/me",
		LoginAPI:          "/api/v4/users/login",
		LogoutAPI:         "/api/v4/users/logout",
		MetadataAPI:       "/api/v4/workspace-enterprise/openid/logout-metadata",
	})
	require.NoError(t, err)

	for _, tc := range []struct {
		name, endpoint, request, expected string
	}{
		{"provider URL", "https://identity.example/logout", `'/api/v4/users/logout'`, "https://identity.example/logout"},
		{"local URL object", "https://identity.example/logout", `new URL('/api/v4/users/logout', window.location.href)`, "https://identity.example/logout"},
		{"script URL", "javascript:alert(1)", `'/api/v4/users/logout'`, "https://workspace.example/"},
		{"another origin", "https://identity.example/logout", `'https://other.example/api/v4/users/logout'`, "https://workspace.example/channels/town-square"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := `const nativeTimer = setTimeout;
global.window = {
    location: {origin: 'https://workspace.example', href: 'https://workspace.example/channels/town-square'},
    fetch: async (input) => {
        if (input === '/api/v4/users/me') return {ok: true, json: async () => ({auth_service: 'openid'})};
        if (input === 'https://identity.example/.well-known/openid-configuration')
            return {ok: true, json: async () => ({end_session_endpoint: ` + strconv.Quote(tc.endpoint) + `})};
        if (String(input).includes('/api/v4/users/logout')) return {ok: true};
        throw new Error('unexpected URL: ' + input);
    },
};
` + nodeLogoutEventHarness + `
eval(` + strconv.Quote(string(script)) + `);
(async () => {
    await window.fetch(` + tc.request + `, {method: 'POST'});
    finishLogout();
    await new Promise(resolve => nativeTimer(resolve, 20));
    process.stdout.write(window.location.href);
})().catch(error => { process.stderr.write(String(error)); process.exitCode = 1; });`
			output, runErr := exec.Command("node", "-e", harness).CombinedOutput()
			require.NoError(t, runErr, string(output))
			require.True(t, strings.HasPrefix(string(output), tc.expected), string(output))
		})
	}
}

func TestClientIgnoresCrossOriginLogin(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is unavailable")
	}
	script, err := clientScript(logoutSettings{
		DiscoveryEndpoint: "https://identity.example/.well-known/openid-configuration",
		UserAPI:           "/api/v4/users/me",
		LoginAPI:          "/api/v4/users/login",
		LogoutAPI:         "/api/v4/users/logout",
	})
	require.NoError(t, err)
	harness := `const nativeTimer = setTimeout;
global.window = {
    location: {origin: 'https://workspace.example', href: 'https://workspace.example/channels/town-square'},
    fetch: async (input) => {
        if (input === '/api/v4/users/me') return {ok: true, json: async () => ({auth_service: 'email'})};
        if (input === 'https://identity.example/.well-known/openid-configuration')
            return {ok: true, json: async () => ({end_session_endpoint: 'https://identity.example/logout'})};
        if (input === 'https://other.example/api/v4/users/login')
            return {ok: true, clone: () => ({json: async () => ({auth_service: 'openid'})})};
        if (input === '/api/v4/users/logout') return {ok: true};
        throw new Error('unexpected URL: ' + input);
    },
};
` + nodeLogoutEventHarness + `
eval(` + strconv.Quote(string(script)) + `);
(async () => {
    await window.fetch('https://other.example/api/v4/users/login', {method: 'POST'});
    await window.fetch('/api/v4/users/logout', {method: 'POST'});
    finishLogout();
    await new Promise(resolve => nativeTimer(resolve, 20));
    process.stdout.write(window.location.href);
})().catch(error => { process.stderr.write(String(error)); process.exitCode = 1; });`
	output, runErr := exec.Command("node", "-e", harness).CombinedOutput()
	require.NoError(t, runErr, string(output))
	require.Equal(t, "https://workspace.example/", string(output))
}

func TestClientLogoutDoesNotWaitForUserLookup(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is unavailable")
	}
	script, err := clientScript(logoutSettings{
		DiscoveryEndpoint: "https://identity.example/.well-known/openid-configuration",
		UserAPI:           "/api/v4/users/me",
		LoginAPI:          "/api/v4/users/login",
		LogoutAPI:         "/api/v4/users/logout",
	})
	require.NoError(t, err)
	harness := `const nativeTimer = setTimeout;
let releaseLookup;
let logoutStarted = false;
global.window = {
    location: {origin: 'https://workspace.example', href: 'https://workspace.example/channels/town-square'},
    fetch: async (input) => {
        if (input === '/api/v4/users/me') return new Promise(resolve => {
            releaseLookup = () => resolve({ok: true, json: async () => ({auth_service: 'openid'})});
        });
        if (input === 'https://identity.example/.well-known/openid-configuration')
            return {ok: true, json: async () => ({end_session_endpoint: 'https://identity.example/logout'})};
        if (input === '/api/v4/users/logout') { logoutStarted = true; return {ok: true}; }
        throw new Error('unexpected URL: ' + input);
    },
};
` + nodeLogoutEventHarness + `
eval(` + strconv.Quote(string(script)) + `);
(async () => {
    const logout = window.fetch('/api/v4/users/logout', {method: 'POST'});
    if (!logoutStarted) throw new Error('local logout was delayed');
    await logout;
    if (!logoutStarted) throw new Error('local logout did not complete');
    await new Promise(resolve => nativeTimer(resolve, 20));
    if (window.location.href !== 'https://workspace.example/channels/town-square')
        throw new Error('provider navigation raced application cleanup');
    finishLogout();
    releaseLookup();
    await new Promise(resolve => nativeTimer(resolve, 20));
    process.stdout.write(window.location.href);
})().catch(error => { process.stderr.write(String(error)); process.exitCode = 1; });`
	output, runErr := exec.Command("node", "-e", harness).CombinedOutput()
	require.NoError(t, runErr, string(output))
	require.True(t, strings.HasPrefix(string(output), "https://identity.example/logout"), string(output))
}

func TestClientLogoutDeadlineDoesNotNavigateLate(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is unavailable")
	}
	script, err := clientScript(logoutSettings{
		DiscoveryEndpoint: "https://identity.example/discovery", UserAPI: "/api/v4/users/me",
		LogoutAPI: "/api/v4/users/logout",
	})
	require.NoError(t, err)
	harness := `const nativeTimer = setTimeout;
global.setTimeout = (fn, ms) => nativeTimer(fn, ms === 6000 ? 5 : ms);
let releaseDiscovery;
global.window = {
    location: {origin: 'https://workspace.example', href: 'https://workspace.example/channels/town-square'},
    fetch: async (input) => {
        if (input === '/api/v4/users/me') return new Promise(() => {});
        if (input === 'https://identity.example/discovery') return new Promise(resolve => {
            releaseDiscovery = () => resolve({ok: true, json: async () => ({end_session_endpoint: 'https://identity.example/logout'})});
        });
        if (input === '/api/v4/users/logout') return {ok: true, headers: {get: () => 'true'}};
        throw new Error('unexpected request');
    },
};
` + nodeLogoutEventHarness + `
eval(` + strconv.Quote(string(script)) + `);
(async () => {
    await window.fetch('/api/v4/users/logout', {method: 'POST'});
    if (!finishLogout()) throw new Error('cleanup event was not handled');
    await new Promise(resolve => nativeTimer(resolve, 20));
    if (window.location.href !== 'https://workspace.example/') throw new Error('local fallback did not complete');
    releaseDiscovery();
    await new Promise(resolve => nativeTimer(resolve, 20));
    if (window.location.href !== 'https://workspace.example/') throw new Error('late provider response changed the completed navigation');
    process.stdout.write('passed');
})().catch(error => {process.stderr.write(String(error));process.exitCode=1;});`
	output, runErr := exec.Command("node", "-e", harness).CombinedOutput()
	require.NoError(t, runErr, string(output))
	require.Equal(t, "passed", string(output))
}

func TestClientDoesNotTakeOverFailedLogout(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is unavailable")
	}
	script, err := clientScript(logoutSettings{UserAPI: "/api/v4/users/me", LogoutAPI: "/api/v4/users/logout"})
	require.NoError(t, err)
	harness := `global.window = {
    location: {origin: 'https://workspace.example', href: 'https://workspace.example/channels/town-square'},
    fetch: async () => ({ok: false}),
};
` + nodeLogoutEventHarness + `
eval(` + strconv.Quote(string(script)) + `);
(async () => {
    await window.fetch('/api/v4/users/logout', {method:'POST'});
    if (finishLogout()) throw new Error('failed logout must keep normal application navigation');
    process.stdout.write('passed');
})().catch(error => {process.stderr.write(String(error));process.exitCode=1;});`
	output, runErr := exec.Command("node", "-e", harness).CombinedOutput()
	require.NoError(t, runErr, string(output))
	require.Equal(t, "passed", string(output))
}

func TestClientUsesLogoutResponseWhenUserLookupStalls(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is unavailable")
	}
	script, err := clientScript(logoutSettings{
		DiscoveryEndpoint: "https://identity.example/.well-known/openid-configuration",
		UserAPI:           "/api/v4/users/me",
		LoginAPI:          "/api/v4/users/login",
		LogoutAPI:         "/api/v4/users/logout",
	})
	require.NoError(t, err)
	harness := `const nativeTimer = setTimeout;
global.window = {
    location: {origin: 'https://workspace.example', href: 'https://workspace.example/channels/town-square'},
    fetch: async (input) => {
        if (input === '/api/v4/users/me') return new Promise(() => {});
        if (input === 'https://identity.example/.well-known/openid-configuration')
            return {ok: true, json: async () => ({end_session_endpoint: 'https://identity.example/logout'})};
        if (input === '/api/v4/users/logout')
            return {ok: true, headers: {get: name => name === 'X-Workspace-OpenID-Logout' ? 'true' : null}};
        throw new Error('unexpected URL: ' + input);
    },
};
` + nodeLogoutEventHarness + `
eval(` + strconv.Quote(string(script)) + `);
(async () => {
    await window.fetch('/api/v4/users/logout', {method: 'POST'});
    finishLogout();
    await new Promise(resolve => nativeTimer(resolve, 20));
    process.stdout.write(window.location.href);
})().catch(error => { process.stderr.write(String(error)); process.exitCode = 1; });`
	output, runErr := exec.Command("node", "-e", harness).CombinedOutput()
	require.NoError(t, runErr, string(output))
	require.True(t, strings.HasPrefix(string(output), "https://identity.example/logout"), string(output))
}
