package channel_read_receipts

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReceiptBatchReducesFanoutAndKeepsLatestRead(t *testing.T) {
	var events int
	var result map[string]int64
	b := newReceiptBatcher(context.Background(), func(channelID string, receipts map[string]int64) {
		require.Equal(t, "channel", channelID)
		events++
		result = receipts
	})
	for user := range 150 {
		for _, at := range []int64{10, 30, 20} {
			b.add("channel", fmt.Sprint(user), at)
		}
	}
	b.publishAll(b.drain())
	require.Equal(t, 1, events, "150 readers must not produce 150 channel broadcasts")
	require.Len(t, result, 150)
	for _, at := range result {
		require.EqualValues(t, 30, at)
	}
}

func TestReceiptBatchBoundsMemoryAndPayloadWithoutDroppingReaders(t *testing.T) {
	seen := make(map[string]int64)
	channels := make(map[string]bool)
	b := newReceiptBatcher(context.Background(), func(channelID string, receipts map[string]int64) {
		require.LessOrEqual(t, len(receipts), maxReceiptsPerEvent)
		channels[channelID] = true
		for userID, at := range receipts {
			seen[userID] = at
		}
	})
	for user := range maxPendingReceipts + 150 {
		b.add(fmt.Sprint(user%3), fmt.Sprint(user), int64(user+1))
		require.Less(t, b.count, maxPendingReceipts)
	}
	require.Len(t, seen, maxPendingReceipts, "capacity triggers a bounded flush")
	b.publishAll(b.drain())
	require.Len(t, seen, maxPendingReceipts+150)
	require.Len(t, channels, 3)
}

func TestReceiptBatchConcurrentReaders(t *testing.T) {
	b := newReceiptBatcher(context.Background(), func(string, map[string]int64) {})
	var wg sync.WaitGroup
	for user := range 150 {
		wg.Go(func() {
			for at := range int64(100) {
				b.add("channel", fmt.Sprint(user), at)
			}
		})
	}
	wg.Wait()
	batch := b.drain()
	require.Len(t, batch["channel"], 150)
	for _, at := range batch["channel"] {
		require.EqualValues(t, 99, at)
	}
}

func TestReceiptBatchFlushesWithoutMoreTrafficAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan map[string]int64, 2)
	b := newReceiptBatcher(ctx, func(_ string, receipts map[string]int64) { events <- receipts })
	done := make(chan struct{})
	go func() { b.run(); close(done) }()
	b.add("channel", "reader", 123)
	select {
	case event := <-events:
		require.Equal(t, map[string]int64{"reader": 123}, event)
	case <-time.After(3 * time.Second):
		t.Fatal("last read was not flushed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop with server")
	}
	b.add("channel", "reader", 456)
	require.Empty(t, b.drain())
}

func BenchmarkReceiptBatch150Readers(b *testing.B) {
	batcher := newReceiptBatcher(context.Background(), func(string, map[string]int64) {})
	users := make([]string, 150)
	for i := range users {
		users[i] = fmt.Sprint(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		for _, user := range users {
			batcher.add("channel", user, 1)
		}
		batcher.publishAll(batcher.drain())
	}
}
