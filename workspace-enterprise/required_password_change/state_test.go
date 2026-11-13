package required_password_change

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRotationExemptionsCannotMutateProfileOrUseNormalAPI(t *testing.T) {
	id := "12345678901234567890123456"
	for _, tc := range []struct {
		method, path string
		exempt       bool
	}{
		{http.MethodGet, "/api/v4/users/me", true},
		{http.MethodGet, "/api/v4/users/" + id, true},
		{http.MethodPatch, "/api/v4/users/" + id + "/patch", false},
		{http.MethodPut, "/api/v4/users/" + id, false},
		{http.MethodPut, "/api/v4/users/" + id + "/password", false},
		{http.MethodPut, "/api/v4/workspace/users/me/required_password", true},
		{http.MethodGet, "/api/v4/users/me/channels", false},
		{http.MethodPost, "/api/v4/posts", false},
		{http.MethodGet, "/api/v4/websocket", false},
		{http.MethodGet, "/plugins/example/api/status", false},
		{http.MethodGet, "/static/main.js", true},
		{http.MethodPost, "/api/v4/users/logout", true},
		{http.MethodPut, "/api/v4/users/" + id + "/mfa", true},
		{http.MethodPut, "/api/v4/users/other/mfa", false},
	} {
		require.Equal(t, tc.exempt, rotationExempt(httptest.NewRequest(tc.method, tc.path, nil), id), tc.path)
	}
}
