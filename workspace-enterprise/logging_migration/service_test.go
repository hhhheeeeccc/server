package logging_migration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/config"
	"github.com/stretchr/testify/require"
)

func logConfig(location string) *model.Config {
	c := &model.Config{}
	c.SetDefaults()
	c.LogSettings.EnableFile = new(true)
	c.LogSettings.FileLocation = &location
	return c
}

func TestPrepareLegacyLogs(t *testing.T) {
	root := t.TempDir()
	cfg := logConfig("/logs")
	before := cfg.Clone()
	require.Error(t, config.ValidateLogFilePath(config.GetLogFileLocation(*cfg.LogSettings.FileLocation), root), "reproduce log reader's 403 path rejection")
	migrated, changed, err := Prepare(cfg, root)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, root, *migrated.LogSettings.FileLocation)
	require.NoError(t, config.ValidateLogPaths(migrated, root))
	require.Equal(t, before, cfg, "preparation must not mutate the active configuration")
	expected := before.Clone()
	expected.LogSettings.FileLocation = &root
	require.Equal(t, expected, migrated, "only the legacy file location may change")
	again, changed, err := Prepare(migrated, root)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, migrated, again)
}

func TestPreparePreservesExplicitConfiguration(t *testing.T) {
	root := t.TempDir()
	for _, location := range []string{"", root, filepath.Join(root, "custom"), "/another-custom-path"} {
		cfg := logConfig(location)
		result, changed, err := Prepare(cfg, root)
		require.NoError(t, err)
		require.False(t, changed)
		require.Same(t, cfg, result)
	}
	cfg := logConfig("/logs")
	result, changed, err := Prepare(cfg, "/logs")
	require.NoError(t, err)
	require.False(t, changed)
	require.Same(t, cfg, result)
	cfg.LogSettings.EnableFile = new(false)
	_, changed, err = Prepare(cfg, root)
	require.NoError(t, err)
	require.False(t, changed)
	_, changed, err = Prepare(nil, root)
	require.NoError(t, err)
	require.False(t, changed)
}

func TestPrepareDoesNotBroadenRootOrAcceptSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "outside.log")
	require.NoError(t, os.WriteFile(target, []byte("sensitive"), 0600))
	require.NoError(t, os.Symlink(target, filepath.Join(root, config.LogFilename)))
	cfg := logConfig("/logs")
	_, changed, err := Prepare(cfg, root)
	require.Error(t, err)
	require.False(t, changed)
	require.Equal(t, "/logs", *cfg.LogSettings.FileLocation)
	content, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "sensitive", string(content))
}
