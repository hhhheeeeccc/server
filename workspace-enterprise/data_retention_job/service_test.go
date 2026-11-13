package data_retention_job

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

func TestRetentionEnabled(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		require.False(t, retentionEnabled(nil))
	})

	t.Run("message deletion", func(t *testing.T) {
		cfg := &model.Config{}
		cfg.DataRetentionSettings.SetDefaults()
		cfg.DataRetentionSettings.EnableMessageDeletion = model.NewPointer(true)
		require.True(t, retentionEnabled(cfg))
	})

	t.Run("file deletion", func(t *testing.T) {
		cfg := &model.Config{}
		cfg.DataRetentionSettings.SetDefaults()
		cfg.DataRetentionSettings.EnableFileDeletion = model.NewPointer(true)
		require.True(t, retentionEnabled(cfg))
	})
}

func TestDeletionStartTime(t *testing.T) {
	cfg := &model.Config{}
	cfg.DataRetentionSettings.SetDefaults()
	cfg.DataRetentionSettings.DeletionJobStartTime = model.NewPointer("04:35")

	start := deletionStartTime(cfg)
	require.NotNil(t, start)
	require.Equal(t, 4, start.Hour())
	require.Equal(t, 35, start.Minute())

	cfg.DataRetentionSettings.DeletionJobStartTime = model.NewPointer("invalid")
	require.Nil(t, deletionStartTime(cfg))
}

func TestMakeBatchConfig(t *testing.T) {
	cfg := &model.Config{}
	cfg.DataRetentionSettings.SetDefaults()
	cfg.DataRetentionSettings.BatchSize = model.NewPointer(125)
	cfg.DataRetentionSettings.PreservePinnedPosts = model.NewPointer(true)
	policy := &model.GlobalRetentionPolicy{
		MessageDeletionEnabled: true,
		MessageRetentionCutoff: 777,
	}

	batchConfig := makeBatchConfig(cfg, policy, 999)
	require.Equal(t, int64(125), batchConfig.Limit)
	require.Equal(t, int64(999), batchConfig.Now)
	require.Equal(t, int64(777), batchConfig.GlobalPolicyEndTime)
	require.True(t, batchConfig.PreservePinnedPosts)
}

func TestWorkerWaitDistinguishesCancellationFromShutdown(t *testing.T) {
	t.Run("job cancellation", func(t *testing.T) {
		cancelRequested := make(chan struct{})
		close(cancelRequested)
		worker := &Worker{stop: make(chan bool, 1)}
		require.Equal(t, jobCanceled, worker.wait(cancelRequested, time.Second))
	})

	t.Run("worker shutdown", func(t *testing.T) {
		worker := &Worker{stop: make(chan bool, 1)}
		worker.stop <- true
		require.Equal(t, workerStopped, worker.wait(make(chan struct{}), time.Second))
	})
}

func TestWorkerCanRestartAfterStop(t *testing.T) {
	logger, err := mlog.NewLogger()
	require.NoError(t, err)
	w := &Worker{
		logger:  logger,
		jobs:    make(chan model.Job),
		stop:    make(chan bool, 1),
		stopped: make(chan bool, 1),
	}

	for range 3 {
		go w.Run()
		require.Eventually(t, func() bool {
			w.lifecycleMu.Lock()
			defer w.lifecycleMu.Unlock()
			return w.running
		}, 2*time.Second, time.Millisecond)
		w.Stop()
		w.Stop()
	}
}
