package autotranslation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

const (
	historyETagPrefix  = `"workspace-translations-`
	historyBufferLimit = 8 << 20
)

// RegisterHistory keeps conditional history reads behind the native
// authentication, permissions, filtering, and translation preparation. The
// core's timestamp validator can otherwise return 304 before GetBatch runs.
func RegisterHistory(a *app.App) {
	a.Srv().Router.Use(New(a.Srv()).historyETag)
}

func translationHistoryRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	// Match the native handler: only a positive since selects incremental
	// history. Empty, zero, and negative values fall through to page/before/
	// after reads, which can otherwise return 304 before translations load.
	if raw := r.URL.Query().Get("since"); raw != "" {
		since, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || since > 0 {
			return false
		}
	}
	_, rest, found := strings.Cut(r.URL.Path, model.APIURLSuffix+"/channels/")
	parts := strings.Split(rest, "/")
	return found && len(parts) == 2 && model.IsValidId(parts[0]) && parts[1] == "posts"
}

func (s *Service) historyETag(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		condition := r.Header.Get(model.HeaderEtagClient)
		// A previously issued validator remains scoped after license/config
		// revocation, so it cannot preserve a formerly translated response.
		if !translationHistoryRequest(r) || (!s.IsFeatureAvailable() && !strings.Contains(condition, historyETagPrefix)) {
			next.ServeHTTP(w, r)
			return
		}
		fresh := r.Clone(r.Context())
		fresh.Header.Del(model.HeaderEtagClient)
		response := &historyResponse{dst: w, header: w.Header().Clone(), status: http.StatusOK}
		next.ServeHTTP(response, fresh)
		if response.streaming {
			return
		}
		if response.status == http.StatusOK {
			// Hash the actual representation, including its query and encoding.
			// This detects same-millisecond writes, deletes, locale/preferences,
			// and reconciliation without depending on replica/cache timestamps.
			hash := sha256.New()
			_, _ = hash.Write([]byte(r.URL.RequestURI() + "\x00" + response.header.Get("Content-Encoding") + "\x00"))
			_, _ = hash.Write(response.body.Bytes())
			etag := historyETagPrefix + hex.EncodeToString(hash.Sum(nil)) + `"`
			response.header.Set(model.HeaderEtagServer, etag)
			response.header.Set("Cache-Control", "private, no-cache")
			if historyETagMatches(condition, etag) {
				response.status = http.StatusNotModified
				response.header.Del("Content-Length")
				response.body.Reset()
			}
		}
		response.commit()
	})
}

func historyETagMatches(condition, etag string) bool {
	for _, candidate := range strings.Split(condition, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// Only bounded history responses are retained. Larger responses (or a handler
// flush) pass through in full without a validator or conditional short circuit.
type historyResponse struct {
	dst       http.ResponseWriter
	header    http.Header
	body      bytes.Buffer
	status    int
	wrote     bool
	streaming bool
}

func (w *historyResponse) Header() http.Header { return w.header }

func (w *historyResponse) WriteHeader(status int) {
	if !w.wrote {
		w.status, w.wrote = status, true
	}
}

func (w *historyResponse) Write(data []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if !w.streaming && len(data) > historyBufferLimit-w.body.Len() {
		if err := w.startStreaming(); err != nil {
			return 0, err
		}
	}
	if w.streaming {
		return w.dst.Write(data)
	}
	return w.body.Write(data)
}

func (w *historyResponse) copyHeader() {
	for key := range w.dst.Header() {
		w.dst.Header().Del(key)
	}
	for key, values := range w.header {
		w.dst.Header()[key] = append([]string(nil), values...)
	}
}

func (w *historyResponse) commit() error {
	w.copyHeader()
	w.dst.WriteHeader(w.status)
	_, err := w.dst.Write(w.body.Bytes())
	return err
}

func (w *historyResponse) startStreaming() error {
	if w.streaming {
		return nil
	}
	w.header.Del(model.HeaderEtagServer)
	w.header.Set("Cache-Control", "private, no-store")
	err := w.commit()
	w.body.Reset()
	w.streaming = true
	return err
}

func (w *historyResponse) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	_ = w.startStreaming()
	_ = http.NewResponseController(w.dst).Flush()
}

func (w *historyResponse) Unwrap() http.ResponseWriter { return w.dst }
