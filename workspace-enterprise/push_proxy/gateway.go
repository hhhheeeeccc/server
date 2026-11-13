package push_proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

// The server's largest post is 1 MiB before JSON escaping. Leave room for
// escape expansion and push metadata while bounding gateway memory use.
const maxPushRequestBytes = 8 << 20

const upstreamRequestTimeout = 25 * time.Second
const verifierTimeout = 5 * time.Second

// TokenVerifier checks a server's current or still-valid previous token.
// Implementations must compare token hashes without exposing raw tokens.
type TokenVerifier interface {
	Verify(ctx context.Context, serverID, token string) (bool, error)
}

// NewGateway protects the two push-proxy endpoints before forwarding to an
// existing proxy. It deliberately has no default verifier or bypass mode.
func NewGateway(upstream *url.URL, verifier TokenVerifier) (http.Handler, error) {
	return newGateway(upstream, verifier, upstreamRequestTimeout)
}

func newGateway(upstream *url.URL, verifier TokenVerifier, timeout time.Duration) (http.Handler, error) {
	if upstream == nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" ||
		upstream.User != nil || (upstream.Path != "" && upstream.Path != "/") || upstream.RawPath != "" ||
		upstream.Opaque != "" || upstream.RawQuery != "" || upstream.ForceQuery || upstream.Fragment != "" || verifier == nil || timeout <= 0 {
		return nil, &url.Error{Op: "configure push gateway", Err: errInvalidGatewayConfig}
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(upstream)
			// The original proxy consumes only the JSON body. Forward no client
			// headers, especially auth and address headers it may trust.
			request.Out.Header = make(http.Header)
			request.Out.Header.Set("Content-Type", "application/json")
		},
		ModifyResponse: func(response *http.Response) error {
			// A redirect sent back to Mattermost could replay its custom push
			// authentication headers to another origin.
			if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
				return errors.New("push upstream redirect refused")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "push upstream unavailable", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || (r.URL.Path != "/api/v1/send_push" && r.URL.Path != "/api/v1/ack") {
			http.NotFound(w, r)
			return
		}
		serverID := r.Header.Get("X-Mattermost-ServerID")
		token := r.Header.Get("X-Mattermost-Auth")
		if !model.IsValidId(serverID) || token == "" || strings.Contains(token, ",") ||
			len(r.Header.Values("X-Mattermost-Auth")) != 1 ||
			len(r.Header.Values("X-Mattermost-ServerID")) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		verifyCtx, cancelVerify := context.WithTimeout(r.Context(), min(verifierTimeout, timeout))
		valid, err := verifier.Verify(verifyCtx, serverID, token)
		deadlineErr := verifyCtx.Err()
		cancelVerify()
		if err != nil || deadlineErr != nil {
			http.Error(w, "push authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		if !valid {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.RawQuery != "" {
			http.Error(w, "query parameters are not supported", http.StatusBadRequest)
			return
		}
		if r.ContentLength > maxPushRequestBytes {
			http.Error(w, "push request is too large", http.StatusRequestEntityTooLarge)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPushRequestBytes))
		if err != nil {
			if _, tooLarge := err.(*http.MaxBytesError); tooLarge {
				http.Error(w, "push request is too large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "unable to read push request", http.StatusBadRequest)
			}
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		proxy.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}
