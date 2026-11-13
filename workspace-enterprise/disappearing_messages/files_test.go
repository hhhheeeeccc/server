package disappearing_messages

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestFileRoutesRespectSiteSubpath(t *testing.T) {
	for _, tc := range []struct {
		name, siteURL, requestPath string
		want                       []string
	}{
		{"root download", "https://example.test", "/api/v4/files/file-id", []string{"api", "v4", "files", "file-id"}},
		{"subpath download", "https://example.test/workspace", "/workspace/api/v4/files/file-id", []string{"api", "v4", "files", "file-id"}},
		{"subpath public link", "https://example.test/workspace/", "/workspace/api/v4/files/file-id/public", []string{"api", "v4", "files", "file-id", "public"}},
		{"nested thumbnail", "https://example.test/company/workspace", "/company/workspace/api/v4/files/file-id/thumbnail", []string{"api", "v4", "files", "file-id", "thumbnail"}},
		{"other prefix", "https://example.test/workspace", "/other/api/v4/files/file-id", nil},
		{"similar prefix", "https://example.test/work", "/workspace/api/v4/files/file-id", nil},
		{"other API", "https://example.test/workspace", "/workspace/api/v4/posts/file-id", nil},
		{"not a file", "https://example.test", "/api/v4/files", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &model.Config{}
			cfg.ServiceSettings.SiteURL = model.NewPointer(tc.siteURL)
			require.Equal(t, tc.want, fileRouteParts(tc.requestPath, cfg))
		})
	}
}
