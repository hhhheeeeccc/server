package openid_logout

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
)

const (
	scriptRoute            = "/workspace-enterprise/openid-logout.js"
	metadataRoute          = "/api/v4/workspace-enterprise/openid/logout-metadata"
	discoveryResponseLimit = 1024 * 1024
	discoveryCacheTTL      = 15 * time.Minute
	maxHTMLResponseBytes   = 8 << 20
	maxDecodedHTMLBytes    = 16 << 20
)

type Service struct {
	app    *app.App
	client *http.Client

	cacheMu sync.RWMutex
	cache   discoveryCache
}

type discoveryCache struct {
	discoveryEndpoint  string
	endSessionEndpoint string
	expiresAt          time.Time
}

type logoutSettings struct {
	DiscoveryEndpoint string `json:"discoveryEndpoint"`
	ClientID          string `json:"clientId"`
	BasePath          string `json:"basePath"`
	UserAPI           string `json:"userApi"`
	LoginAPI          string `json:"loginApi"`
	LogoutAPI         string `json:"logoutApi"`
	MetadataAPI       string `json:"metadataApi"`
}

type discoveryDocument struct {
	EndSessionEndpoint string `json:"end_session_endpoint"`
}

type bufferedResponseWriter struct {
	header    http.Header
	status    int
	body      bytes.Buffer
	dst       http.ResponseWriter
	streaming bool
}

func Register(a *app.App) *Service {
	service := &Service{
		app: a,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
	a.Srv().Router.Handle(scriptRoute, http.HandlerFunc(service.serveScript)).Methods(http.MethodGet)
	a.Srv().Router.Handle(metadataRoute, http.HandlerFunc(service.serveLogoutMetadata)).Methods(http.MethodGet)
	a.Srv().Router.Use(service.injectLogoutClient)
	return service
}

func (s *Service) settings() (logoutSettings, bool) {
	cfg := s.app.Config()
	subpath, err := utils.GetSubpathFromConfig(cfg)
	if err != nil {
		return logoutSettings{}, false
	}
	clientID := ""
	if cfg.OpenIdSettings.Id != nil {
		clientID = *cfg.OpenIdSettings.Id
	}

	settings := logoutSettings{
		DiscoveryEndpoint: model.SafeDereference(cfg.OpenIdSettings.DiscoveryEndpoint),
		ClientID:          clientID,
		BasePath:          subpath,
		UserAPI:           path.Join(subpath, model.APIURLSuffix, "users/me"),
		LoginAPI:          path.Join(subpath, model.APIURLSuffix, "users/login"),
		LogoutAPI:         path.Join(subpath, model.APIURLSuffix, "users/logout"),
		MetadataAPI:       path.Join(subpath, metadataRoute),
	}
	license := s.app.License()
	enabled := license != nil && license.Features != nil && model.SafeDereference(license.Features.OpenId) &&
		model.SafeDereference(cfg.OpenIdSettings.Enable) && settings.DiscoveryEndpoint != ""
	return settings, enabled
}

func (s *Service) serveScript(w http.ResponseWriter, r *http.Request) {
	settings, enabled := s.settings()
	if !enabled {
		http.NotFound(w, r)
		return
	}
	script, err := clientScript(settings)
	if err != nil {
		http.Error(w, "unable to render OpenID logout client", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(script)
}

func (s *Service) serveLogoutMetadata(w http.ResponseWriter, r *http.Request) {
	settings, enabled := s.settings()
	if !enabled {
		writeJSONError(w, http.StatusForbidden, "OpenID logout is unavailable")
		return
	}

	endpoint, err := s.endSessionEndpoint(r.Context(), settings.DiscoveryEndpoint)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "Unable to load the OpenID logout endpoint")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(discoveryDocument{EndSessionEndpoint: endpoint})
}

func (s *Service) endSessionEndpoint(ctx context.Context, discoveryEndpoint string) (string, error) {
	now := time.Now()
	s.cacheMu.RLock()
	cached := s.cache
	s.cacheMu.RUnlock()
	if cached.discoveryEndpoint == discoveryEndpoint && cached.endSessionEndpoint != "" && now.Before(cached.expiresAt) {
		return cached.endSessionEndpoint, nil
	}

	endpoint, err := fetchEndSessionEndpoint(ctx, s.client, discoveryEndpoint)
	if err != nil {
		return "", err
	}
	s.cacheMu.Lock()
	s.cache = discoveryCache{
		discoveryEndpoint:  discoveryEndpoint,
		endSessionEndpoint: endpoint,
		expiresAt:          now.Add(discoveryCacheTTL),
	}
	s.cacheMu.Unlock()
	return endpoint, nil
}

func fetchEndSessionEndpoint(ctx context.Context, client *http.Client, discoveryEndpoint string) (string, error) {
	discoveryURL, err := url.Parse(discoveryEndpoint)
	if err != nil || discoveryURL.Host == "" || (discoveryURL.Scheme != "https" && discoveryURL.Scheme != "http") {
		return "", fmt.Errorf("invalid OpenID discovery endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	redirectClient := *client
	previousCheck := client.CheckRedirect
	redirectClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 || next.URL.Scheme != discoveryURL.Scheme || !strings.EqualFold(next.URL.Host, discoveryURL.Host) {
			return fmt.Errorf("OpenID discovery redirected outside its origin")
		}
		if previousCheck != nil {
			return previousCheck(next, via)
		}
		return nil
	}
	response, err := redirectClient.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("OpenID discovery returned status %d", response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, discoveryResponseLimit+1))
	if err != nil {
		return "", err
	}
	if len(body) > discoveryResponseLimit {
		return "", fmt.Errorf("OpenID discovery response exceeds size limit")
	}
	var document discoveryDocument
	if err = json.Unmarshal(body, &document); err != nil {
		return "", err
	}
	logoutURL, err := url.Parse(document.EndSessionEndpoint)
	if err != nil || logoutURL.Host == "" || (logoutURL.Scheme != "https" && logoutURL.Scheme != "http") {
		return "", fmt.Errorf("OpenID discovery response has no valid end_session_endpoint")
	}
	return logoutURL.String(), nil
}

func (s *Service) injectLogoutClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		settings, enabled := s.settings()
		if r.Method == http.MethodPost && r.URL.Path == settings.LogoutAPI {
			// An already loaded client must also stop redirecting if its license
			// or OpenID setting was removed after the document was loaded.
			w.Header().Set("X-Workspace-OpenID-Logout", "false")
			if enabled {
				if isOpenID, known := s.logoutAuthService(r); known {
					w.Header().Set("X-Workspace-OpenID-Logout", strconv.FormatBool(isOpenID))
				}
			}
			next.ServeHTTP(w, r)
			return
		}
		if !enabled {
			next.ServeHTTP(w, r)
			return
		}
		if !isHTMLNavigation(r) {
			next.ServeHTTP(w, r)
			return
		}

		buffer := &bufferedResponseWriter{header: make(http.Header), dst: w}
		next.ServeHTTP(buffer, r)
		if buffer.streaming {
			return
		}
		body := buffer.body.Bytes()
		if shouldInjectScript(buffer.status, buffer.header) {
			decoded, encode, err := decodeHTML(body, buffer.header.Get("Content-Encoding"))
			if err == nil {
				scriptURL := path.Join(settings.BasePath, scriptRoute)
				updated := injectScriptTag(decoded, scriptURL)
				if !bytes.Equal(updated, decoded) {
					if encoded, encodeErr := encode(updated); encodeErr == nil {
						body = encoded
						buffer.header.Del("ETag")
						buffer.header.Set("Content-Length", strconv.Itoa(len(body)))
					}
				}
			}
		}
		copyHeaders(w.Header(), buffer.header)
		status := buffer.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

func (s *Service) logoutAuthService(r *http.Request) (bool, bool) {
	token, _ := app.ParseAuthTokenFromRequest(r)
	if token == "" {
		return false, false
	}
	session, appErr := s.app.GetSession(token)
	if appErr != nil || session == nil || session.IsExpired() || session.UserId == "" {
		return false, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	var authService string
	err := s.app.Srv().Store().GetInternalMasterDB().QueryRowContext(ctx,
		`SELECT authservice FROM users WHERE id=$1 AND deleteat=0`, session.UserId).Scan(&authService)
	if err != nil {
		return false, false
	}
	return authService == "openid", true
}

func shouldInjectScript(status int, header http.Header) bool {
	return status < http.StatusMultipleChoices && strings.Contains(strings.ToLower(header.Get("Content-Type")), "text/html")
}

func decodeHTML(body []byte, contentEncoding string) ([]byte, func([]byte) ([]byte, error), error) {
	encoding := strings.TrimSpace(strings.ToLower(contentEncoding))
	identity := func(value []byte) ([]byte, error) { return value, nil }
	if encoding == "" || encoding == "identity" {
		if len(body) > maxDecodedHTMLBytes {
			return nil, nil, fmt.Errorf("HTML response exceeds decoded size limit")
		}
		return body, identity, nil
	}
	var reader io.Reader
	var closeReader func() error
	var encode func([]byte) ([]byte, error)
	switch encoding {
	case "gzip":
		gzipReader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		reader, closeReader = gzipReader, gzipReader.Close
		encode = func(value []byte) ([]byte, error) {
			var output bytes.Buffer
			writer := gzip.NewWriter(&output)
			if _, err := writer.Write(value); err != nil {
				return nil, err
			}
			if err := writer.Close(); err != nil {
				return nil, err
			}
			return output.Bytes(), nil
		}
	case "deflate":
		zlibReader, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		reader, closeReader = zlibReader, zlibReader.Close
		encode = func(value []byte) ([]byte, error) {
			var output bytes.Buffer
			writer := zlib.NewWriter(&output)
			if _, err := writer.Write(value); err != nil {
				return nil, err
			}
			if err := writer.Close(); err != nil {
				return nil, err
			}
			return output.Bytes(), nil
		}
	case "br":
		reader = brotli.NewReader(bytes.NewReader(body))
		encode = func(value []byte) ([]byte, error) {
			var output bytes.Buffer
			writer := brotli.NewWriter(&output)
			if _, err := writer.Write(value); err != nil {
				return nil, err
			}
			if err := writer.Close(); err != nil {
				return nil, err
			}
			return output.Bytes(), nil
		}
	case "zstd":
		decoder, err := zstd.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		reader = decoder
		closeReader = func() error { decoder.Close(); return nil }
		encode = func(value []byte) ([]byte, error) {
			encoder, err := zstd.NewWriter(nil)
			if err != nil {
				return nil, err
			}
			defer encoder.Close()
			return encoder.EncodeAll(value, nil), nil
		}
	default:
		return nil, nil, fmt.Errorf("unsupported HTML content encoding")
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, maxDecodedHTMLBytes+1))
	if closeReader != nil {
		if closeErr := closeReader(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return nil, nil, err
	}
	if len(decoded) > maxDecodedHTMLBytes {
		return nil, nil, fmt.Errorf("HTML response exceeds decoded size limit")
	}
	return decoded, encode, nil
}

func isHTMLNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet || strings.HasPrefix(r.URL.Path, model.APIURLSuffix+"/") ||
		strings.HasPrefix(r.URL.Path, "/static/") || strings.HasPrefix(r.URL.Path, "/plugins/") {
		return false
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}

func injectScriptTag(body []byte, scriptURL string) []byte {
	tag := []byte(`<script defer src="` + html.EscapeString(scriptURL) + `"></script>`)
	if bytes.Contains(body, tag) {
		return body
	}
	lower := bytes.ToLower(body)
	if index := bytes.Index(lower, []byte("</head>")); index >= 0 {
		result := make([]byte, 0, len(body)+len(tag))
		result = append(result, body[:index]...)
		result = append(result, tag...)
		result = append(result, body[index:]...)
		return result
	}
	return append(append(append([]byte{}, body...), tag...), '\n')
}

func clientScript(settings logoutSettings) ([]byte, error) {
	encodedSettings, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	return []byte(`(() => {
    'use strict';
    if (window.__workspaceOpenIDLogoutInstalled || typeof window.fetch !== 'function') {
        return;
    }
    window.__workspaceOpenIDLogoutInstalled = true;
    const settings = ` + string(encodedSettings) + `;
    const nativeFetch = window.fetch;
    let currentUserUsesOpenID = null;
    let logoutDecision = null;

    const requestPath = (input) => {
        try {
            const value = typeof input === 'string' || input instanceof URL ? input : input.url;
            const requested = new URL(value, window.location.href);
            return requested.origin === window.location.origin ? requested.pathname : '';
        } catch (_) {
            return '';
        }
    };
    const requestMethod = (input, init) => String((init && init.method) || (input && input.method) || 'GET').toUpperCase();
    const readCurrentAuthService = async () => {
        try {
            const response = await nativeFetch.call(window, settings.userApi, {
                method: 'GET',
                credentials: 'same-origin',
                headers: {'Accept': 'application/json'},
            });
            if (!response.ok) {
                return false;
            }
            const user = await response.json();
            return user && user.auth_service === 'openid';
        } catch (_) {
            return false;
        }
    };
    const authDetection = readCurrentAuthService().then((isOpenID) => {
        currentUserUsesOpenID = isOpenID;
        return isOpenID;
    });

    const loadLogoutEndpoint = async () => {
        try {
            const response = await nativeFetch.call(window, settings.discoveryEndpoint, {
                method: 'GET',
                credentials: 'omit',
                headers: {'Accept': 'application/json'},
            });
            if (response.ok) {
                const document = await response.json();
                if (document && document.end_session_endpoint) {
                    return document.end_session_endpoint;
                }
            }
        } catch (_) {
            // The server-side proxy below handles providers that do not allow browser CORS.
        }
        try {
            const response = await nativeFetch.call(window, settings.metadataApi, {
                method: 'GET',
                credentials: 'same-origin',
                headers: {'Accept': 'application/json'},
            });
            if (response.ok) {
                const document = await response.json();
                return document && document.end_session_endpoint;
            }
        } catch (_) {
            // Mattermost's normal local logout remains the fallback.
        }
        return '';
    };
    const logoutEndpoint = loadLogoutEndpoint();

    const redirectToIdentityProviderLogout = async (isActive = () => true) => {
        const endpoint = await logoutEndpoint;
        if (!endpoint || !isActive()) {
            return false;
        }
        try {
            const logoutURL = new URL(endpoint);
            if (logoutURL.protocol !== 'https:' && logoutURL.protocol !== 'http:') {
                return false;
            }
            const returnPath = settings.basePath ? settings.basePath.replace(/\/$/, '') + '/' : '/';
            const returnURL = window.location.origin + returnPath;
            logoutURL.searchParams.append('post_logout_redirect_uri', returnURL);
            logoutURL.searchParams.append('redirect_uri', returnURL);
            if (settings.clientId) {
                logoutURL.searchParams.append('client_id', settings.clientId);
            }
            window.location.href = logoutURL.toString();
            return true;
        } catch (_) {
            // Mattermost has already completed its local logout.
        }
        return false;
    };

    if (typeof window.addEventListener === 'function') {
        window.addEventListener('workspace:logout-redirect', (event) => {
            if (!logoutDecision || !event.cancelable) {
                return;
            }
            let destination;
            try {
                destination = new URL(event.detail.url, window.location.href);
                if (destination.origin !== window.location.origin) {
                    return;
                }
            } catch (_) {
                return;
            }
            event.preventDefault();
            const decision = logoutDecision;
            logoutDecision = null;
            let active = true;
            const finishLocal = () => {
                if (active) {
                    active = false;
                    window.location.href = destination.toString();
                }
            };
            // Local logout has already succeeded. A stalled provider or user
            // lookup must not keep the cleaned-up application on this page.
            const timer = setTimeout(finishLocal, 6000);
            void decision.then(async (isOpenID) => {
                if (!active) return;
                const redirected = isOpenID && await redirectToIdentityProviderLogout(() => active);
                if (redirected) active = false;
                else finishLocal();
            }).catch(finishLocal).finally(() => clearTimeout(timer));
        });
    }

    window.fetch = function(input, init) {
        const pathname = requestPath(input);
        const method = requestMethod(input, init);
        const args = arguments;
        const receiver = this;

        if (pathname === settings.loginApi && method === 'POST') {
            return Reflect.apply(nativeFetch, receiver, args).then((response) => {
                if (response.ok) {
                    logoutDecision = null;
                    response.clone().json().then((user) => {
                        currentUserUsesOpenID = Boolean(user && user.auth_service === 'openid');
                    }).catch(() => {});
                }
                return response;
            });
        }
        if (pathname !== settings.logoutApi || method !== 'POST') {
            return Reflect.apply(nativeFetch, receiver, args);
        }

        const knowAuthService = currentUserUsesOpenID === null ? authDetection : Promise.resolve(currentUserUsesOpenID);
        logoutDecision = null;
        return Reflect.apply(nativeFetch, receiver, args).then((response) => {
            if (response.ok) {
                const known = response.headers && response.headers.get('X-Workspace-OpenID-Logout');
                const resolved = known === 'true' ? Promise.resolve(true) :
                    known === 'false' ? Promise.resolve(false) : knowAuthService.catch(() => false);
                logoutDecision = resolved;
                // The webapp emits its event after purging persisted state.
                // A timer here loses the provider redirect to its hard reload.
            }
            return response;
        });
    };
})();
`), nil
}

func (w *bufferedResponseWriter) Header() http.Header {
	return w.header
}

func (w *bufferedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *bufferedResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.streaming {
		return w.dst.Write(data)
	}
	if len(data) > maxHTMLResponseBytes-w.body.Len() {
		w.streaming = true
		copyHeaders(w.dst.Header(), w.header)
		w.dst.WriteHeader(w.status)
		if _, err := w.dst.Write(w.body.Bytes()); err != nil {
			return 0, err
		}
		w.body.Reset()
		return w.dst.Write(data)
	}
	return w.body.Write(data)
}

func copyHeaders(destination, source http.Header) {
	for key, values := range source {
		destination[key] = append([]string(nil), values...)
	}
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":          "workspace.openid_logout.app_error",
		"message":     message,
		"status_code": status,
	})
}
