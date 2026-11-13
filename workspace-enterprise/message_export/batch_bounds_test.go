package message_export

import (
	"testing"

	"github.com/mattermost/enterprise/message_export/shared"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestMessageExportRejectsInvalidPostBatchSizes(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	post := th.CreatePost(t)
	for _, tc := range []struct {
		name string
		size int
	}{
		{"zero", 0}, {"negative", -1}, {"overflow_lookahead", int(^uint(0) >> 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := shared.JobData{JobDataExported: shared.JobDataExported{
				BatchSize: tc.size, JobEndTime: post.UpdateAt + 1,
			}}
			require.NotPanics(t, func() {
				returned, err := GetDataForBatch(th.Context, data, shared.BackendParams{Store: shared.NewMessageExportStore(th.Store)})
				require.ErrorContains(t, err, "batch size")
				require.Equal(t, data, returned, "rejected bounds must not advance the export cursor")
			})
		})
	}
}
