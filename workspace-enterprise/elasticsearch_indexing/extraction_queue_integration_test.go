package elasticsearch_indexing

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/stretchr/testify/require"
)

// Occupy actual native workers and their queue, not a replacement extraction
// implementation. Release always runs before the fixture's server shuts down.
func saturateExtractionQueue(t *testing.T, server *app.Server) func() {
	t.Helper()
	workers := runtime.NumCPU()
	entered := make(chan struct{}, workers)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var completed atomic.Int32
	for range workers {
		require.Eventually(t, func() bool {
			return server.GoExtraction(func() {
				entered <- struct{}{}
				<-release
				completed.Add(1)
			})
		}, 5*time.Second, time.Millisecond)
	}
	for range workers {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("not all native extraction workers reached the blocker")
		}
	}
	for range workers {
		require.True(t, server.GoExtraction(func() {
			<-release
			completed.Add(1)
		}))
	}
	var rejectedTaskRan atomic.Bool
	require.False(t, server.GoExtraction(func() { rejectedTaskRan.Store(true) }), "the native queue must actually be full")
	return func() {
		unblock()
		require.Eventually(t, func() bool { return completed.Load() == int32(2*workers) }, 5*time.Second, time.Millisecond)
		require.False(t, rejectedTaskRan.Load())
		var resumed atomic.Bool
		require.True(t, server.GoExtraction(func() { resumed.Store(true) }))
		require.Eventually(t, resumed.Load, 5*time.Second, time.Millisecond, "new extraction work must run after the queue drains")
	}
}
