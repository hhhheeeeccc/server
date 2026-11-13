package profile_gif

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCorrectImageType(t *testing.T) {
	service := &Service{}
	for _, test := range []struct {
		name        string
		image       string
		contentType string
	}{
		{name: "GIF", image: "GIF89aimage", contentType: "image/gif"},
		{name: "PNG", image: "\x89PNGimage", contentType: "image/png"},
	} {
		t.Run(test.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write([]byte(test.image))
			})
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/v4/users/abc/image", nil)
			service.correctImageType(next).ServeHTTP(response, request)
			require.Equal(t, test.contentType, response.Header().Get("Content-Type"))
		})
	}
}
