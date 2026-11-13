package required_password_change

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestRequiredPasswordChangePreservesOtherUserProperties(t *testing.T) {
	original := model.StringMap{"custom_status": "busy", "locale_hint": "ar"}
	required := withRequiredPasswordChange(original, true)
	require.Equal(t, "true", required[propKey])
	require.Equal(t, "busy", required["custom_status"])
	require.Equal(t, "ar", required["locale_hint"])
	require.NotContains(t, original, propKey)

	completed := withRequiredPasswordChange(required, false)
	require.NotContains(t, completed, propKey)
	require.Equal(t, "busy", completed["custom_status"])
	require.Equal(t, "true", required[propKey])
}
