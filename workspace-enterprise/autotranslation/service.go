package autotranslation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	ejobs "github.com/mattermost/mattermost/server/v8/einterfaces/jobs"
)

const (
	providerName       = "libretranslate"
	recoveryAge        = 10 * time.Minute
	recoveryBatchSize  = 1000
	recoveryJobPeriod  = 5 * time.Minute
	maxProviderPayload = 4 << 20
)

type Service struct {
	server      *app.Server
	closed      atomic.Bool
	lifecycleMu sync.Mutex
	workContext context.Context
	cancelWork  context.CancelFunc
}

type translateRequest struct {
	Text   string `json:"q"`
	Source string `json:"source"`
	Target string `json:"target"`
	Format string `json:"format"`
	APIKey string `json:"api_key,omitempty"`
}

type translateResponse struct {
	TranslatedText   string `json:"translatedText"`
	DetectedLanguage *struct {
		Confidence float64 `json:"confidence"`
		Language   string  `json:"language"`
	} `json:"detectedLanguage"`
}

type detection struct {
	Confidence float64 `json:"confidence"`
	Language   string  `json:"language"`
}

func New(server *app.Server) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{server: server, workContext: ctx, cancelWork: cancel}
}

func (s *Service) IsFeatureAvailable() bool {
	return s.licensed() && configEnabled(s.server.Config())
}

func (s *Service) licensed() bool {
	if s == nil || s.server == nil || s.closed.Load() {
		return false
	}
	license := s.server.License()
	if license == nil || license.Features == nil || license.Features.AutoTranslation == nil || !*license.Features.AutoTranslation {
		return false
	}
	return true
}

func configEnabled(cfg *model.Config) bool {
	if cfg == nil || cfg.FeatureFlags == nil || !cfg.FeatureFlags.AutoTranslation {
		return false
	}
	settings := cfg.AutoTranslationSettings
	return settings.Enable != nil && *settings.Enable &&
		settings.Provider != nil && *settings.Provider == providerName &&
		settings.LibreTranslate != nil && settings.LibreTranslate.URL != nil &&
		strings.TrimSpace(*settings.LibreTranslate.URL) != ""
}

func (s *Service) IsChannelEnabled(channelID string) (bool, *model.AppError) {
	if !s.IsFeatureAvailable() {
		return false, nil
	}
	if !model.IsValidId(channelID) {
		return false, model.NewAppError("AutoTranslation.IsChannelEnabled", "model.channel.is_valid.id.app_error", nil, "", http.StatusBadRequest)
	}
	channel, err := s.server.Store().Channel().Get(channelID, true)
	if err != nil {
		return false, providerError("AutoTranslation.IsChannelEnabled", http.StatusInternalServerError, err)
	}
	return channel.DeleteAt == 0 && channel.AutoTranslation, nil
}

func (s *Service) IsUserEnabled(channelID, userID string) (bool, *model.AppError) {
	if !s.IsFeatureAvailable() {
		return false, nil
	}
	if !model.IsValidId(channelID) || !model.IsValidId(userID) {
		return false, model.NewAppError("AutoTranslation.IsUserEnabled", model.NoTranslation, nil, "invalid channel or user id", http.StatusBadRequest)
	}
	enabled, err := s.server.Store().AutoTranslation().IsUserEnabled(userID, channelID)
	if err != nil {
		return false, providerError("AutoTranslation.IsUserEnabled", http.StatusInternalServerError, err)
	}
	return enabled, nil
}

func (s *Service) GetUserLanguage(userID, channelID string) (string, *model.AppError) {
	if !s.IsFeatureAvailable() {
		return "", unavailableError("AutoTranslation.GetUserLanguage")
	}
	language, err := s.server.Store().AutoTranslation().GetUserLanguage(userID, channelID)
	if err != nil {
		return "", providerError("AutoTranslation.GetUserLanguage", http.StatusInternalServerError, err)
	}
	return language, nil
}

func (s *Service) GetBatch(objectType string, objectIDs []string, dstLang string) (map[string]*model.Translation, *model.AppError) {
	if !s.IsFeatureAvailable() {
		return nil, unavailableError("AutoTranslation.GetBatch")
	}
	var translations map[string]*model.Translation
	var err error
	if objectType == model.TranslationObjectTypePost {
		translations, err = s.getPostTranslations(objectIDs, dstLang)
	} else {
		translations, err = s.server.Store().AutoTranslation().GetBatch(objectType, objectIDs, dstLang)
	}
	if err != nil {
		return nil, providerError("AutoTranslation.GetBatch", http.StatusInternalServerError, err)
	}
	for _, translation := range translations {
		if translation.Provider == "" {
			translation.Provider = providerName
		}
	}
	if !s.IsFeatureAvailable() {
		return nil, unavailableError("AutoTranslation.GetBatch")
	}
	return translations, nil
}

func (s *Service) Translate(ctx context.Context, objectType, objectID, channelID, userID string, content any) (*model.Translation, *model.AppError) {
	// The native create/edit paths pass the post and then serialize that same
	// object for their WebSocket event. Never leave a prior edit's translations
	// on it if this attempt is canceled, revoked, superseded, or ineligible.
	var livePost *model.Post
	if post, ok := content.(*model.Post); ok && post != nil &&
		objectType == model.TranslationObjectTypePost && post.Id == objectID && post.ChannelId == channelID {
		livePost = post
		if livePost.Metadata != nil {
			// Post.Clone shares metadata. Keep other post copies intact when
			// replacing this attempt's translations.
			metadata := *livePost.Metadata
			metadata.Translations = nil
			livePost.Metadata = &metadata
		}
	}
	if !s.IsFeatureAvailable() {
		return nil, unavailableError("AutoTranslation.Translate")
	}
	if ctx.Err() != nil {
		return nil, canceledTranslationError()
	}
	if objectType == "" || !model.IsValidId(objectID) || !model.IsValidId(channelID) || !model.IsValidId(userID) {
		return nil, model.NewAppError("AutoTranslation.Translate", model.NoTranslation, nil, "invalid translation object, channel, or user id", http.StatusBadRequest)
	}
	// The webapp renders translations only for ordinary posts. In particular,
	// burn-on-read content must not reappear as metadata on a redacted event.
	if livePost != nil && livePost.Type != "" {
		return nil, nil
	}

	text, translationType, appErr := extractText(content)
	if appErr != nil {
		return nil, appErr
	}
	current, appErr := s.sourceCurrent(ctx, objectType, objectID, channelID, text)
	if appErr != nil || !current {
		return nil, appErr
	}
	normalized := strings.TrimSpace(text)
	if normalized == "" {
		if objectType == model.TranslationObjectTypePost {
			if err := s.skipBlankPostTranslations(ctx, objectID, channelID, text); err != nil {
				if ctx.Err() != nil {
					return nil, canceledTranslationError()
				}
				return nil, providerError("AutoTranslation.Translate", http.StatusInternalServerError, err)
			}
			s.server.Store().AutoTranslation().InvalidatePostTranslationEtag(channelID)
		}
		return nil, nil
	}
	digest := sha256.Sum256([]byte(normalized))
	normHash := hex.EncodeToString(digest[:])

	languages, err := s.server.Store().AutoTranslation().GetActiveDestinationLanguages(channelID, userID, nil)
	if err != nil {
		return nil, providerError("AutoTranslation.Translate", http.StatusInternalServerError, err)
	}
	userLanguage, err := s.server.Store().AutoTranslation().GetUserLanguage(userID, channelID)
	if err != nil {
		return nil, providerError("AutoTranslation.Translate", http.StatusInternalServerError, err)
	}
	if userLanguage != "" {
		languages = append(languages, userLanguage)
	}
	languages = uniqueLanguages(languages)
	if len(languages) == 0 {
		return nil, nil
	}

	var requested *model.Translation
	var requestedErr *model.AppError
	postTranslations := make(map[string]*model.PostTranslation, len(languages))
	finalize := func() (bool, *model.AppError) {
		if ctx.Err() != nil {
			return false, canceledTranslationError()
		}
		if !s.IsFeatureAvailable() {
			return false, unavailableError("AutoTranslation.Translate")
		}
		current, sourceErr := s.sourceCurrent(ctx, objectType, objectID, channelID, text)
		if sourceErr != nil || !current {
			return current, sourceErr
		}
		if ctx.Err() != nil {
			return false, canceledTranslationError()
		}
		if !s.IsFeatureAvailable() {
			return false, unavailableError("AutoTranslation.Translate")
		}
		if livePost != nil && len(postTranslations) > 0 {
			if livePost.Metadata == nil {
				livePost.Metadata = &model.PostMetadata{}
			}
			// Native channel events carry the active languages together; the
			// reader chooses its locale and opt-out in the webapp. HTTP history
			// continues to fetch only its authenticated reader's language.
			livePost.Metadata.Translations = postTranslations
		}
		return true, nil
	}
	pending := make([]string, 0, len(languages))
	for _, language := range languages {
		existing, getErr := s.server.Store().AutoTranslation().Get(objectType, objectID, language)
		if getErr != nil {
			var missing *store.ErrNotFound
			if !errors.As(getErr, &missing) {
				return nil, providerError("AutoTranslation.Translate", http.StatusInternalServerError, getErr)
			}
		}
		if getErr == nil && existing != nil && existing.NormHash == normHash &&
			(existing.State == model.TranslationStateReady || existing.State == model.TranslationStateSkipped) {
			if existing.Provider == "" {
				existing.Provider = providerName
			}
			if language == userLanguage {
				requested = existing
			}
			postTranslations[language] = existing.ToPostTranslation()
			continue
		}
		pending = append(pending, language)
	}
	if len(pending) == 0 {
		current, appErr = finalize()
		if appErr != nil || !current {
			return nil, appErr
		}
		return requested, nil
	}

	// Native create/edit waits for Translate before broadcasting the post.
	// Share one provider budget across detection and all destination languages
	// so a slow provider cannot multiply that wait by the number of readers.
	// Keep persistence on the caller context: budget exhaustion is a provider
	// failure to retry, whereas caller cancellation must not save late results.
	providerCtx, cancelProvider := context.WithTimeout(ctx, s.timeout())
	defer cancelProvider()
	sourceLanguage, confidence, _ := s.DetectRemote(providerCtx, normalized)
	if ctx.Err() != nil {
		return nil, canceledTranslationError()
	}
	for _, language := range pending {
		if ctx.Err() != nil {
			return nil, canceledTranslationError()
		}
		if !s.IsFeatureAvailable() {
			return nil, unavailableError("AutoTranslation.Translate")
		}
		current, appErr = s.sourceCurrent(ctx, objectType, objectID, channelID, text)
		if appErr != nil || !current {
			return nil, appErr
		}
		translation := &model.Translation{
			ObjectID:   objectID,
			ObjectType: objectType,
			ChannelID:  channelID,
			Lang:       language,
			Provider:   providerName,
			Type:       translationType,
			Confidence: confidence,
			NormHash:   normHash,
			Meta:       map[string]any{"src_lang": sourceLanguage},
		}
		if sameLanguage(sourceLanguage, language) {
			translation.State = model.TranslationStateSkipped
		} else {
			var translatedText, detectedLanguage string
			var detectedConfidence *float64
			var remoteErr *model.AppError
			if providerCtx.Err() != nil {
				remoteErr = model.NewAppError("AutoTranslation.Translate", model.NoTranslation, nil,
					"translation provider budget exhausted", http.StatusBadGateway)
			} else {
				translatedText, detectedLanguage, detectedConfidence, remoteErr = s.translateRemote(providerCtx, normalized, language)
			}
			if detectedLanguage != "" {
				translation.Meta["src_lang"] = detectedLanguage
			}
			if detectedConfidence != nil {
				translation.Confidence = detectedConfidence
			}
			if remoteErr != nil {
				translation.State = model.TranslationStateUnavailable
				translation.Meta["error"] = remoteErr.DetailedError
				if language == userLanguage {
					requestedErr = remoteErr
				}
			} else {
				translation.Text = translatedText
				translation.State = model.TranslationStateReady
			}
		}

		if ctx.Err() != nil {
			return nil, canceledTranslationError()
		}
		if !s.IsFeatureAvailable() {
			return nil, unavailableError("AutoTranslation.Translate")
		}
		if objectType == model.TranslationObjectTypePost {
			var saveErr error
			translation, saveErr = s.savePostTranslation(ctx, translation, text)
			if saveErr != nil {
				if ctx.Err() != nil {
					return nil, canceledTranslationError()
				}
				return nil, providerError("AutoTranslation.Translate", http.StatusInternalServerError, saveErr)
			}
			if translation == nil {
				return nil, nil
			}
		} else if saveErr := s.server.Store().AutoTranslation().Save(translation); saveErr != nil {
			return nil, providerError("AutoTranslation.Translate", http.StatusInternalServerError, saveErr)
		}
		s.server.Store().AutoTranslation().InvalidatePostTranslationEtag(channelID)
		postTranslations[language] = translation.ToPostTranslation()
		if language == userLanguage {
			requested = translation
			if translation.State == model.TranslationStateReady || translation.State == model.TranslationStateSkipped {
				requestedErr = nil
			}
		}
	}
	current, appErr = finalize()
	if appErr != nil || !current {
		return nil, appErr
	}
	if requestedErr != nil {
		return requested, requestedErr
	}
	return requested, nil
}

func extractText(content any) (string, model.TranslationType, *model.AppError) {
	switch value := content.(type) {
	case string:
		return value, model.TranslationTypeString, nil
	case *model.Post:
		if value == nil {
			return "", model.TranslationTypeString, nil
		}
		return value.Message, model.TranslationTypeString, nil
	case json.RawMessage:
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return "", "", providerError("AutoTranslation.Translate", http.StatusBadRequest, err)
		}
		return text, model.TranslationTypeString, nil
	case []byte:
		return extractText(json.RawMessage(value))
	case map[string]any:
		if text, ok := value["message"].(string); ok {
			return text, model.TranslationTypeString, nil
		}
	}
	return "", "", model.NewAppError("AutoTranslation.Translate", model.NoTranslation, nil, "unsupported translation content", http.StatusBadRequest)
}

func (s *Service) DetectRemote(ctx context.Context, text string) (string, *float64, *model.AppError) {
	if !s.IsFeatureAvailable() {
		return "", nil, unavailableError("AutoTranslation.DetectRemote")
	}
	payload := map[string]string{"q": text}
	if apiKey := s.apiKey(); apiKey != "" {
		payload["api_key"] = apiKey
	}
	var detected []detection
	if appErr := s.callProvider(ctx, "/detect", payload, &detected); appErr != nil {
		return "", nil, appErr
	}
	if len(detected) == 0 || strings.TrimSpace(detected[0].Language) == "" {
		return "", nil, model.NewAppError("AutoTranslation.DetectRemote", model.NoTranslation, nil, "translation provider returned no detected language", http.StatusBadGateway)
	}
	confidence := detected[0].Confidence
	return detected[0].Language, &confidence, nil
}

func (s *Service) translateRemote(ctx context.Context, text, destination string) (string, string, *float64, *model.AppError) {
	payload := translateRequest{
		Text:   text,
		Source: "auto",
		Target: providerLanguage(destination),
		Format: "text",
		APIKey: s.apiKey(),
	}
	var response translateResponse
	if appErr := s.callProvider(ctx, "/translate", payload, &response); appErr != nil {
		return "", "", nil, appErr
	}
	if strings.TrimSpace(response.TranslatedText) == "" {
		return "", "", nil, model.NewAppError("AutoTranslation.TranslateRemote", model.NoTranslation, nil, "translation provider returned empty text", http.StatusBadGateway)
	}
	if response.DetectedLanguage == nil {
		return response.TranslatedText, "", nil, nil
	}
	confidence := response.DetectedLanguage.Confidence
	return response.TranslatedText, response.DetectedLanguage.Language, &confidence, nil
}

func (s *Service) callProvider(ctx context.Context, path string, payload, result any) *model.AppError {
	body, err := json.Marshal(payload)
	if err != nil {
		return providerError("AutoTranslation.Provider", http.StatusInternalServerError, err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, strings.TrimRight(s.providerURL(), "/")+path, bytes.NewReader(body))
	if err != nil {
		return model.NewAppError("AutoTranslation.Provider", model.NoTranslation, nil,
			"translation provider URL is invalid", http.StatusInternalServerError)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := providerHTTPClient(request.URL).Do(request)
	if err != nil {
		// A transport error can contain the configured URL, including its query.
		// Keep it out of the error returned to the API caller.
		return model.NewAppError("AutoTranslation.Provider", model.NoTranslation, nil,
			"translation provider request failed", http.StatusBadGateway)
	}
	defer response.Body.Close()
	return decodeProviderResponse(response, result)
}

func decodeProviderResponse(response *http.Response, result any) *model.AppError {
	// Provider error bodies can echo the request text or API key. Never include
	// that body in an API error returned to the caller.
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return model.NewAppError("AutoTranslation.Provider", model.NoTranslation, nil,
			fmt.Sprintf("translation provider returned status %d", response.StatusCode), http.StatusBadGateway)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProviderPayload+1))
	if err != nil || len(body) > maxProviderPayload {
		return model.NewAppError("AutoTranslation.Provider", model.NoTranslation, nil,
			"translation provider response could not be read within the size limit", http.StatusBadGateway)
	}
	if err := json.Unmarshal(body, result); err != nil {
		return model.NewAppError("AutoTranslation.Provider", model.NoTranslation, nil,
			"translation provider returned invalid JSON", http.StatusBadGateway)
	}
	return nil
}

func providerHTTPClient(origin *url.URL) *http.Client {
	return &http.Client{CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many translation provider redirects")
		}
		if !strings.EqualFold(request.URL.Scheme, origin.Scheme) || !strings.EqualFold(request.URL.Host, origin.Host) {
			return errors.New("translation provider redirected to a different origin")
		}
		return nil
	}}
}

func (s *Service) providerURL() string {
	return strings.TrimSpace(*s.server.Config().AutoTranslationSettings.LibreTranslate.URL)
}

func (s *Service) apiKey() string {
	settings := s.server.Config().AutoTranslationSettings.LibreTranslate
	if settings == nil || settings.APIKey == nil {
		return ""
	}
	return *settings.APIKey
}

func (s *Service) timeout() time.Duration {
	return boundedProviderTimeout(s.server.Config().AutoTranslationSettings.TimeoutMs)
}

func boundedProviderTimeout(timeoutMs *int) time.Duration {
	if timeoutMs == nil || *timeoutMs <= 0 {
		return 5 * time.Second
	}
	if int64(*timeoutMs) > math.MaxInt64/int64(time.Millisecond) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(*timeoutMs) * time.Millisecond
}

func uniqueLanguages(languages []string) []string {
	seen := make(map[string]struct{}, len(languages))
	result := make([]string, 0, len(languages))
	for _, language := range languages {
		language = strings.TrimSpace(language)
		if language == "" {
			continue
		}
		if _, ok := seen[language]; ok {
			continue
		}
		seen[language] = struct{}{}
		result = append(result, language)
	}
	return result
}

func providerLanguage(language string) string {
	if index := strings.IndexAny(language, "-_"); index > 0 {
		return strings.ToLower(language[:index])
	}
	return strings.ToLower(language)
}

func sameLanguage(left, right string) bool {
	return left != "" && providerLanguage(left) == providerLanguage(right)
}

func unavailableError(where string) *model.AppError {
	return model.NewAppError(where, model.NoTranslation, nil, "auto-translation is unavailable or disabled", http.StatusNotImplemented)
}

func canceledTranslationError() *model.AppError {
	return model.NewAppError("AutoTranslation.Translate", model.NoTranslation, nil,
		"translation request was canceled", http.StatusRequestTimeout)
}

func providerError(where string, status int, err error) *model.AppError {
	return model.NewAppError(where, model.NoTranslation, nil, err.Error(), status).Wrap(err)
}

func (s *Service) Start() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed.Load() || s.workContext == nil {
		s.workContext, s.cancelWork = context.WithCancel(context.Background())
	}
	s.closed.Store(false)
	return nil
}

func (s *Service) Shutdown() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.closed.Store(true)
	if s.cancelWork != nil {
		s.cancelWork()
	}
	return nil
}

func (s *Service) Close() error {
	return s.Shutdown()
}

func (s *Service) MakeWorker() model.Worker {
	ctx, cancel := s.newWorkContext()
	worker := &recoveryWorker{service: s, ctx: ctx, cancel: cancel}
	execute := func(logger mlog.LoggerIFace, _ *model.Job) error {
		worker.mu.Lock()
		ctx := worker.ctx
		worker.mu.Unlock()
		return s.recoverTranslations(ctx, logger)
	}
	worker.SimpleWorker = jobs.NewSimpleWorker("WorkspaceAutoTranslationRecovery", s.server.Jobs, execute, func(cfg *model.Config) bool {
		return s.licensed() && configEnabled(cfg)
	})
	return worker
}

func (s *Service) newWorkContext() (context.Context, context.CancelFunc) {
	s.lifecycleMu.Lock()
	parent := s.workContext
	s.lifecycleMu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	return context.WithCancel(parent)
}

func (s *Service) MakeScheduler() ejobs.Scheduler {
	return jobs.NewPeriodicScheduler(s.server.Jobs, model.JobTypeAutoTranslationRecovery, recoveryJobPeriod, func(cfg *model.Config) bool {
		return s.licensed() && configEnabled(cfg)
	})
}
