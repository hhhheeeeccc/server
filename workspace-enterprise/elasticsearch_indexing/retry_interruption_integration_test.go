package elasticsearch_indexing

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	"github.com/stretchr/testify/require"
)

// Fail one real worker read, then leave the reader healthy. A retry must not
// run after Stop: returning promptly must also preserve a replayable cursor.
func TestIndexingStopInterruptsFailedBatchRetry(t *testing.T) {
	for _, phase := range []string{"posts", "channels", "users", "files"} {
		t.Run(phase, func(t *testing.T) {
			th, endpoint, prefix := setupIndexer(t)
			file, err := th.Store.FileInfo().Save(th.Context, &model.FileInfo{
				CreatorId: th.BasicUser.Id, PostId: th.BasicPost.Id, ChannelId: th.BasicChannel.Id,
				Name: "retry.txt", Path: "fixture/retry.txt", Extension: "txt", MimeType: "text/plain", Content: "إعادة محاولة",
			})
			require.NoError(t, err)
			w := New(th.Server).MakeWorker().(*checkpointWorker)
			fault := &failedIndexingRead{Store: w.server.Store, phase: phase, entered: make(chan struct{})}
			w.server.Store = fault
			th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, w, nil)
			go w.Run()
			t.Cleanup(w.Stop)
			data := model.StringMap{"start_time": "0", "index_posts": "false", "index_channels": "false", "index_users": "false", "index_files": "false"}
			data["index_"+phase] = "true"
			job := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, data)
			sendCheckpointJob(t, w, job)
			select {
			case <-fault.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("worker did not reach the failed batch read")
			}
			stopped := make(chan struct{})
			started := time.Now()
			go func() { w.Stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("Stop waited for the fixed 15-second batch retry delay")
			}
			require.Less(t, time.Since(started), 2*time.Second)
			require.EqualValues(t, 1, fault.reads.Load(), "a stopped worker must not retry the read")
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, model.JobStatusPending, stored.Status)
			checkpoint, err := loadIndexingCheckpoint(stored)
			require.NoError(t, err)
			require.Zero(t, checkpoint.Progress.LastEntityTime)
			require.Zero(t, checkpoint.Progress.DonePostsCount+checkpoint.Progress.DoneChannelsCount+checkpoint.Progress.DoneUsersCount+checkpoint.Progress.DoneFilesCount)
			require.False(t, checkpoint.Progress.IsDone(stored))
			// Resume the same instance and job after the single failed read. This
			// catches leaked retry state, orphaned work and incorrectly advanced cursors.
			go w.Run()
			sendCheckpointJob(t, w, stored)
			final := waitCheckpointJob(t, th, stored.Id, model.JobStatusSuccess)
			require.NotEqual(t, stored.Data[indexingAttemptKey], final.Data[indexingAttemptKey])
			index, id := phase, th.BasicPost.Id
			switch phase {
			case "posts":
				index += "*"
			case "channels":
				id = th.BasicChannel.Id
			case "users":
				id = th.BasicUser.Id
			case "files":
				id = file.Id
			}
			assertPostIndexed(t, endpoint, prefix+index, id)
		})
	}
}

func TestIndexingRetryHonorsCancellationAndLiveGates(t *testing.T) {
	for _, action := range []string{"cancel", "license", "configuration", "destination"} {
		t.Run(action, func(t *testing.T) {
			th, endpoint, prefix := setupIndexer(t)
			w := New(th.Server).MakeWorker().(*checkpointWorker)
			fault := &failedIndexingRead{Store: w.server.Store, phase: "posts", entered: make(chan struct{})}
			w.server.Store = fault
			th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, w, nil)
			go w.Run()
			t.Cleanup(w.Stop)
			job := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, model.StringMap{
				"start_time": "0", "index_posts": "true", "index_channels": "false", "index_users": "false", "index_files": "false",
			})
			sendCheckpointJob(t, w, job)
			select {
			case <-fault.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("worker did not reach the failed batch read")
			}
			want, deadline := model.JobStatusPending, 2*time.Second
			switch action {
			case "cancel":
				_, err := th.Store.Job().UpdateStatus(job.Id, model.JobStatusCancelRequested)
				require.NoError(t, err)
				// The existing cancellation watcher polls every five seconds.
				want, deadline = model.JobStatusCanceled, 7*time.Second
			case "license":
				th.Server.SetLicense(nil)
			case "configuration":
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.EnableIndexing = new(false) })
			case "destination":
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.IndexPrefix = new(prefix + "changed_") })
			}
			var stored *model.Job
			require.Eventually(t, func() bool {
				var err error
				stored, err = th.Store.Job().Get(th.Context, job.Id)
				require.NoError(t, err)
				return stored.Status == want
			}, deadline, 20*time.Millisecond)
			require.EqualValues(t, 1, fault.reads.Load(), "interruption must prevent another read")
			checkpoint, err := loadIndexingCheckpoint(stored)
			require.NoError(t, err)
			require.Zero(t, checkpoint.Progress.LastEntityTime)
			require.Zero(t, checkpoint.Progress.DonePostsCount)
			require.False(t, checkpoint.Progress.DonePosts)
			// Restore availability, then use this same worker for a real job.
			// Cancellation remains terminal; pending work resumes its saved Job.
			require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
			th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.ElasticsearchSettings.EnableIndexing = new(true)
				cfg.ElasticsearchSettings.IndexPrefix = new(prefix)
			})
			if action == "cancel" {
				runIndexingJob(t, th, w, model.JobStatusSuccess)
				current, err := th.Store.Job().Get(th.Context, stored.Id)
				require.NoError(t, err)
				require.Equal(t, model.JobStatusCanceled, current.Status)
				require.Equal(t, stored.Data[indexingAttemptKey], current.Data[indexingAttemptKey])
			} else {
				sendCheckpointJob(t, w, stored)
				waitCheckpointJob(t, th, stored.Id, model.JobStatusSuccess)
			}
			assertPostIndexed(t, endpoint, prefix, th.BasicPost.Id)
		})
	}
}

type failedIndexingRead struct {
	store.Store
	phase   string
	reads   atomic.Int64
	entered chan struct{}
}

func (s *failedIndexingRead) fail(phase string) error {
	if phase == s.phase && s.reads.Add(1) == 1 {
		close(s.entered)
		return errors.New("isolated transient indexing read failure")
	}
	return nil
}

func (s *failedIndexingRead) Post() store.PostStore {
	return &failedPostRead{PostStore: s.Store.Post(), fault: s}
}
func (s *failedIndexingRead) Channel() store.ChannelStore {
	return &failedChannelRead{ChannelStore: s.Store.Channel(), fault: s}
}
func (s *failedIndexingRead) User() store.UserStore {
	return &failedUserRead{UserStore: s.Store.User(), fault: s}
}
func (s *failedIndexingRead) FileInfo() store.FileInfoStore {
	return &failedFileRead{FileInfoStore: s.Store.FileInfo(), fault: s}
}

type failedPostRead struct {
	store.PostStore
	fault *failedIndexingRead
}

func (s *failedPostRead) GetPostsBatchForIndexing(at int64, id string, limit int) ([]*model.PostForIndexing, error) {
	if err := s.fault.fail("posts"); err != nil {
		return nil, err
	}
	return s.PostStore.GetPostsBatchForIndexing(at, id, limit)
}

type failedChannelRead struct {
	store.ChannelStore
	fault *failedIndexingRead
}

func (s *failedChannelRead) GetChannelsBatchForIndexing(at int64, id string, limit int) ([]*model.Channel, error) {
	if err := s.fault.fail("channels"); err != nil {
		return nil, err
	}
	return s.ChannelStore.GetChannelsBatchForIndexing(at, id, limit)
}

type failedUserRead struct {
	store.UserStore
	fault *failedIndexingRead
}

func (s *failedUserRead) GetUsersBatchForIndexing(at int64, id string, limit int) ([]*model.UserForIndexing, error) {
	if err := s.fault.fail("users"); err != nil {
		return nil, err
	}
	return s.UserStore.GetUsersBatchForIndexing(at, id, limit)
}

type failedFileRead struct {
	store.FileInfoStore
	fault *failedIndexingRead
}

func (s *failedFileRead) GetFilesBatchForIndexing(at int64, id string, deleted bool, limit int) ([]*model.FileForIndexing, error) {
	if err := s.fault.fail("files"); err != nil {
		return nil, err
	}
	return s.FileInfoStore.GetFilesBatchForIndexing(at, id, deleted, limit)
}
