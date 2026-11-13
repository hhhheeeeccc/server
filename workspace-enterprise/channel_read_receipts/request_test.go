package channel_read_receipts

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestReadChannelIDsDeduplicatesMarkRead(t *testing.T) {
	first, second := model.NewId(), model.NewId()
	ids := append(make([]string, 0, 1002), first, second)
	for range 1000 {
		ids = append(ids, first)
	}
	body, err := json.Marshal(ids)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, channelMemberViewPathPrefix+"me/mark_read", strings.NewReader(string(body)))
	channels, ok := readChannelIDs(r, "mark_read", int64(len(body)))
	require.True(t, ok)
	require.Equal(t, []string{first, second}, channels, "one membership lookup per distinct channel")
	restored, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	require.Equal(t, body, restored, "the core handler must still receive the original body")
}

func TestSaveReactionRequestWithSubpath(t *testing.T) {
	reaction := reactionRequest{UserID: model.NewId(), PostID: model.NewId()}
	body, err := json.Marshal(reaction)
	require.NoError(t, err)
	for _, subpath := range []string{"", "/workspace", "/company/chat"} {
		t.Run(subpath, func(t *testing.T) {
			config := &model.Config{}
			config.SetDefaults()
			config.ServiceSettings.SiteURL = new("https://example.com" + subpath)
			r := httptest.NewRequest(http.MethodPost, subpath+saveReactionPath+"/", strings.NewReader(string(body)))
			got, ok := saveReactionRequest(r, config)
			require.True(t, ok)
			require.Equal(t, reaction, got)
			restored, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Equal(t, body, restored)
		})
	}
}

func TestSaveReactionRequestRejectsOtherRoutes(t *testing.T) {
	reaction := reactionRequest{UserID: model.NewId(), PostID: model.NewId()}
	body, err := json.Marshal(reaction)
	require.NoError(t, err)
	config := &model.Config{}
	config.SetDefaults()
	config.ServiceSettings.SiteURL = new("https://example.com/workspace")
	for _, path := range []string{"/api/v4/reactions", "/workspace/api/v4/reactions/extra", "/workspace/notapi/v4/reactions", "/workspace/api/v4/posts", "/workspace/api/v4/reactions//", "/other/api/v4/reactions", "/plugins/test/api/v4/reactions"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
			_, ok := saveReactionRequest(r, config)
			require.False(t, ok)
		})
	}
	r := httptest.NewRequest(http.MethodGet, "/workspace/api/v4/reactions", strings.NewReader(string(body)))
	_, ok := saveReactionRequest(r, config)
	require.False(t, ok)
}

func TestThreadReadRequest(t *testing.T) {
	userID, teamID, threadID := model.NewId(), model.NewId(), model.NewId()
	cfg := &model.Config{}
	cfg.SetDefaults()
	cfg.ServiceSettings.SiteURL = new("https://example.com/workspace")
	path := "/workspace/api/v4/users/" + userID + "/teams/" + teamID + "/threads/" + threadID + "/read/123"
	for _, tc := range []struct {
		name, method, path string
		valid              bool
	}{
		{"valid", http.MethodPut, path, true},
		{"me", http.MethodPut, strings.Replace(path, userID, "me", 1), true},
		{"channel view is not thread read", http.MethodPost, channelMemberViewPathPrefix + "me/view", false},
		{"wrong method", http.MethodGet, path, false},
		{"invalid timestamp", http.MethodPut, strings.TrimSuffix(path, "123") + "invalid", false},
		{"other subpath", http.MethodPut, strings.TrimPrefix(path, "/workspace"), false},
		{"extra segment", http.MethodPut, path + "/extra", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, gotThread, ok := threadReadRequest(httptest.NewRequest(tc.method, tc.path, nil), cfg)
			require.Equal(t, tc.valid, ok)
			if ok {
				require.Equal(t, threadID, gotThread)
			}
		})
	}
}
