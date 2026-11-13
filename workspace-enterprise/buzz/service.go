package buzz

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/mattermost/mattermost/server/public/shared/mlog"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

const (
	buzzMessage       = "BUZZMESSAGE"
	buzzCooldownMS    = int64(180000)
	createPostAPIPath = model.APIURLSuffix + "/posts"
)

type Service struct {
	app         *app.App
	schemaMu    sync.Mutex
	schemaReady bool
}

// Register installs the Buzz cooldown around Mattermost's existing create-post
// route. The original route remains responsible for authentication,
// permissions, post validation, persistence, and WebSocket publication.
func Register(a *app.App) {
	service := &Service{app: a}
	a.Srv().Router.Use(service.enforceCooldown)
}

func (s *Service) enforceCooldown(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		channelID, ok := buzzPostRequest(r, *s.app.Config().ServiceSettings.MaximumPayloadSizeBytes)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		session, ok := s.resolveSession(r)
		if !ok {
			// Let Mattermost return its normal authentication error.
			next.ServeHTTP(w, r)
			return
		}

		if s.app.License() == nil {
			writeAppError(w, model.NewAppError(
				"WorkspaceEnterprise.BuzzCooldown",
				"api.license.upgrade_needed.app_error",
				nil,
				"",
				http.StatusForbidden,
			))
			return
		}

		rctx := request.EmptyContext(s.app.Log()).
			WithContext(r.Context()).
			With(app.RequestContextWithMaster).
			WithSession(session)
		user, appErr := s.app.GetUser(rctx, session.UserId)
		if appErr != nil {
			// The create-post handler will return the canonical user error.
			next.ServeHTTP(w, r)
			return
		}
		if user.IsSystemAdmin() {
			next.ServeHTTP(w, r)
			return
		}

		token, err := s.reserveCooldown(rctx.Context(), channelID, session.UserId)
		if err != nil {
			rctx.Logger().Warn("Unable to reserve Buzz cooldown", mlog.Err(err))
			writeAppError(w, model.NewAppError("WorkspaceEnterprise.BuzzCooldown", "api.context.internal_server_error.app_error", nil, "", http.StatusInternalServerError))
			return
		}
		if token == "" {
			writeAppError(w, model.NewAppError("CreatePost", "api.post.create_post.cooldown.app_error", nil, "Please wait 3 minutes before buzzing again", http.StatusTooManyRequests))
			return
		}

		capture := &statusResponseWriter{ResponseWriter: w}
		defer func() {
			if err := s.finishCooldown(rctx.Context(), channelID, session.UserId, token,
				capture.status >= http.StatusOK && capture.status < http.StatusMultipleChoices); err != nil {
				rctx.Logger().Warn("Unable to finalize Buzz cooldown", mlog.Err(err))
			}
		}()
		next.ServeHTTP(capture, r)

	})
}

func buzzPostRequest(r *http.Request, maxPayloadSize int64) (string, bool) {
	if r.Method != http.MethodPost || strings.TrimSuffix(r.URL.Path, "/") != createPostAPIPath || r.Body == nil {
		return "", false
	}

	originalBody := r.Body
	limit := maxPayloadSize + bytes.MinRead
	body, err := io.ReadAll(io.LimitReader(originalBody, limit+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.MultiReader(bytes.NewReader(body), originalBody), Closer: originalBody}
	if err != nil || int64(len(body)) > limit {
		return "", false
	}

	var post struct {
		ChannelID string `json:"channel_id"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(body, &post); err != nil || post.Message != buzzMessage || !model.IsValidId(post.ChannelID) {
		return "", false
	}
	return post.ChannelID, true
}

func (s *Service) resolveSession(r *http.Request) (*model.Session, bool) {
	token, _ := app.ParseAuthTokenFromRequest(r)
	if token == "" {
		return nil, false
	}
	session, appErr := s.app.GetSession(token)
	if appErr != nil || session == nil || !model.IsValidId(session.UserId) {
		return nil, false
	}
	return session, true
}

func writeAppError(w http.ResponseWriter, appErr *model.AppError) {
	requestID := model.NewId()
	appErr.RequestId = requestID
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(model.HeaderRequestId, requestID)
	w.WriteHeader(appErr.StatusCode)
	_, _ = w.Write([]byte(appErr.ToJSON()))
}
