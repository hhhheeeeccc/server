package visual_editor

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpdatePersistsBeforeNotification(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "config.json")
	var notifications []bool
	s := &Service{filename: filename}
	s.notify = func(settings Settings) {
		disk, err := loadSettings(filename)
		require.NoError(t, err)
		require.Equal(t, settings, disk)
		notifications = append(notifications, settings.Enabled)
	}
	handler := middleware(func() bool { return s.current().Enabled }, "/api/v4/config/client", func() bool { return true })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"FeatureFlagWysiwygEditor":"false"}`))
	}))
	for _, enabled := range []bool{true, false, true} {
		require.NoError(t, s.update(Settings{Enabled: enabled}))
		require.Equal(t, enabled, s.current().Enabled)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v4/config/client", nil))
		if enabled {
			require.Contains(t, response.Body.String(), `"WorkspaceVisualEditorEnabled":"true"`)
		} else {
			require.Contains(t, response.Body.String(), `"WorkspaceVisualEditorEnabled":"false"`)
		}
	}
	require.Equal(t, []bool{true, false, true}, notifications)
	restarted, err := loadSettings(filename)
	require.NoError(t, err)
	require.True(t, restarted.Enabled)
}

func TestFailedWriteDoesNotChangeStateOrNotify(t *testing.T) {
	notified := false
	s := &Service{
		filename: filepath.Join(t.TempDir(), "missing", "config.json"),
		settings: Settings{Enabled: true},
		notify:   func(Settings) { notified = true },
	}
	require.Error(t, s.update(Settings{Enabled: false}))
	require.True(t, s.current().Enabled)
	require.False(t, notified)
}

func TestConcurrentSettingsUpdates(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "config.json")
	s := &Service{filename: filename}
	var group sync.WaitGroup
	errors := make(chan error, 16)
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func(enabled bool) {
			defer group.Done()
			errors <- s.update(Settings{Enabled: enabled})
			_ = s.current()
		}(i%2 == 0)
	}
	group.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	disk, err := loadSettings(filename)
	require.NoError(t, err)
	require.Equal(t, s.current(), disk)
}
