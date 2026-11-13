package elasticsearch_indexing

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	"github.com/stretchr/testify/require"
)

func TestIndexingCheckpointWaitsForBulkAcknowledgements(t *testing.T) {
	th, endpoint, prefix := setupIndexer(t)
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	fault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_bulk") {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(fault.Close)
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ElasticsearchSettings.ConnectionURL = new(fault.URL)
		cfg.ElasticsearchSettings.RequestTimeoutSeconds = new(10)
	})
	worker := New(th.Server).MakeWorker()
	require.NotNil(t, worker)
	th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, worker, nil)
	go worker.Run()
	t.Cleanup(func() { unblock(); worker.Stop() })
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeElasticsearchPostIndexing, map[string]string{
		"start_time": "0", "index_posts": "true", "index_channels": "false", "index_users": "false", "index_files": "false",
	})
	require.Nil(t, appErr)
	select {
	case worker.JobChannel() <- *job:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not accept job")
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not send its bulk request")
	}
	stored, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	done, _ := strconv.ParseInt(stored.Data["done_posts_count"], 10, 64)
	require.Zero(t, done, "unacknowledged documents must not become a durable checkpoint")
	require.Empty(t, stored.Data["start_post_id"])
	unblock()
	require.Eventually(t, func() bool {
		stored, err = th.Store.Job().Get(th.Context, job.Id)
		require.NoError(t, err)
		return stored.Status == model.JobStatusSuccess || stored.Status == model.JobStatusError
	}, 30*time.Second, 25*time.Millisecond)
	require.Equal(t, model.JobStatusSuccess, stored.Status, "%v", stored.Data)
	assertPostIndexed(t, endpoint, prefix, th.BasicPost.Id)
}

func TestIndexingCheckpointResumesEveryEntityPhase(t *testing.T) {
	for _, phase := range []string{"posts", "channels", "users", "files"} {
		t.Run(phase, func(t *testing.T) {
			th, endpoint, prefix := setupIndexer(t)
			file, err := th.Store.FileInfo().Save(th.Context, &model.FileInfo{
				CreatorId: th.BasicUser.Id, PostId: th.BasicPost.Id, ChannelId: th.BasicChannel.Id,
				Name: "resume.txt", Path: "fixture/resume.txt", Extension: "txt", MimeType: "text/plain",
				Content: "ملف أقدم من مؤشر الرسائل", Size: 42, CreateAt: model.GetMillis() - 20000,
			})
			require.NoError(t, err)
			var extraIDs []string
			for n := 0; n < 8; n++ {
				switch phase {
				case "posts":
					post, err := th.Store.Post().Save(th.Context, &model.Post{
						ChannelId: th.BasicChannel.Id, UserId: th.BasicUser.Id, Message: "رسالة لاستئناف الفهرسة",
					})
					require.NoError(t, err)
					extraIDs = append(extraIDs, post.Id)
				case "channels":
					channel, err := th.Store.Channel().Save(th.Context, &model.Channel{
						TeamId: th.BasicTeam.Id, Name: model.NewId(), DisplayName: "قناة استئناف", Type: model.ChannelTypeOpen,
					}, 100)
					require.NoError(t, err)
					extraIDs = append(extraIDs, channel.Id)
				case "users":
					user, err := th.Store.User().Save(th.Context, &model.User{
						Username: "resume" + model.NewId(), Email: model.NewId() + "@example.test",
					})
					require.NoError(t, err)
					extraIDs = append(extraIDs, user.Id)
				case "files":
					file, err := th.Store.FileInfo().Save(th.Context, &model.FileInfo{
						CreatorId: th.BasicUser.Id, PostId: th.BasicPost.Id, ChannelId: th.BasicChannel.Id,
						Name: "extra.txt", Path: "fixture/" + model.NewId(), Extension: "txt", MimeType: "text/plain",
						Content: "ملف استئناف", Size: 20,
					})
					require.NoError(t, err)
					extraIDs = append(extraIDs, file.Id)
				}
			}
			var mu sync.Mutex
			writes := make(map[string]int)
			target, err := url.Parse(endpoint)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(target)
			forward := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/_bulk") {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					r.Body.Close()
					r.Body = io.NopCloser(bytes.NewReader(body))
					mu.Lock()
					for _, line := range bytes.Split(body, []byte{'\n'}) {
						var metadata map[string]struct {
							ID string `json:"_id"`
						}
						if json.Unmarshal(line, &metadata) == nil && metadata["index"].ID != "" {
							writes[metadata["index"].ID]++
						}
					}
					mu.Unlock()
				}
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(forward.Close)
			th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.ElasticsearchSettings.ConnectionURL = new(forward.URL)
				cfg.ElasticsearchSettings.BatchSize = new(2)
			})
			worker := New(th.Server).MakeWorker()
			require.NotNil(t, worker)
			gate := &checkpointReadGate{Store: worker.(*checkpointWorker).server.Store, phase: phase, entered: make(chan struct{}), release: make(chan struct{})}
			worker.(*checkpointWorker).server.Store = gate
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(gate.release) }) }
			th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, worker, nil)
			go worker.Run()
			t.Cleanup(func() { unblock(); worker.Stop() })
			job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeElasticsearchPostIndexing, map[string]string{"start_time": "0"})
			require.Nil(t, appErr)
			sendCheckpointJob(t, worker, job)
			select {
			case <-gate.entered:
			case <-time.After(20 * time.Second):
				t.Fatal("worker never reached the selected phase")
			}
			stopped := make(chan struct{})
			stopSignal := worker.(*checkpointWorker).stop
			go func() { worker.Stop(); close(stopped) }()
			select {
			case <-stopSignal:
			case <-time.After(5 * time.Second):
				t.Fatal("stop signal was not delivered")
			}
			unblock()
			select {
			case <-stopped:
			case <-time.After(20 * time.Second):
				t.Fatal("worker did not finish its graceful checkpoint")
			}
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, model.JobStatusPending, stored.Status)
			cp, err := loadIndexingCheckpoint(stored)
			require.NoError(t, err)
			p := cp.Progress
			done := map[string]bool{"posts": p.DonePosts, "channels": p.DoneChannels, "users": p.DoneUsers, "files": p.DoneFiles}
			require.False(t, done[phase], "fixture must stop inside the phase, not after it")
			require.Greater(t, p.LastEntityTime, p.StartAtTime)
			require.EqualValues(t, 0, p.StartAtTime)
			// A new worker reloads the same persisted Job. This is an explicit
			// graceful resume, not a claim of automatic crash recovery.
			resumed := New(th.Server).MakeWorker()
			th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, resumed, nil)
			go resumed.Run()
			t.Cleanup(resumed.Stop)
			sendCheckpointJob(t, resumed, stored)
			final := waitCheckpointJob(t, th, stored.Id, model.JobStatusSuccess)
			finished, err := loadIndexingCheckpoint(final)
			require.NoError(t, err)
			require.True(t, finished.Progress.IsDone(final))
			require.Equal(t, p.EndAtTime, finished.Progress.EndAtTime)
			for _, entity := range []struct{ index, id string }{
				{"posts*", th.BasicPost.Id}, {"channels", th.BasicChannel.Id}, {"users", th.BasicUser.Id}, {"files", file.Id},
			} {
				assertPostIndexed(t, endpoint, prefix+entity.index, entity.id)
			}
			for _, id := range extraIDs {
				assertPostIndexed(t, endpoint, prefix+phase, id)
			}
			mu.Lock()
			defer mu.Unlock()
			for id, count := range writes {
				require.Equal(t, 1, count, "already acknowledged document %s was replayed", id)
			}
		})
	}
}

func TestIndexingCheckpointRetriesSameJobAfterMissingAcknowledgements(t *testing.T) {
	th, endpoint, prefix := setupIndexer(t)
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var omit atomic.Bool
	omit.Store(true)
	fault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if omit.Load() && strings.HasSuffix(r.URL.Path, "/_bulk") {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			_, _ = io.WriteString(w, `{"errors":false,"items":[]}`)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(fault.Close)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.ConnectionURL = new(fault.URL) })
	worker := New(th.Server).MakeWorker()
	th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, worker, nil)
	go worker.Run()
	t.Cleanup(worker.Stop)
	failed := runIndexingJob(t, th, worker, model.JobStatusError)
	cp, err := loadIndexingCheckpoint(failed)
	require.NoError(t, err)
	require.Zero(t, cp.Progress.DonePostsCount)
	require.Empty(t, cp.Progress.LastPostID)
	omit.Store(false)
	requeued, err := th.Store.Job().UpdateStatusOptimistically(failed.Id, model.JobStatusError, model.JobStatusPending)
	require.NoError(t, err)
	require.NotNil(t, requeued)
	sendCheckpointJob(t, worker, requeued)
	waitCheckpointJob(t, th, failed.Id, model.JobStatusSuccess)
	assertPostIndexed(t, endpoint, prefix, th.BasicPost.Id)
}

func TestIndexingCheckpointRejectsChangedDestination(t *testing.T) {
	th, _, prefix := setupIndexer(t)
	worker := New(th.Server).MakeWorker()
	th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, worker, nil)
	go worker.Run()
	t.Cleanup(worker.Stop)
	finished := runIndexingJob(t, th, worker, model.JobStatusSuccess)
	worker.Stop()
	checkpoint := finished.Data[checkpointKey]
	requeued, err := th.Store.Job().UpdateStatusOptimistically(finished.Id, model.JobStatusSuccess, model.JobStatusPending)
	require.NoError(t, err)
	require.NotNil(t, requeued)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.IndexPrefix = new(prefix + "changed_") })
	resumed := New(th.Server).MakeWorker()
	th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, resumed, nil)
	go resumed.Run()
	t.Cleanup(resumed.Stop)
	sendCheckpointJob(t, resumed, requeued)
	failed := waitCheckpointJob(t, th, requeued.Id, model.JobStatusError)
	require.Contains(t, failed.Data["error"], "destination changed")
	require.Equal(t, checkpoint, failed.Data[checkpointKey], "a mismatched destination must not replace the old checkpoint")
}

func TestIndexingCheckpointStopsEnqueueingAfterLicenseRemoval(t *testing.T) {
	th, endpoint, prefix := setupIndexer(t)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.BatchSize = new(2) })
	worker := New(th.Server).MakeWorker()
	gate := &checkpointReadGate{Store: worker.(*checkpointWorker).server.Store, phase: "posts", entered: make(chan struct{}), release: make(chan struct{})}
	worker.(*checkpointWorker).server.Store = gate
	var once sync.Once
	unblock := func() { once.Do(func() { close(gate.release) }) }
	th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, worker, nil)
	go worker.Run()
	t.Cleanup(func() { unblock(); worker.Stop() })
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeElasticsearchPostIndexing, map[string]string{
		"start_time": "0", "index_posts": "true", "index_channels": "false", "index_users": "false", "index_files": "false",
	})
	require.Nil(t, appErr)
	sendCheckpointJob(t, worker, job)
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not reach second post batch")
	}
	th.Server.SetLicense(nil)
	unblock()
	var pending *model.Job
	require.Eventually(t, func() bool {
		var err error
		pending, err = th.Store.Job().Get(th.Context, job.Id)
		require.NoError(t, err)
		return pending.Status == model.JobStatusPending
	}, 20*time.Second, 25*time.Millisecond)
	cp, err := loadIndexingCheckpoint(pending)
	require.NoError(t, err)
	require.EqualValues(t, 2, cp.Progress.DonePostsCount, "only the batch queued before revocation may advance")
	require.False(t, cp.Progress.DonePosts)
	indexRequest(t, endpoint, http.MethodPost, "/"+prefix+"*/_refresh", "")
	response := indexRequest(t, endpoint, http.MethodPost, "/"+prefix+"*/_search", `{"size":0}`)
	require.EqualValues(t, 2, response["hits"].(map[string]any)["total"].(map[string]any)["value"])
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	sendCheckpointJob(t, worker, pending)
	waitCheckpointJob(t, th, pending.Id, model.JobStatusSuccess)
	assertPostIndexed(t, endpoint, prefix, th.BasicPost.Id)
}

func TestIndexingCancellationWinsDuringFinalBulkClose(t *testing.T) {
	testIndexingCancellationDuringClose(t, false)
}

func TestIndexingCancellationWinsWithStaleJobRead(t *testing.T) {
	testIndexingCancellationDuringClose(t, true)
}

func testIndexingCancellationDuringClose(t *testing.T, staleRead bool) {
	t.Helper()
	th, endpoint, _ := setupIndexer(t)
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	fault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_bulk") {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(fault.Close)
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ElasticsearchSettings.ConnectionURL = new(fault.URL)
		cfg.ElasticsearchSettings.RequestTimeoutSeconds = new(10)
	})
	worker := New(th.Server).MakeWorker()
	if staleRead {
		guard := worker.(*checkpointWorker).store
		guard.JobStore = &staleCancellationRead{JobStore: guard.JobStore}
	}
	th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, worker, nil)
	go worker.Run()
	t.Cleanup(func() { unblock(); worker.Stop() })
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeElasticsearchPostIndexing, map[string]string{
		"start_time": "0", "index_posts": "true", "index_channels": "false", "index_users": "false", "index_files": "false",
	})
	require.Nil(t, appErr)
	sendCheckpointJob(t, worker, job)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("bulk request not observed")
	}
	require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
	if staleRead {
		stale, err := worker.(*checkpointWorker).store.JobStore.Get(th.Context, job.Id)
		require.NoError(t, err)
		require.Equal(t, model.JobStatusInProgress, stale.Status, "the simulated replica really must lag")
		authoritative, err := th.Store.Job().Get(th.Context, job.Id)
		require.NoError(t, err)
		require.Equal(t, model.JobStatusCancelRequested, authoritative.Status)
	}
	unblock()
	waitCheckpointJob(t, th, job.Id, model.JobStatusCanceled)
}

// Only this worker's reads are stale. The authoritative SQL row and all CAS
// writes remain real; simulates replica lag without modifying an app database.
type staleCancellationRead struct{ store.JobStore }

func (s *staleCancellationRead) Get(rctx request.CTX, id string) (*model.Job, error) {
	job, err := s.JobStore.Get(rctx, id)
	if err == nil && job.Status == model.JobStatusCancelRequested {
		job.Status = model.JobStatusInProgress
	}
	return job, err
}

func sendCheckpointJob(t *testing.T, worker model.Worker, job *model.Job) {
	t.Helper()
	select {
	case worker.JobChannel() <- *job:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not accept checkpoint job")
	}
}

func waitCheckpointJob(t *testing.T, th *api4.TestHelper, id, status string) *model.Job {
	t.Helper()
	var job *model.Job
	require.Eventually(t, func() bool {
		var err error
		job, err = th.Store.Job().Get(th.Context, id)
		require.NoError(t, err)
		return job.Status == model.JobStatusSuccess || job.Status == model.JobStatusError || job.Status == model.JobStatusCanceled
	}, 30*time.Second, 25*time.Millisecond)
	require.Equal(t, status, job.Status, "%v", job.Data)
	return job
}

type checkpointReadGate struct {
	store.Store
	phase            string
	reads            int
	entered, release chan struct{}
}

func (s *checkpointReadGate) wait(phase string) {
	if s.phase == phase {
		s.reads++
		if s.reads == 2 {
			close(s.entered)
			<-s.release
		}
	}
}

func (s *checkpointReadGate) Post() store.PostStore {
	return &checkpointPostGate{PostStore: s.Store.Post(), gate: s}
}
func (s *checkpointReadGate) Channel() store.ChannelStore {
	return &checkpointChannelGate{ChannelStore: s.Store.Channel(), gate: s}
}
func (s *checkpointReadGate) User() store.UserStore {
	return &checkpointUserGate{UserStore: s.Store.User(), gate: s}
}
func (s *checkpointReadGate) FileInfo() store.FileInfoStore {
	return &checkpointFileGate{FileInfoStore: s.Store.FileInfo(), gate: s}
}

type checkpointPostGate struct {
	store.PostStore
	gate *checkpointReadGate
}

func (s *checkpointPostGate) GetPostsBatchForIndexing(at int64, id string, limit int) ([]*model.PostForIndexing, error) {
	s.gate.wait("posts")
	return s.PostStore.GetPostsBatchForIndexing(at, id, limit)
}

type checkpointChannelGate struct {
	store.ChannelStore
	gate *checkpointReadGate
}

func (s *checkpointChannelGate) GetChannelsBatchForIndexing(at int64, id string, limit int) ([]*model.Channel, error) {
	s.gate.wait("channels")
	return s.ChannelStore.GetChannelsBatchForIndexing(at, id, limit)
}

type checkpointUserGate struct {
	store.UserStore
	gate *checkpointReadGate
}

func (s *checkpointUserGate) GetUsersBatchForIndexing(at int64, id string, limit int) ([]*model.UserForIndexing, error) {
	s.gate.wait("users")
	return s.UserStore.GetUsersBatchForIndexing(at, id, limit)
}

type checkpointFileGate struct {
	store.FileInfoStore
	gate *checkpointReadGate
}

func (s *checkpointFileGate) GetFilesBatchForIndexing(at int64, id string, deleted bool, limit int) ([]*model.FileForIndexing, error) {
	s.gate.wait("files")
	return s.FileInfoStore.GetFilesBatchForIndexing(at, id, deleted, limit)
}
