package channel_read_receipts

import (
	"context"
	"sync"
	"time"
)

const (
	receiptBatchInterval = 500 * time.Millisecond
	maxPendingReceipts   = 4096
	maxReceiptsPerEvent  = 200
)

// One worker per server, no polling while idle and no goroutine per receipt.
// Only transient notifications are buffered; read state is already persisted by
// the authenticated endpoint before add is called.
type receiptBatcher struct {
	ctx     context.Context
	publish func(string, map[string]int64)
	notify  chan struct{}
	mu      sync.Mutex
	pending map[string]map[string]int64
	count   int
}

func newReceiptBatcher(ctx context.Context, publish func(string, map[string]int64)) *receiptBatcher {
	return &receiptBatcher{ctx: ctx, publish: publish, notify: make(chan struct{}, 1)}
}

func (b *receiptBatcher) add(channelID, userID string, viewedAt int64) {
	if b.ctx.Err() != nil {
		return
	}
	var full map[string]map[string]int64
	b.mu.Lock()
	if b.ctx.Err() != nil {
		b.mu.Unlock()
		return
	}
	if b.pending == nil {
		b.pending = make(map[string]map[string]int64)
	}
	receipts := b.pending[channelID]
	if receipts == nil {
		receipts = make(map[string]int64)
		b.pending[channelID] = receipts
	}
	previous, exists := receipts[userID]
	if !exists {
		b.count++
	}
	if !exists || viewedAt > previous {
		receipts[userID] = viewedAt
	}
	if b.count >= maxPendingReceipts {
		full = b.pending
		b.pending, b.count = nil, 0
	}
	b.mu.Unlock()
	// Bound retained memory without dropping readers or starting extra workers.
	// Publishing at capacity provides backpressure to the producing request.
	b.publishAll(full)
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

func (b *receiptBatcher) drain() map[string]map[string]int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	pending := b.pending
	b.pending, b.count = nil, 0
	return pending
}

func (b *receiptBatcher) run() {
	defer b.drain()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-b.notify:
		}
		timer := time.NewTimer(receiptBatchInterval)
		select {
		case <-b.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			b.publishAll(b.drain())
		}
	}
}

func (b *receiptBatcher) publishAll(channels map[string]map[string]int64) {
	for channelID, receipts := range channels {
		batch := make(map[string]int64, min(len(receipts), maxReceiptsPerEvent))
		for userID, viewedAt := range receipts {
			batch[userID] = viewedAt
			if len(batch) == maxReceiptsPerEvent {
				if b.ctx.Err() != nil {
					return
				}
				b.publish(channelID, batch)
				batch = make(map[string]int64, min(len(receipts), maxReceiptsPerEvent))
			}
		}
		if len(batch) > 0 && b.ctx.Err() == nil {
			b.publish(channelID, batch)
		}
	}
}
