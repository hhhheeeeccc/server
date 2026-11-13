package saml

import (
	"strings"

	"github.com/mattermost/enterprise/localization"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

var errorMessages = map[string]string{
	"ent.saml.configuration.app_error":         "SAML configuration or certificates are invalid.",
	"ent.saml.invalid_response.app_error":      "The SAML response is invalid, expired, or has already been used. Start a new sign-in.",
	"ent.saml.store.app_error":                 "Unable to store SAML sign-in state.",
	"ent.saml.invalid_attributes.app_error":    "The SAML provider returned missing, conflicting, or invalid user attributes.",
	"ent.saml.profile_conflict.app_error":      "Unable to update the SAML profile. The email or username may belong to another account.",
	"ent.saml.ldap_sync_unavailable.app_error": "LDAP synchronization is unavailable. Configure and enable the LDAP service before signing in.",
}

// Redirect errors are translated before the web handler signs the error URL;
// the JSON-only localization middleware cannot translate that response later.
func localizeError(rctx request.CTX, appErr *model.AppError) {
	if appErr == nil {
		return
	}
	message, ok := errorMessages[appErr.Id]
	if !ok {
		return
	}
	locale := strings.ToLower(strings.TrimSpace(strings.SplitN(rctx.AcceptLanguage(), ",", 2)[0]))
	if locale == "ar" || strings.HasPrefix(locale, "ar-") || strings.HasPrefix(locale, "ar;") {
		message = localization.ArabicTranslateFunc(func(string, ...any) string { return message })(appErr.Id)
	}
	appErr.Message, appErr.SkipTranslation = message, true
}
