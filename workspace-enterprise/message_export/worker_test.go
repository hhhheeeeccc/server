package message_export

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestMessageExportWorkerIsEnabled(t *testing.T) {
	cfg := &model.Config{}
	cfg.SetDefaults()
	*cfg.MessageExportSettings.EnableExport = true

	worker := &MessageExportWorker{license: func() *model.License { return nil }}
	require.False(t, worker.IsEnabled(cfg))

	license := model.NewTestLicense("message_export")
	worker.license = func() *model.License { return license }
	require.True(t, worker.IsEnabled(cfg))

	license.Features.MessageExport = nil
	require.False(t, worker.IsEnabled(cfg))
	require.False(t, worker.IsEnabled(nil))
}
