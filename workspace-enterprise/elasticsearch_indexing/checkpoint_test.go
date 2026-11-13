package elasticsearch_indexing

import (
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/enterprise/elasticsearch/common"
	"github.com/stretchr/testify/require"
)

func TestBulkAcknowledgementsRemainScopedToTheirBatch(t *testing.T) {
	r := &bulkResults{}
	first := r.beginBatch()
	completeFirst := r.addItem()
	second := r.beginBatch()
	completeSecond := r.addItem()
	completeSecond(false)
	require.True(t, second.confirmed())
	require.False(t, first.confirmed(), "a later acknowledgement cannot cover an earlier batch")
	completeFirst(true)
	require.False(t, first.confirmed(), "a failed earlier batch cannot become a safe checkpoint")
	require.EqualValues(t, 1, r.failed.Load())
	require.EqualValues(t, 2, r.completed.Load())
}

func TestIndexingCheckpointRetainsScopePhaseAndPartitionTime(t *testing.T) {
	job := &model.Job{Data: model.StringMap{"index_posts": "true", "index_channels": "true", "index_users": "true", "index_files": "true"}}
	progress := common.IndexingProgress{
		Now:         time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		StartAtTime: 100, EndAtTime: 900, LastEntityTime: 420,
		DonePosts: true, DoneChannels: true, DonePostsCount: 10, DoneChannelsCount: 4,
		LastPostID: "lastpost", LastChannelID: "lastchannel", LastUserID: "lastuser", DoneUsersCount: 3,
	}
	require.NoError(t, putIndexingCheckpoint(job, progress))
	// The old single cursor is insufficient to reconstruct the original
	// scope or completed phases. The versioned snapshot is authoritative.
	job.Data["start_time"] = "420"
	job.Data["index_posts"] = "false"
	loaded, err := loadIndexingCheckpoint(job)
	require.NoError(t, err)
	require.Equal(t, progress, loaded.Progress)
	require.Equal(t, "true", loaded.Entities["index_posts"])
}

func TestLegacyIndexingCheckpointReplaysOriginalScope(t *testing.T) {
	job := &model.Job{Data: model.StringMap{
		"start_time": "400", "original_start_time": "100", "end_time": "900",
		"done_posts_count": "99", "start_post_id": "unconfirmed", "start_user_id": "alsounconfirmed",
	}}
	loaded, err := loadIndexingCheckpoint(job)
	require.NoError(t, err)
	require.EqualValues(t, 100, loaded.Progress.StartAtTime)
	require.EqualValues(t, 100, loaded.Progress.LastEntityTime)
	require.EqualValues(t, 900, loaded.Progress.EndAtTime)
	require.Zero(t, loaded.Progress.DonePostsCount)
	require.Empty(t, loaded.Progress.LastPostID)
	require.Empty(t, loaded.Progress.LastUserID)
}

func TestIndexingCheckpointRejectsCorruption(t *testing.T) {
	for _, raw := range []string{`{`, `{"version":2}`, `{"version":1,"progress":{"StartAtTime":-1}}`} {
		t.Run(raw, func(t *testing.T) {
			_, err := loadIndexingCheckpoint(&model.Job{Data: model.StringMap{checkpointKey: raw}})
			require.Error(t, err)
		})
	}
}
