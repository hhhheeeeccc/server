package elasticsearch_indexing

import (
	"errors"
	"fmt"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/enterprise/elasticsearch/common"
)

// Reading/retrying belongs to the Enterprise lifecycle; the common worker's
// exported Bulk helpers retain its document serialization and exclusions.
// PostgreSQL reads use the job's independent, interruptible reader. Other
// database backends retain their native store and its cancellation limitations.
func (w *checkpointWorker) indexBatch(p common.IndexingProgress, job *model.Job, stop, canceled <-chan struct{}) (common.IndexingProgress, string, error) {
	interrupted := func() string { return w.batchInterruption(stop, canceled) }
	limit := *w.server.Config().ElasticsearchSettings.BatchSize
	var done *bool
	var cursor *string
	var count *int64
	var size int
	var reachedEnd bool
	var lastAt int64
	var lastID, status string
	var err error
	switch {
	case job.Data["index_posts"] != "false" && !p.DonePosts:
		done, cursor, count = &p.DonePosts, &p.LastPostID, &p.DonePostsCount
		var rows []*model.PostForIndexing
		rows, status, err = readIndexingBatch(w.logger, "posts", interrupted, 15*time.Second, func() ([]*model.PostForIndexing, error) {
			return w.server.Store.Post().GetPostsBatchForIndexing(p.LastEntityTime, p.LastPostID, limit)
		})
		rows, reachedEnd = indexingRowsThrough(rows, p.EndAtTime, func(row *model.PostForIndexing) int64 { return row.CreateAt })
		size = len(rows)
		if err == nil && status == "" && size > 0 {
			last, appErr := w.batcher.BulkIndexPosts(rows, p)
			if appErr != nil {
				err = appErr
			} else {
				lastAt, lastID = last.CreateAt, last.Id
			}
		}
	case job.Data["index_channels"] != "false" && !p.DoneChannels:
		done, cursor, count = &p.DoneChannels, &p.LastChannelID, &p.DoneChannelsCount
		var rows []*model.Channel
		rows, status, err = readIndexingBatch(w.logger, "channels", interrupted, 15*time.Second, func() ([]*model.Channel, error) {
			return w.server.Store.Channel().GetChannelsBatchForIndexing(p.LastEntityTime, p.LastChannelID, limit)
		})
		rows, reachedEnd = indexingRowsThrough(rows, p.EndAtTime, func(row *model.Channel) int64 { return row.CreateAt })
		size = len(rows)
		if err == nil && status == "" && size > 0 {
			last, appErr := common.BulkIndexChannels(w.server.Config(), w.server.Store, w.logger, w.add, rows, p)
			if appErr != nil {
				err = appErr
			} else {
				lastAt, lastID = last.CreateAt, last.Id
			}
		}
	case job.Data["index_users"] != "false" && !p.DoneUsers:
		done, cursor, count = &p.DoneUsers, &p.LastUserID, &p.DoneUsersCount
		var rows []*model.UserForIndexing
		rows, status, err = readIndexingBatch(w.logger, "users", interrupted, 15*time.Second, func() ([]*model.UserForIndexing, error) {
			return w.server.Store.User().GetUsersBatchForIndexing(p.LastEntityTime, p.LastUserID, limit)
		})
		rows, reachedEnd = indexingRowsThrough(rows, p.EndAtTime, func(row *model.UserForIndexing) int64 { return row.CreateAt })
		size = len(rows)
		if err == nil && status == "" && size > 0 {
			last, appErr := w.batcher.BulkIndexUsers(rows, p)
			if appErr != nil {
				err = appErr
			} else {
				lastAt, lastID = last.CreateAt, last.Id
			}
		}
	case job.Data["index_files"] != "false" && !p.DoneFiles:
		done, cursor, count = &p.DoneFiles, &p.LastFileID, &p.DoneFilesCount
		var rows []*model.FileForIndexing
		rows, status, err = readIndexingBatch(w.logger, "files", interrupted, 15*time.Second, func() ([]*model.FileForIndexing, error) {
			return w.server.Store.FileInfo().GetFilesBatchForIndexing(p.LastEntityTime, p.LastFileID, true, limit)
		})
		rows, reachedEnd = indexingRowsThrough(rows, p.EndAtTime, func(row *model.FileForIndexing) int64 { return row.CreateAt })
		size = len(rows)
		if err == nil && status == "" && size > 0 {
			last, appErr := w.batcher.BulkIndexFiles(rows, p)
			if appErr != nil {
				err = appErr
			} else {
				lastAt, lastID = last.CreateAt, last.Id
			}
		}
	default:
		return p, "", errors.New("no indexing entities left to process")
	}
	if state := interrupted(); state != "" {
		return p, state, nil
	}
	if err != nil || status != "" {
		return p, status, err
	}
	if size == 0 {
		*done = true
		p.LastEntityTime = p.StartAtTime
		return p, "", nil
	}
	*cursor = lastID
	*count += int64(size)
	if reachedEnd {
		*done = true
		p.LastEntityTime = p.StartAtTime
	} else {
		p.LastEntityTime = lastAt
	}
	return p, "", nil
}

// Reads are ordered by (CreateAt, Id). The end timestamp is inclusive: an
// entire batch at EndAtTime must advance its ID cursor and read again, since
// more entities can share that timestamp. Never index rows beyond the bound.
func indexingRowsThrough[T any](rows []T, end int64, createdAt func(T) int64) ([]T, bool) {
	for i, row := range rows {
		if createdAt(row) > end {
			return rows[:i], true
		}
	}
	return rows, false
}

func (w *checkpointWorker) batchInterruption(stop, canceled <-chan struct{}) string {
	if cause := indexingWorkCause(w.workContext); cause != nil {
		return cause.status
	}
	// Cancellation wins if both signals are already visible, as it does in the
	// final SQL transition. Reading the license is an in-memory atomic load.
	select {
	case <-canceled:
		return model.JobStatusCanceled
	default:
	}
	select {
	case <-stop:
		return model.JobStatusPending
	default:
	}
	if !w.IsEnabled(w.liveConfig()) || indexingTarget(w.liveConfig()) != w.target {
		return model.JobStatusPending
	}
	return ""
}

func readIndexingBatch[T any](logger mlog.LoggerIFace, entity string, interrupted func() string, delay time.Duration, read func() ([]T, error)) ([]T, string, error) {
	for attempt := 0; ; attempt++ {
		if status := interrupted(); status != "" {
			return nil, status, nil
		}
		rows, err := read()
		if status := interrupted(); status != "" {
			return nil, status, nil
		}
		if err == nil {
			return rows, "", nil
		}
		if attempt >= 10 {
			return nil, "", fmt.Errorf("read %s batch for indexing after 11 attempts: %w", entity, err)
		}
		logger.Warn("Failed to read indexing batch; retrying", mlog.String("entity", entity), mlog.Err(err))
		if status := waitIndexingRetry(interrupted, delay); status != "" {
			return nil, status, nil
		}
	}
}

func waitIndexingRetry(interrupted func() string, delay time.Duration) string {
	deadline := time.NewTimer(delay)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if status := interrupted(); status != "" {
			return status
		}
		select {
		case <-deadline.C:
			return interrupted()
		case <-tick.C:
		}
	}
}
