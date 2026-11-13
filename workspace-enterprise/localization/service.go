package localization

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	goi18n "github.com/mattermost/go-i18n/i18n"
	goi18nbundle "github.com/mattermost/go-i18n/i18n/bundle"
	"github.com/mattermost/mattermost/server/public/model"
	sharedi18n "github.com/mattermost/mattermost/server/public/shared/i18n"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

const (
	systemBotName           = "نظام"
	contentReviewBotName    = "مراجعة المحتوى"
	townSquareName          = "المجموعة الرئيسية"
	offTopicName            = "المجموعة الفرعية"
	maxTranslatedErrorBytes = 1 << 20
)

//go:embed ar.json
var arabicCatalog []byte

var (
	arabicBundleOnce sync.Once
	arabicTranslator sharedi18n.TranslateFunc
)

// RegisterArabicLocale makes the embedded catalog available before the server
// validates its localization settings during startup.
func RegisterArabicLocale() error {
	if err := goi18n.ParseTranslationFileBytes("ar.json", arabicCatalog); err != nil {
		return err
	}
	sharedi18n.GetSupportedLocales()["ar"] = "embedded:ar.json"
	return nil
}

type translation struct {
	ID          string          `json:"id"`
	Translation json.RawMessage `json:"translation"`
}

type Service struct {
	app          *app.App
	translations map[string]string
	initMu       sync.Mutex
	initialized  bool
}

func Register(a *app.App) {
	s := &Service{app: a, translations: loadTranslations()}
	a.Srv().Router.Use(s.middleware)
}

func loadTranslations() map[string]string {
	var entries []translation
	if err := json.Unmarshal(arabicCatalog, &entries); err != nil {
		return map[string]string{}
	}

	translations := make(map[string]string, len(entries))
	for _, entry := range entries {
		var message string
		if json.Unmarshal(entry.Translation, &message) == nil {
			translations[entry.ID] = message
		}
	}
	return translations
}

// ArabicTranslateFunc returns the Arabic server translations from the embedded
// catalog, with fallback for strings that are not present in that catalog.
func ArabicTranslateFunc(fallback sharedi18n.TranslateFunc) sharedi18n.TranslateFunc {
	arabicBundleOnce.Do(func() {
		bundle := goi18nbundle.New()
		if err := bundle.ParseTranslationFileBytes("ar.json", arabicCatalog); err != nil {
			return
		}

		translate, err := bundle.Tfunc("ar")
		if err != nil {
			return
		}
		arabicTranslator = func(translationID string, args ...any) string {
			return translate(translationID, args...)
		}
	})

	return func(translationID string, args ...any) string {
		args = localizeCommandTemplateArgs(translationID, args)
		if arabicTranslator != nil {
			if translated := arabicTranslator(translationID, args...); translated != translationID {
				return translated
			}
		}
		if fallback != nil {
			return fallback(translationID, args...)
		}
		return translationID
	}
}

func localizeCommandTemplateArgs(translationID string, args []any) []any {
	if len(args) == 0 {
		return args
	}

	values, ok := args[0].(map[string]any)
	if !ok {
		return args
	}

	localizedArgs := append([]any(nil), args...)
	localizedValues := make(map[string]any, len(values))
	for key, value := range values {
		localizedValues[key] = value
	}

	switch translationID {
	case "api.command_remote.remote_add_remove.help", "api.command_remote.missing_command":
		localizedValues["Actions"] = "إنشاء، قبول، إزالة، حالة"
	case "api.command_share.available_actions", "api.command_share.missing_action", "api.command_share.unknown_action":
		localizedValues["Actions"] = "دعوة، إلغاء-الدعوة، إلغاء-المشاركة، حالة"
	case "api.command_remote.permission_required":
		localizedValues["Permission"] = "إدارة الاتصالات الآمنة"
	case "api.command_share.permission_required":
		localizedValues["Permission"] = "إدارة المجموعات المشتركة"
	case "api.command_remote.missing_empty":
		if argument, ok := localizedValues["Arg"].(string); ok {
			localizedValues["Arg"] = map[string]string{
				"password": "كلمة-المرور",
				"name":     "اسم",
				"invite":   "الدعوة",
				"remoteId": "معرف-الاتصال",
			}[argument]
		}
	case "api.command_remote.invite_summary":
		localizedValues["Command"] = "/اتصال_آمن قبول"
	case "api.command_share.invalid_value.error":
		localizedValues["Arg"] = "للقراءة-فقط"
		localizedValues["Error"] = "استخدم إحدى القيم: نعم أو لا"
	case "api.command_exportlink.hint":
		localizedValues["LatestMsg"] = "الأحدث"
	}

	localizedArgs[0] = localizedValues
	return localizedArgs
}

func (s *Service) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		licensed := s.app.License() != nil
		if licensed {
			s.initialize(r)
		}

		writer := w
		var arabicWriter *translatedErrorWriter
		if licensed && wantsArabicForRequest(r.Header.Get("Accept-Language"), *s.app.Config().LocalizationSettings.DefaultClientLocale) {
			w.Header().Set("Content-Language", "ar")
			arabicWriter = &translatedErrorWriter{ResponseWriter: w}
			writer = arabicWriter
		}

		next.ServeHTTP(writer, r)
		if arabicWriter != nil {
			arabicWriter.finish(s.translations)
		}

		if !licensed {
			return
		}
		if r.Method == http.MethodPost && strings.TrimRight(r.URL.Path, "/") == model.APIURLSuffix+"/teams" {
			if err := s.localizeDefaultChannels(r); err != nil {
				s.app.Log().Warn("Failed to localize default channel names", mlog.Err(err))
			}
		}
		if strings.HasPrefix(r.URL.Path, model.APIURLSuffix+"/content_flagging") {
			if err := s.ensureBot(r, model.ContentFlaggingBotUsername, contentReviewBotName); err != nil {
				s.app.Log().Warn("Failed to localize the content review bot", mlog.Err(err))
			}
		}
	})
}

// Requests without a language preference use the configured client default.
func wantsArabicForRequest(header, defaultLocale string) bool {
	if strings.TrimSpace(header) == "" {
		return defaultLocale == "ar"
	}
	bestQuality := -1.0
	bestArabic := false
	supported := false
	for _, value := range strings.Split(header, ",") {
		parts := strings.Split(value, ";")
		locale := strings.ToLower(strings.TrimSpace(parts[0]))
		arabic := locale == "ar" || strings.HasPrefix(locale, "ar-")
		english := locale == "en" || strings.HasPrefix(locale, "en-")
		if !arabic && !english {
			continue
		}
		supported = true
		quality := 1.0
		valid := true
		for _, parameter := range parts[1:] {
			parameter = strings.TrimSpace(parameter)
			if !strings.HasPrefix(strings.ToLower(parameter), "q=") {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(parameter[2:]), 64)
			if err != nil || parsed < 0 || parsed > 1 {
				valid = false
				break
			}
			quality = parsed
		}
		if valid && quality > 0 && quality > bestQuality {
			bestQuality = quality
			bestArabic = arabic
		}
	}
	if bestQuality >= 0 {
		return bestArabic
	}
	return !supported && defaultLocale == "ar"
}

func (s *Service) initialize(r *http.Request) {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	if s.initialized {
		return
	}

	if err := s.localizeDefaultChannels(r); err != nil {
		s.app.Log().Warn("Failed to localize existing default channels", mlog.Err(err))
		return
	}
	if err := s.ensureBot(r, model.BotSystemBotUsername, systemBotName); err != nil {
		s.app.Log().Warn("Failed to initialize the localized system bot", mlog.Err(err))
		return
	}
	if err := s.ensureBot(r, model.ContentFlaggingBotUsername, contentReviewBotName); err != nil {
		s.app.Log().Warn("Failed to initialize the localized content review bot", mlog.Err(err))
		return
	}
	s.initialized = true
}

func (s *Service) localizeDefaultChannels(r *http.Request) error {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return fmt.Errorf("master database connection is unavailable")
	}

	result, err := db.ExecContext(r.Context(), `
		UPDATE channels
		SET displayname = CASE name
			WHEN 'town-square' THEN $1
			WHEN 'off-topic' THEN $2
		END
		WHERE name IN ('town-square', 'off-topic')
		  AND displayname IS DISTINCT FROM CASE name
			WHEN 'town-square' THEN $1
			WHEN 'off-topic' THEN $2
		  END
	`, townSquareName, offTopicName)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr == nil && changed > 0 {
		s.app.Srv().Store().Channel().ClearCaches()
	}
	return nil
}

func (s *Service) ensureBot(_ *http.Request, username, displayName string) error {
	rctx := request.EmptyContext(s.app.Log())
	bot, appErr := s.app.GetOrCreateSystemOwnedBot(rctx, username, displayName)
	if appErr != nil {
		return appErr
	}
	if bot.DisplayName == displayName {
		return nil
	}

	patched, appErr := s.app.PatchBot(rctx, bot.UserId, &model.BotPatch{DisplayName: &displayName})
	if appErr != nil {
		return appErr
	}
	if patched == nil || patched.DisplayName != displayName {
		return fmt.Errorf("bot %q retained display name %q", username, bot.DisplayName)
	}
	return nil
}

type translatedErrorWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	buffering   bool
	body        bytes.Buffer
}

func (w *translatedErrorWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.buffering = status >= http.StatusBadRequest && status <= 599
	if !w.buffering {
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *translatedErrorWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.buffering {
		if len(data) > maxTranslatedErrorBytes-w.body.Len() {
			if err := w.flushOriginal(); err != nil {
				return 0, err
			}
			return w.ResponseWriter.Write(data)
		}
		return w.body.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

func (w *translatedErrorWriter) flushOriginal() error {
	w.buffering = false
	w.ResponseWriter.WriteHeader(w.status)
	_, err := w.body.WriteTo(w.ResponseWriter)
	return err
}

func (w *translatedErrorWriter) finish(translations map[string]string) {
	if !w.buffering {
		return
	}

	body := w.body.Bytes()
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		if id, ok := payload["id"].(string); ok {
			if message := translations[id]; message != "" && !strings.Contains(message, "{{") {
				payload["message"] = message
				if encoded, err := json.Marshal(payload); err == nil {
					body = append(encoded, '\n')
				}
			}
		}
	}

	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(body)
}

func (w *translatedErrorWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.buffering {
		return
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *translatedErrorWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (w *translatedErrorWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (w *translatedErrorWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.buffering {
		return io.Copy(struct{ io.Writer }{w}, r)
	}
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

func (w *translatedErrorWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
