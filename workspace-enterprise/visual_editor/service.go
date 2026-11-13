package visual_editor

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const clientSetting = "WorkspaceVisualEditorEnabled"
const settingsRoute = model.APIURLSuffix + "/workspace/visual_editor/settings"

type Settings struct {
	Enabled bool `json:"Enabled"`
}

type Service struct {
	app      *app.App
	filename string
	mu       sync.RWMutex
	settings Settings
	notify   func(Settings)
}

func (s *Service) current() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

// Register runs from the central Enterprise registration. Client reads use the
// in-memory snapshot; only an authorized admin update writes the settings file.
func Register(a *app.App, settingsPath string) {
	settings, err := loadSettings(settingsPath)
	if err != nil {
		a.Log().Error("Visual editor settings could not be loaded; Enterprise override is disabled", mlog.Err(err))
		return
	}
	subpath, err := utils.GetSubpathFromConfig(a.Config())
	if err != nil {
		a.Log().Error("Visual editor client route could not be resolved", mlog.Err(err))
		return
	}
	s := &Service{app: a, filename: settingsPath, settings: settings}
	s.notify = func(updated Settings) {
		config := a.Srv().Platform().ClientConfigWithComputed()
		config[clientSetting] = strconv.FormatBool(updated.Enabled && a.License() != nil)
		event := model.NewWebSocketEvent(model.WebsocketEventConfigChanged, "", "", "", nil, "")
		event.Add("config", config)
		a.Publish(event)
	}
	a.Srv().Router.Use(middleware(func() bool { return s.current().Enabled }, path.Join(subpath, model.APIURLSuffix, "config/client"), func() bool {
		return a.License() != nil
	}))
	for _, route := range []struct {
		method  string
		handler func(*web.Context, http.ResponseWriter, *http.Request)
	}{
		{http.MethodGet, s.getSettings},
		{http.MethodPut, s.putSettings},
	} {
		a.Srv().Router.Handle(settingsRoute, &web.Handler{
			Srv: a.Srv(), HandleFunc: route.handler,
			HandlerName: web.GetHandlerName(route.handler), RequireSession: true, RequireMfa: true,
		}).Methods(route.method)
	}
}

func loadSettings(filename string) (Settings, error) {
	file, err := os.Open(filename)
	if os.IsNotExist(err) {
		// Existing deployments may mount a config directory without this new
		// file. Keep the settings API available so an admin can configure it.
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return Settings{}, err
	}
	if len(data) > 4096 {
		return Settings{}, fmt.Errorf("visual editor settings exceed 4096 bytes")
	}
	var settings *Settings
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return Settings{}, fmt.Errorf("invalid visual editor settings: %w", err)
	}
	if settings == nil {
		return Settings{}, fmt.Errorf("visual editor settings must be a JSON object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Settings{}, fmt.Errorf("visual editor settings must contain exactly one JSON object")
	}
	return *settings, nil
}

func middleware(enabled func() bool, clientPath string, licensed func() bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != clientPath {
				next.ServeHTTP(w, r)
				return
			}
			response := &bufferedResponse{header: make(http.Header)}
			next.ServeHTTP(response, r)
			body := response.body.Bytes()
			status := response.status
			if status == 0 {
				status = http.StatusOK
			}
			if status == http.StatusOK {
				if updated, ok := updateClientBody(body, response.header.Get("Content-Encoding"), enabled() && licensed()); ok {
					body = updated
					response.header.Del("Content-Length")
					response.header.Del("ETag")
					response.header.Set("Cache-Control", "no-store")
				}
			}
			for key, values := range response.header {
				w.Header()[key] = values
			}
			w.WriteHeader(status)
			_, _ = w.Write(body)
		})
	}
}

// The core API handler may compress before the router middleware sees its body.
// Preserve that encoding so browser requests receive the override as well.
func updateClientBody(body []byte, encoding string, enabled bool) ([]byte, bool) {
	encoding = strings.ToLower(strings.TrimSpace(encoding))
	decoded := body
	switch encoding {
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, false
		}
		defer reader.Close()
		decoded, err = io.ReadAll(io.LimitReader(reader, (1<<20)+1))
		if err != nil || len(decoded) > 1<<20 {
			return nil, false
		}
	case "zstd":
		reader, err := zstd.NewReader(bytes.NewReader(body), zstd.WithDecoderMaxMemory(8<<20), zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, false
		}
		defer reader.Close()
		decoded, err = io.ReadAll(io.LimitReader(reader, (1<<20)+1))
		if err != nil || len(decoded) > 1<<20 {
			return nil, false
		}
	case "", "identity":
	default:
		return nil, false
	}
	var config map[string]string
	if err := json.Unmarshal(decoded, &config); err != nil || config == nil {
		return nil, false
	}
	config[clientSetting] = strconv.FormatBool(enabled)
	updated, err := json.Marshal(config)
	if err != nil {
		return nil, false
	}
	switch encoding {
	case "gzip":
		var result bytes.Buffer
		writer := gzip.NewWriter(&result)
		if _, err := writer.Write(updated); err != nil {
			return nil, false
		}
		if err := writer.Close(); err != nil {
			return nil, false
		}
		updated = result.Bytes()
	case "zstd":
		writer, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		if err != nil {
			return nil, false
		}
		defer writer.Close()
		updated = writer.EncodeAll(updated, nil)
	}
	return updated, true
}

// Only the finite JSON config response is buffered. Other routes, including
// websocket upgrades and downloads, go directly to their original handler.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *bufferedResponse) Header() http.Header { return w.header }
func (w *bufferedResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *bufferedResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}
