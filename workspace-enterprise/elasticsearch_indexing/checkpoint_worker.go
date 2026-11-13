package elasticsearch_indexing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/configservice"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/mattermost/mattermost/server/v8/enterprise/elasticsearch/common"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
)

const checkpointKey = "workspace_indexing_checkpoint"
const checkpointTargetKey = "workspace_indexing_target"

type indexingCheckpoint struct {
	Version  int                     `json:"version"`
	Progress common.IndexingProgress `json:"progress"`
	Entities map[string]string       `json:"entities"`
	Target   string                  `json:"target"`
}

// The common batch transformations remain authoritative. This lifecycle keeps
// their complete progress snapshot, and commits only a contiguous prefix of
// batches whose individual bulk items were acknowledged successfully.
type checkpointWorker struct {
	batcher         *common.IndexerWorker
	server          *jobs.JobServer
	logger          mlog.LoggerIFace
	store           *checkpointJobStore
	create          func() error
	close           func() (int64, error)
	add             func(string, string, string, io.ReadSeeker) error
	results         func() *bulkResults
	liveConfig      func() *model.Config
	target          string
	jobs            chan model.Job
	mu              sync.Mutex
	stop            chan struct{}
	done            chan struct{}
	databaseDSN     string
	queryTimeout    time.Duration
	readSources     []string
	readStore       *indexingStore
	workContext     context.Context
	master          *sql.DB
	fence           *indexingFence
	publish         func(*model.WebSocketEvent)
	metrics         einterfaces.MetricsInterface
	recoveryAfterAt int64
	recoveryAfterID string
}

func newCheckpointWorker(server *app.Server, name, backend string, jobServer *jobs.JobServer, logger mlog.LoggerIFace,
	files filestore.FileBackend, license func() *model.License, create func() error,
	add func(string, string, string, io.ReadSeeker) error, closeProcessor func() (int64, error), results func() *bulkResults, fence *indexingFence,
) model.Worker {
	readStore, _ := jobServer.Store.(*indexingStore)
	guard := &checkpointJobStore{JobStore: jobServer.Store.Job()}
	wrapped := &checkpointWorkerStore{Store: jobServer.Store, job: guard}
	frozen := server.Config().Clone()
	guard.postgres = model.SafeDereference(frozen.SqlSettings.DriverName) == model.DatabaseDriverPostgres
	// Count ownership in this process with a defer, including session-loss
	// paths that cannot update SQL. Recovery must not decrement a dead node's gauge.
	jobServer = jobs.NewJobServer(&indexingConfig{ConfigService: server.Platform(), config: frozen}, wrapped, nil, server.Log(), server.Platform().Publish)
	licensedAdd := func(index, action, id string, body io.ReadSeeker) error {
		if guard.postgres && (guard.execution == nil || guard.execution.lost.Load()) {
			results().addItem()(true)
			return errIndexingOwnershipLost
		}
		if current := license(); current == nil || current.Features.Elasticsearch == nil || !*current.Features.Elasticsearch || !*server.Config().ElasticsearchSettings.EnableIndexing {
			results().addItem()(true)
			return errors.New("indexing license or configuration disabled before enqueue")
		}
		return add(index, action, id, body)
	}
	w := &checkpointWorker{
		batcher: common.NewIndexerWorker(name, backend, jobServer, logger, files, license, create, licensedAdd, closeProcessor),
		server:  jobServer, logger: logger, store: guard, create: create, close: closeProcessor, add: licensedAdd, results: results,
		liveConfig: server.Config, target: indexingTarget(frozen),
		jobs:  make(chan model.Job),
		fence: fence, publish: server.Platform().Publish, metrics: server.GetMetrics(),
		readStore: readStore, readSources: indexingReadSources(frozen.SqlSettings),
	}
	if guard.postgres {
		w.databaseDSN = model.SafeDereference(frozen.SqlSettings.DataSource)
		w.queryTimeout = time.Duration(model.SafeDereference(frozen.SqlSettings.QueryTimeout)) * time.Second
		w.master = server.Jobs.Store.GetInternalMasterDB()
	}
	return w
}

func (w *checkpointWorker) IsEnabled(config *model.Config) bool { return w.batcher.IsEnabled(config) }
func (w *checkpointWorker) JobChannel() chan<- model.Job        { return w.jobs }

func (w *checkpointWorker) Run() {
	w.mu.Lock()
	if w.done != nil {
		w.mu.Unlock()
		return
	}
	stop, done := make(chan struct{}), make(chan struct{})
	w.stop, w.done = stop, done
	w.mu.Unlock()
	defer close(done)
	w.recoverInterruptedJobs(stop)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			w.recoverInterruptedJobs(stop)
		case job := <-w.jobs:
			w.doJob(job, stop)
		}
	}
}

func (w *checkpointWorker) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done == nil {
		return
	}
	close(w.stop)
	<-w.done
	w.done, w.stop = nil, nil
}

type acknowledgedBatch struct {
	progress common.IndexingProgress
	results  *bulkBatchResults
}

func (w *checkpointWorker) doJob(input model.Job, stop <-chan struct{}) {
	if !w.IsEnabled(w.liveConfig()) {
		return
	}
	if w.store.postgres {
		execution, err := newIndexingExecution(w.databaseDSN, w.queryTimeout)
		if err != nil {
			w.logger.Error("Unable to open indexing execution", mlog.Err(err))
			return
		}
		defer execution.Close()
		acquired, err := execution.acquire(context.Background(), input.Id)
		if err != nil || !acquired {
			return
		}
		w.store.execution = execution
		defer func() { w.fence.execution.Store(nil); w.store.execution = nil }()
	}
	job, appErr := w.server.ClaimJob(&input)
	if appErr != nil || job == nil {
		return
	}
	if w.metrics != nil {
		w.metrics.IncrementJobActive(job.Type)
		defer w.metrics.DecrementJobActive(job.Type)
	}
	if w.store.postgres {
		w.fence.execution.Store(w.store.execution)
	}
	defer w.server.HandleJobPanic(w.logger, job)
	w.store.candidate = job
	checkpoint, err := loadIndexingCheckpoint(job)
	if err != nil {
		w.finish(job, model.JobStatusError, err)
		return
	}
	if job.Data[checkpointKey] != "" && checkpoint.Target != w.target {
		w.finish(job, model.JobStatusError, errors.New("indexing destination changed; create a new indexing job for the new destination"))
		return
	}
	job.Data[checkpointTargetKey] = w.target
	for entity, enabled := range checkpoint.Entities {
		job.Data[entity] = enabled
	}
	delete(job.Data, "error")
	workCtx, canceled, stopWork := w.startIndexingWork(stop, job.Id)
	w.fence.work.Store(&indexingWorkContext{ctx: workCtx})
	defer w.fence.work.Store(nil)
	defer func() { stopWork(); w.workContext = nil }()
	if w.readStore != nil {
		reader := newIndexingReader(workCtx, w.readSources, w.queryTimeout)
		w.readStore.reader = reader
		defer func() { reader.Close(); w.readStore.reader = nil }()
	}
	progress := checkpoint.Progress
	w.populateTotals(&progress, job)
	safe := progress
	if err := w.saveProgress(job, safe); err != nil {
		w.finish(job, model.JobStatusError, err)
		return
	}
	lastSaved, lastActivity := safe, time.Now()
	if state := w.batchInterruption(stop, canceled); state != "" {
		if cause := indexingWorkCause(workCtx); cause != nil {
			err = cause.err
		}
		w.finish(job, state, err)
		return
	}
	if err := w.create(); err != nil {
		w.finish(job, model.JobStatusError, fmt.Errorf("create bulk processor: %w", err))
		return
	}
	var closeOnce sync.Once
	var closeErr error
	drain := func() {
		closeOnce.Do(func() {
			failed, err := w.close()
			closeErr = err
			if failed > 0 {
				closeErr = errors.Join(closeErr, fmt.Errorf("bulk processor failed or did not acknowledge %d items", failed))
			}
		})
	}
	defer drain()
	results := w.results()
	var pending []acknowledgedBatch
	advance := func() {
		for len(pending) > 0 && pending[0].results.confirmed() {
			safe = pending[0].progress
			pending[0] = acknowledgedBatch{}
			pending = pending[1:]
		}
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	status := model.JobStatusSuccess
	for !progress.IsDone(job) {
		if w.store.postgres {
			if checkErr := w.store.execution.check(context.Background()); checkErr != nil {
				err = checkErr
				status = model.JobStatusError
				break
			}
		}
		if !w.IsEnabled(w.liveConfig()) || indexingTarget(w.liveConfig()) != w.target {
			status = model.JobStatusPending
			break
		}
		select {
		case <-stop:
			status = model.JobStatusPending
		case <-canceled:
			status = model.JobStatusCanceled
		case <-timer.C:
			batch := results.beginBatch()
			var batchErr error
			var interrupted string
			progress, interrupted, batchErr = w.indexBatch(progress, job, stop, canceled)
			if interrupted != "" {
				status = interrupted
				break
			}
			if batchErr != nil {
				err = batchErr
				status = model.JobStatusError
				break
			}
			pending = append(pending, acknowledgedBatch{progress: progress, results: batch})
			advance()
			if !w.IsEnabled(w.liveConfig()) {
				status = model.JobStatusPending
			} else if results.failed.Load() > 0 || results.processorError.Load() {
				err = errors.New("bulk processor reported a write failure")
				status = model.JobStatusError
			} else if len(pending) > 1024 {
				// Bound checkpoint metadata even when a faulty proxy silently
				// omits callbacks. No post bodies are retained in this ledger.
				err = errors.New("too many batches without complete bulk acknowledgements")
				status = model.JobStatusError
			} else if safe != lastSaved || time.Since(lastActivity) >= 5*time.Second {
				// A buffered bulk processor may not confirm anything for 30s.
				// Retain the heartbeat without writing the same checkpoint 10/s.
				if err = w.saveProgress(job, safe); err != nil {
					status = model.JobStatusError
				} else {
					lastSaved, lastActivity = safe, time.Now()
				}
			}
		}
		if status != model.JobStatusSuccess {
			break
		}
		timer.Reset(100 * time.Millisecond)
	}
	// Flush once per job, including stop/cancel/error paths. Closing a bulk
	// processor per batch would impose a connection/flush cost on every batch.
	drain()
	advance()
	stopWork()
	if cause := indexingWorkCause(workCtx); cause != nil {
		status = cause.status
		err = errors.Join(err, cause.err)
	}
	if status == model.JobStatusSuccess && (closeErr != nil || len(pending) != 0) {
		err = errors.Join(closeErr, errors.New("indexing ended without confirming every batch"))
		status = model.JobStatusError
	}
	if encodeErr := putIndexingCheckpoint(job, safe); encodeErr != nil {
		err = errors.Join(err, encodeErr)
		status = model.JobStatusError
	}
	w.finish(job, status, err)
}

func (w *checkpointWorker) saveProgress(job *model.Job, progress common.IndexingProgress) error {
	if err := putIndexingCheckpoint(job, progress); err != nil {
		return err
	}
	if appErr := w.server.SetJobProgress(job, progress.CurrentProgress()); appErr != nil {
		return appErr
	}
	return nil
}

func (w *checkpointWorker) populateTotals(p *common.IndexingProgress, job *model.Job) {
	// Counts affect the displayed percentage, never the checkpoint cursor. As
	// in the common worker, a failed analytics query must not abort indexing.
	count := func(enabled string, done, previous, estimate int64, query func() (int64, error)) int64 {
		if job.Data[enabled] == "false" {
			return 0
		}
		n, err := query()
		if err == nil {
			return n
		}
		w.logger.Warn("Unable to count indexing entities; retaining estimated progress", mlog.String("entity", enabled), mlog.Err(err))
		return max(done, previous, estimate)
	}
	p.TotalPostsCount = count("index_posts", p.DonePostsCount, p.TotalPostsCount, 10000000, func() (int64, error) {
		return w.server.Store.Post().AnalyticsPostCount(&model.PostCountOptions{})
	})
	p.TotalChannelsCount = count("index_channels", p.DoneChannelsCount, p.TotalChannelsCount, 100000, func() (int64, error) {
		return w.server.Store.Channel().AnalyticsTypeCount("", "")
	})
	p.TotalUsersCount = count("index_users", p.DoneUsersCount, p.TotalUsersCount, 10000, func() (int64, error) {
		return w.server.Store.User().Count(model.UserCountOptions{IncludeBotAccounts: true})
	})
	p.TotalFilesCount = count("index_files", p.DoneFilesCount, p.TotalFilesCount, 100000, w.server.Store.FileInfo().CountAll)
}

func (w *checkpointWorker) finish(job *model.Job, status string, cause error) {
	w.store.candidate = job
	if cause != nil {
		job.Data["error"] = cause.Error()
	}
	var appErr *model.AppError
	switch status {
	case model.JobStatusSuccess:
		job.Progress = 100
		appErr = w.server.SetJobSuccess(job)
	case model.JobStatusPending:
		appErr = w.server.SetJobPending(job)
	case model.JobStatusCanceled:
		appErr = w.server.SetJobCanceled(job)
	default:
		job.Progress = -1
		appErr = w.server.SetJobError(job, nil)
	}
	if appErr != nil && status != model.JobStatusCanceled {
		// A cancellation racing with Close or a progress update wins over
		// success, pending, and error. Use the primary's status CAS directly;
		// Get may read a replica that has not seen the cancellation yet.
		appErr = w.server.SetJobCanceled(job)
	}
	if appErr != nil {
		w.logger.Error("Unable to commit indexing job result", mlog.String("job_id", job.Id), mlog.Err(appErr))
	}
}

func loadIndexingCheckpoint(job *model.Job) (indexingCheckpoint, error) {
	if job.Data == nil {
		job.Data = model.StringMap{}
	}
	if raw := job.Data[checkpointKey]; raw != "" {
		var cp indexingCheckpoint
		if err := json.Unmarshal([]byte(raw), &cp); err != nil {
			return cp, fmt.Errorf("invalid indexing checkpoint: %w", err)
		}
		p := cp.Progress
		if cp.Version != 1 || p.Now.IsZero() || p.StartAtTime < 0 || p.EndAtTime < p.StartAtTime || p.LastEntityTime < p.StartAtTime ||
			p.DonePostsCount < 0 || p.DoneChannelsCount < 0 || p.DoneUsersCount < 0 || p.DoneFilesCount < 0 || len(cp.Entities) != 4 {
			return cp, errors.New("invalid indexing checkpoint scope or progress")
		}
		for _, entity := range []string{"posts", "channels", "users", "files"} {
			if value := cp.Entities["index_"+entity]; value != "true" && value != "false" {
				return cp, errors.New("invalid indexing checkpoint entity selection")
			}
		}
		return cp, nil
	}
	cp := indexingCheckpoint{Version: 1, Entities: make(map[string]string)}
	cp.Progress.Now = time.Now()
	cp.Progress.EndAtTime = model.GetMillis()
	start := job.Data["start_time"]
	// Legacy cursors were saved before acknowledgement. Replay their original
	// known scope rather than treating an unproven cursor as durable evidence.
	if original, ok := job.Data["original_start_time"]; ok {
		start = original
	}
	var err error
	if start != "" {
		cp.Progress.StartAtTime, err = strconv.ParseInt(start, 10, 64)
		if err != nil || cp.Progress.StartAtTime < 0 {
			return cp, errors.New("invalid indexing start time")
		}
	}
	if end := job.Data["end_time"]; end != "" {
		cp.Progress.EndAtTime, err = strconv.ParseInt(end, 10, 64)
		if err != nil || cp.Progress.EndAtTime < cp.Progress.StartAtTime {
			return cp, errors.New("invalid indexing end time")
		}
	}
	cp.Progress.LastEntityTime = cp.Progress.StartAtTime
	if cp.Progress.EndAtTime < cp.Progress.StartAtTime {
		return cp, errors.New("indexing end time precedes start time")
	}
	for _, entity := range []string{"posts", "channels", "users", "files"} {
		key := "index_" + entity
		value, specified := job.Data[key]
		cp.Entities[key] = strconv.FormatBool(!specified || value == "true")
	}
	return cp, nil
}

func putIndexingCheckpoint(job *model.Job, progress common.IndexingProgress) error {
	entities := make(map[string]string, 4)
	for _, name := range []string{"posts", "channels", "users", "files"} {
		entities["index_"+name] = job.Data["index_"+name]
	}
	raw, err := json.Marshal(indexingCheckpoint{Version: 1, Progress: progress, Entities: entities, Target: job.Data[checkpointTargetKey]})
	if err != nil {
		return err
	}
	job.Data[checkpointKey] = string(raw)
	job.Data["start_time"] = strconv.FormatInt(progress.LastEntityTime, 10)
	job.Data["original_start_time"] = strconv.FormatInt(progress.StartAtTime, 10)
	job.Data["end_time"] = strconv.FormatInt(progress.EndAtTime, 10)
	job.Data["start_post_id"], job.Data["start_channel_id"] = progress.LastPostID, progress.LastChannelID
	job.Data["start_user_id"], job.Data["start_file_id"] = progress.LastUserID, progress.LastFileID
	counts := map[string]int64{
		"done_posts_count": progress.DonePostsCount, "done_channels_count": progress.DoneChannelsCount,
		"done_users_count": progress.DoneUsersCount, "done_files_count": progress.DoneFilesCount,
		"total_posts_count": progress.TotalPostsCount, "total_channels_count": progress.TotalChannelsCount,
		"total_users_count": progress.TotalUsersCount, "total_files_count": progress.TotalFilesCount,
	}
	for key, count := range counts {
		job.Data[key] = strconv.FormatInt(count, 10)
	}
	job.Progress = progress.CurrentProgress()
	return nil
}

type indexingConfig struct {
	configservice.ConfigService
	config *model.Config
}

func (c *indexingConfig) Config() *model.Config { return c.config }

func indexingTarget(config *model.Config) string {
	s := config.ElasticsearchSettings
	// Credentials and TLS settings may rotate without changing the data
	// destination. Persist a digest, never connection secrets, in Job.Data.
	endpoint := *s.ConnectionURL
	if parsed, err := url.Parse(endpoint); err == nil {
		parsed.User = nil
		endpoint = parsed.String()
	}
	raw, _ := json.Marshal(struct {
		Backend, URL, Prefix string
		AggregateDays        int
		Blocks               bool
	}{*s.Backend, endpoint, *s.IndexPrefix, *s.AggregatePostsAfterDays, config.FeatureFlags.MmBlocksEnabled})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

type checkpointWorkerStore struct {
	store.Store
	job *checkpointJobStore
}

func (s *checkpointWorkerStore) Job() store.JobStore { return s.job }

// Keep the JobServer's normal metrics/status handling while replacing its
// unconditional terminal update with an atomic status + checkpoint transition.
type checkpointJobStore struct {
	store.JobStore
	candidate *model.Job // used only by the worker, never the cancellation reader
	postgres  bool
	execution *indexingExecution
}

func (s *checkpointJobStore) UpdateStatusOptimistically(id, from, to string) (*model.Job, error) {
	if !s.postgres {
		return s.JobStore.UpdateStatusOptimistically(id, from, to)
	}
	if s.execution == nil || from != model.JobStatusPending || to != model.JobStatusInProgress {
		return nil, errIndexingOwnershipLost
	}
	return s.execution.claim(id)
}

func (s *checkpointJobStore) UpdateOptimistically(job *model.Job, from string) (*model.Job, error) {
	if !s.postgres {
		return s.JobStore.UpdateOptimistically(job, from)
	}
	if s.execution == nil {
		return nil, errIndexingOwnershipLost
	}
	updated, err := s.execution.update(job, from)
	if err == nil && updated == nil && job.Status == model.JobStatusInProgress {
		err = errIndexingOwnershipLost
	}
	return updated, err
}

func (s *checkpointJobStore) UpdateStatus(id, status string) (*model.Job, error) {
	if s.candidate == nil || s.candidate.Id != id {
		return nil, errors.New("indexing status transition has no checkpoint candidate")
	}
	candidate := *s.candidate
	candidate.Status = status
	candidate.LastActivityAt = model.GetMillis()
	expected := model.JobStatusInProgress
	if status == model.JobStatusCanceled {
		expected = model.JobStatusCancelRequested
	}
	updated, err := s.UpdateOptimistically(&candidate, expected)
	if err == nil && updated == nil {
		err = errors.New("indexing job status changed before checkpoint commit")
	}
	return updated, err
}
