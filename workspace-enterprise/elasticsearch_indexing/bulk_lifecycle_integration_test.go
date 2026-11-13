package elasticsearch_indexing

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/enterprise/elasticsearch/common"
	"github.com/stretchr/testify/require"
)

func TestIndexingStopCancelsSilentBulkClose(t *testing.T) {
	for _, responseStarted := range []bool{false, true} {
		name := "headers"
		if responseStarted {
			name = "body"
		}
		t.Run(name, func(t *testing.T) {
			th, endpoint, prefix := setupIndexer(t)
			target, err := url.Parse(endpoint)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(target)
			entered, release := make(chan struct{}, 1), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			var block atomic.Bool
			block.Store(true)
			var active, attempts atomic.Int64
			fault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/_bulk") && block.Load() {
					attempts.Add(1)
					active.Add(1)
					defer active.Add(-1)
					_, _ = io.Copy(io.Discard, r.Body)
					if responseStarted {
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("X-Elastic-Product", "Elasticsearch")
						_, _ = io.WriteString(w, `{"took":1,`)
						w.(http.Flusher).Flush()
					}
					select {
					case entered <- struct{}{}:
					default:
					}
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(fault.Close)
			th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.ElasticsearchSettings.ConnectionURL = new(fault.URL)
				cfg.ElasticsearchSettings.RequestTimeoutSeconds = new(30)
			})
			worker := New(th.Server).MakeWorker().(*checkpointWorker)
			t.Cleanup(func() { unblock(); worker.Stop() })
			go worker.Run()
			job := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, model.StringMap{
				"start_time": "0", "index_posts": "true", "index_channels": "false", "index_users": "false", "index_files": "false",
			})
			sendCheckpointJob(t, worker, job)
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("final Bulk flush did not reach the silent endpoint")
			}
			stopped := make(chan struct{})
			go func() { worker.Stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(6 * time.Second):
				t.Fatal("Stop remained blocked in final Bulk Close")
			}
			require.Eventually(t, func() bool { return active.Load() == 0 }, time.Second, 10*time.Millisecond, "a returned Stop must not leave its HTTP request running")
			before := attempts.Load()
			time.Sleep(150 * time.Millisecond)
			require.Equal(t, before, attempts.Load(), "no retry may outlive the stopped processor")
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, model.JobStatusPending, stored.Status)
			cp, err := loadIndexingCheckpoint(stored)
			require.NoError(t, err)
			require.Zero(t, cp.Progress.DonePostsCount, "unacknowledged writes must remain replayable")
			require.False(t, cp.Progress.IsDone(stored))
			block.Store(false)
			go worker.Run()
			sendCheckpointJob(t, worker, stored)
			waitCheckpointJob(t, th, job.Id, model.JobStatusSuccess)
			assertPostIndexed(t, endpoint, prefix+"posts*", th.BasicPost.Id)
		})
	}
}

// These processor tests use the production factory/Add/Close and native queues.
// They disable SQL ownership checks because no SQL Job is claimed in this fixture;
// the worker/Stop test above covers the real ownership and checkpoint path.
func TestIndexingBulkQueueCancellationJoinsProcessor(t *testing.T) {
	th, endpoint, prefix := setupIndexer(t)
	var active atomic.Int64
	block := new(atomic.Bool)
	block.Store(true)
	fault := bulkLifecycleEndpoint(t, endpoint, func(w http.ResponseWriter, r *http.Request) {
		if !block.Load() {
			return
		}
		active.Add(1)
		defer active.Add(-1)
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}, block)
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ElasticsearchSettings.ConnectionURL = new(fault.URL)
		cfg.ElasticsearchSettings.RequestTimeoutSeconds = new(30)
	})
	w := New(th.Server).MakeWorker().(*checkpointWorker)
	w.fence.enabled = false
	w.store.postgres = false
	ctx, cancel := context.WithCancel(context.Background())
	w.fence.work.Store(&indexingWorkContext{ctx: ctx})
	require.NoError(t, w.create())
	var closeOnce sync.Once
	closeProcessor := func() { closeOnce.Do(func() { _, _ = w.close() }) }
	t.Cleanup(func() { cancel(); closeProcessor() })
	workers := int64(common.NumIndexWorkers())
	body := `{"message":"` + strings.Repeat("x", 6_000_000) + `"}`
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := int64(0); n < 4*workers; n++ {
			if w.add(prefix+"queue", "index", model.NewId(), strings.NewReader(body)) != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	require.Eventually(t, func() bool { return active.Load() == workers && w.results().added.Load() == 2*workers+1 }, 10*time.Second, 10*time.Millisecond, "all native queues must fill and Add must actually block")
	select {
	case <-done:
		t.Fatal("Add was not blocked by native back-pressure")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not interrupt a full native Add queue")
	}
	closed := make(chan struct{})
	go func() { closeProcessor(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled processor workers were not joined")
	}
	require.Eventually(t, func() bool { return active.Load() == 0 }, time.Second, 10*time.Millisecond)
	require.Positive(t, w.results().failed.Load(), "rejected queued items must never become successful acknowledgements")
	completed := w.results().completed.Load()
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, completed, w.results().completed.Load(), "no callback may outlive Close")
	block.Store(false)
	w.fence.work.Store(&indexingWorkContext{ctx: context.Background()})
	require.NoError(t, w.create())
	id := model.NewId()
	require.NoError(t, w.add(prefix+"queue", "index", id, strings.NewReader(`{"message":"successor"}`)))
	failed, err := w.close()
	require.NoError(t, err)
	require.Zero(t, failed)
	assertPostIndexed(t, endpoint, prefix+"queue", id)
}

func TestIndexingBulkCloseBudgetIncludesQueuedFlushesAndRetries(t *testing.T) {
	th, endpoint, prefix := setupIndexer(t)
	var active, rejected, attempts atomic.Int64
	block := new(atomic.Bool)
	block.Store(true)
	fault := bulkLifecycleEndpoint(t, endpoint, func(w http.ResponseWriter, r *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		attempts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
		rejected.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"fixture unavailable","status":503}`)
	}, block)
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ElasticsearchSettings.ConnectionURL = new(fault.URL)
		cfg.ElasticsearchSettings.RequestTimeoutSeconds = new(1)
	})
	w := New(th.Server).MakeWorker().(*checkpointWorker)
	w.fence.enabled = false
	w.store.postgres = false
	ctx, cancel := context.WithCancel(context.Background())
	w.fence.work.Store(&indexingWorkContext{ctx: ctx})
	require.NoError(t, w.create())
	var once sync.Once
	var failed int64
	var closeErr error
	closeProcessor := func() { once.Do(func() { failed, closeErr = w.close() }) }
	t.Cleanup(func() { cancel(); closeProcessor() })
	body := `{"message":"` + strings.Repeat("x", 6_000_000) + `"}`
	workers := common.NumIndexWorkers()
	for n := 0; n < 2*workers; n++ {
		require.NoError(t, w.add(prefix+"budget", "index", model.NewId(), strings.NewReader(body)))
	}
	started := time.Now()
	closeProcessor()
	limit := 1800 * time.Millisecond
	if *th.App.Config().ElasticsearchSettings.Backend == model.ElasticsearchSettingsOSBackend {
		limit = 4800 * time.Millisecond // final OpenSearch buffers drain serially
	}
	require.Less(t, time.Since(started), limit, "one close budget must cover all queued flushes and retry waits")
	require.Error(t, closeErr)
	require.Positive(t, failed)
	require.Positive(t, rejected.Load(), "fixture must actually return retryable HTTP 503")
	require.Greater(t, attempts.Load(), int64(workers), "at least one retry must really dispatch")
	require.Eventually(t, func() bool { return active.Load() == 0 }, time.Second, 10*time.Millisecond)
	before := attempts.Load()
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, before, attempts.Load())
}

func TestIndexingBulkCloseJoinsSilentDiscovery(t *testing.T) {
	th, endpoint, _ := setupIndexer(t)
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	entered := make(chan struct{}, 1)
	var active, attempts atomic.Int64
	fault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_nodes") {
			attempts.Add(1)
			active.Add(1)
			defer active.Add(-1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"nodes":{`)
			w.(http.Flusher).Flush()
			select {
			case entered <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(fault.Close)
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ElasticsearchSettings.ConnectionURL = new(fault.URL)
		cfg.ElasticsearchSettings.RequestTimeoutSeconds = new(30)
		cfg.ElasticsearchSettings.Sniff = new(true)
	})
	w := New(th.Server).MakeWorker().(*checkpointWorker)
	w.fence.enabled = false
	w.store.postgres = false
	require.Zero(t, attempts.Load(), "discovery must not start outside a job")
	require.NoError(t, w.create())
	var once sync.Once
	closeProcessor := func() { once.Do(func() { _, _ = w.close() }) }
	t.Cleanup(closeProcessor)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("configured node discovery did not start")
	}
	closed := make(chan struct{})
	go func() { closeProcessor(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not join silent discovery")
	}
	require.Eventually(t, func() bool { return active.Load() == 0 }, time.Second, 10*time.Millisecond)
	before := attempts.Load()
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, before, attempts.Load())
}

func bulkLifecycleEndpoint(t *testing.T, endpoint string, bulk http.HandlerFunc, block *atomic.Bool) *httptest.Server {
	t.Helper()
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	fault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_bulk") && block.Load() {
			bulk(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(fault.Close)
	return fault
}

func TestIndexingBulkInterruptionPreservesOwnership(t *testing.T) {
	for _, action := range []string{"cancel", "license", "configuration", "destination"} {
		t.Run(action, func(t *testing.T) {
			th, endpoint, _ := setupIndexer(t)
			entered := make(chan struct{}, 1)
			var active atomic.Int64
			block := new(atomic.Bool)
			block.Store(true)
			fault := bulkLifecycleEndpoint(t, endpoint, func(_ http.ResponseWriter, r *http.Request) {
				active.Add(1)
				defer active.Add(-1)
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case entered <- struct{}{}:
				default:
				}
				<-r.Context().Done()
			}, block)
			th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.ElasticsearchSettings.ConnectionURL = new(fault.URL)
				cfg.ElasticsearchSettings.RequestTimeoutSeconds = new(30)
			})
			w := New(th.Server).MakeWorker().(*checkpointWorker)
			go w.Run()
			t.Cleanup(w.Stop)
			job := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, model.StringMap{
				"start_time": "0", "index_posts": "true", "index_channels": "false", "index_users": "false", "index_files": "false",
			})
			sendCheckpointJob(t, w, job)
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("Bulk did not start before interruption")
			}
			want, timeout := model.JobStatusPending, 6*time.Second
			switch action {
			case "cancel":
				require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
				want, timeout = model.JobStatusCanceled, 11*time.Second
			case "license":
				th.Server.SetLicense(nil)
			case "configuration":
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.EnableIndexing = new(false) })
			case "destination":
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.ConnectionURL = new(endpoint) })
			}
			require.Eventually(t, func() bool {
				stored, err := th.Store.Job().Get(th.Context, job.Id)
				return err == nil && stored.Status == want
			}, timeout, 10*time.Millisecond, "normal interruption must preserve ownership long enough for its final SQL transition")
			require.Eventually(t, func() bool { return active.Load() == 0 }, time.Second, 10*time.Millisecond)
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			cp, err := loadIndexingCheckpoint(stored)
			require.NoError(t, err)
			require.Zero(t, cp.Progress.DonePostsCount)
			require.False(t, cp.Progress.IsDone(stored))
		})
	}
}

func TestIndexingBulkFactoryHandlesCanceledParent(t *testing.T) {
	th, endpoint, prefix := setupIndexer(t)
	var dispatched atomic.Int64
	block := new(atomic.Bool)
	block.Store(true)
	fault := bulkLifecycleEndpoint(t, endpoint, func(w http.ResponseWriter, _ *http.Request) {
		dispatched.Add(1)
		http.Error(w, "a canceled processor must not dispatch", http.StatusInternalServerError)
	}, block)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.ConnectionURL = new(fault.URL) })
	w := New(th.Server).MakeWorker().(*checkpointWorker)
	w.fence.enabled = false
	w.store.postgres = false
	for n := 0; n < 4; n++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		w.fence.work.Store(&indexingWorkContext{ctx: ctx})
		require.NoError(t, w.create())
		require.Error(t, w.add(prefix+"canceled", "index", model.NewId(), strings.NewReader(`{}`)))
		failed, err := w.close()
		require.Error(t, err)
		require.Positive(t, failed)
	}
	require.Zero(t, dispatched.Load())
}
