package localization

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sharedi18n "github.com/mattermost/mattermost/server/public/shared/i18n"
	"github.com/stretchr/testify/require"
)

func TestRegisterArabicLocale(t *testing.T) {
	require.NoError(t, RegisterArabicLocale())
	require.Equal(t, "embedded:ar.json", sharedi18n.GetSupportedLocales()["ar"])
	require.Equal(t, "بالخارج", sharedi18n.GetUserTranslations("ar")("api.command_away.name"))
}

func TestTranslatedErrorWriterStreamsOversizedError(t *testing.T) {
	for _, useReadFrom := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		writer := &translatedErrorWriter{ResponseWriter: recorder}
		writer.WriteHeader(http.StatusBadRequest)
		original := strings.Repeat("x", maxTranslatedErrorBytes+1)
		var err error
		if useReadFrom {
			_, err = writer.ReadFrom(strings.NewReader(original))
		} else {
			_, err = writer.Write([]byte(original))
		}
		require.NoError(t, err)
		require.False(t, writer.buffering)
		require.Zero(t, writer.body.Len())
		writer.finish(map[string]string{"error": "تعذر الطلب"})
		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Equal(t, original, recorder.Body.String())
	}
}

func TestTranslatedErrorWriterStillTranslatesSmallError(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &translatedErrorWriter{ResponseWriter: recorder}
	writer.WriteHeader(http.StatusBadRequest)
	_, err := writer.Write([]byte(`{"id":"example.error","message":"Original"}`))
	require.NoError(t, err)
	writer.finish(map[string]string{"example.error": "خطأ"})
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.JSONEq(t, `{"id":"example.error","message":"خطأ"}`, recorder.Body.String())
}

func TestArabicTranslateFunc(t *testing.T) {
	translate := ArabicTranslateFunc(func(id string, _ ...any) string {
		return "fallback:" + id
	})

	require.Equal(t, "بالخارج", translate("api.command_away.name"))
	require.Equal(t, "تعذّر جلب سياسة التحكم بالوصول.", translate("enterprise.access_control.get_policy.app_error"))
	require.Equal(t, "صيغة قاعدة الوصول غير صحيحة. راجع التعبير ثم حاول الحفظ مجددًا.", translate("enterprise.access_control.invalid_expression.app_error"))
	require.Equal(t, "لا يمكن استخدام سمات القنوات في قواعد الوصول لأن هذه الإمكانية غير مفعّلة على الخادم.", translate("app.pap.save_policy.resource_attributes_disabled"))
	require.Equal(t, "إضافة/إزالة الاتصالات الآمنة. الإجراءات المتاحة: إنشاء، قبول، إزالة، حالة", translate(
		"api.command_remote.remote_add_remove.help",
		map[string]any{"Actions": "إنشاء، قبول، إزالة، حالة"},
	))
	require.Equal(t, "fallback:missing.translation", translate("missing.translation"))
	require.Equal(t, "الأمر مفقود. الإجراءات المتاحة: إنشاء، قبول، إزالة، حالة", translate(
		"api.command_remote.missing_command",
		map[string]any{"Actions": "create, accept, remove, status"},
	))
	require.Equal(t, "مفقود أو فارغ `كلمة-المرور`", translate(
		"api.command_remote.missing_empty",
		map[string]any{"Arg": "password"},
	))
	require.Equal(t, "[رقم-المهمة|اسم ملف مضغوط|الأحدث]", translate(
		"api.command_exportlink.hint",
		map[string]any{"LatestMsg": "latest"},
	))
}

func TestPolicyErrorTranslationPreservesErrorDetails(t *testing.T) {
	// The same error ID is used for missing policies and store failures.
	// Translation must retain the distinction and the request metadata.
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			original := map[string]any{
				"id":             "enterprise.access_control.get_policy.app_error",
				"message":        "enterprise.access_control.get_policy.app_error",
				"status_code":    status,
				"request_id":     "rfig1k9j43njtdk3t54e8sr59w",
				"detailed_error": "",
			}
			body, err := json.Marshal(original)
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			writer := &translatedErrorWriter{ResponseWriter: recorder}
			writer.WriteHeader(status)
			_, err = writer.Write(body)
			require.NoError(t, err)
			writer.finish(loadTranslations())

			original["message"] = "تعذّر جلب سياسة التحكم بالوصول."
			expected, err := json.Marshal(original)
			require.NoError(t, err)
			require.Equal(t, status, recorder.Code)
			require.JSONEq(t, string(expected), recorder.Body.String())
		})
	}
}

func TestWantsArabicForRequest(t *testing.T) {
	require.True(t, wantsArabicForRequest("", "ar"))
	require.True(t, wantsArabicForRequest("ar-YE,ar;q=0.9", "en"))
	require.False(t, wantsArabicForRequest("en", "ar"))
	require.False(t, wantsArabicForRequest("", "en"))
	require.False(t, wantsArabicForRequest("en-US,en;q=0.9,ar;q=0.8", "ar"))
	require.True(t, wantsArabicForRequest("en;q=0.4,ar-YE;q=0.9", "en"))
	require.False(t, wantsArabicForRequest("ar;q=0,en;q=1", "ar"))
	require.True(t, wantsArabicForRequest("fr-FR", "ar"))
}
