package content_extraction

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExtractionWorkerStopBeforeScheduledRun(t *testing.T) {
	// JobServer launches Run asynchronously. Stop must still join that launch
	// when it is called first; otherwise Run can start after shutdown returns.
	// No application, SQL or extraction is used by an idle lifecycle.
	w := New(nil).MakeWorker()
	for range 2 {
		stopping, stopped := make(chan struct{}), make(chan struct{})
		go func() { close(stopping); w.Stop(); close(stopped) }()
		<-stopping
		select {
		case <-stopped:
			t.Fatal("Stop returned before the already scheduled Run was joined")
		case <-time.After(30 * time.Millisecond):
		}
		go w.Run()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("Stop did not join Run after it entered")
		}
	}
	require.Empty(t, w.JobChannel())
}
