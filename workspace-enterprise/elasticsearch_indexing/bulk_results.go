package elasticsearch_indexing

import (
	"sync/atomic"

	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

type bulkResults struct {
	added          atomic.Int64
	completed      atomic.Int64
	failed         atomic.Int64
	processorError atomic.Bool
	// Only the worker goroutine changes activeBatch; Add captures it before
	// handing callbacks to the asynchronous bulk processor.
	activeBatch *bulkBatchResults
}

type bulkBatchResults struct {
	added, completed, failed atomic.Int64
}

func (r *bulkResults) beginBatch() *bulkBatchResults {
	r.activeBatch = &bulkBatchResults{}
	return r.activeBatch
}

func (r *bulkResults) addItem() func(bool) {
	batch := r.activeBatch
	r.added.Add(1)
	if batch != nil {
		batch.added.Add(1)
	}
	return func(failed bool) {
		// Publish completion last: a reader must never mistake a failed write
		// for a successful acknowledgement while these atomics are updating.
		if failed {
			r.failed.Add(1)
			if batch != nil {
				batch.failed.Add(1)
			}
		}
		r.completed.Add(1)
		if batch != nil {
			batch.completed.Add(1)
		}
	}
}

// Called after IndexBatch returns, when this batch cannot receive more items.
func (b *bulkBatchResults) confirmed() bool {
	return b.completed.Load() == b.added.Load() && b.failed.Load() == 0
}

// failureCount is called only after Close has waited for all callbacks. The
// shared worker discards Close's error, so processor failures also need a count.
func (r *bulkResults) failureCount(closeErr error, logger mlog.LoggerIFace, backend string) int64 {
	failed := r.failed.Load()
	if unconfirmed := r.added.Load() - r.completed.Load(); unconfirmed > 0 {
		failed += unconfirmed
		logger.Warn(backend+" bulk response did not acknowledge all documents", mlog.Int("num_unconfirmed", unconfirmed))
	}
	if failed == 0 && (closeErr != nil || r.processorError.Load()) {
		failed = 1
	}
	return failed
}
