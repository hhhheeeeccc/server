package push_proxy

import (
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

func TestWorkerStopBeforeRunAndRestart(t *testing.T) {
	w := &Worker{
		jobs: make(chan model.Job), stop: make(chan struct{}), done: make(chan struct{}),
	}
	w.Stop()
	go w.Run()
	select {
	case <-w.done:
	case <-time.After(time.Second):
		t.Fatal("worker ignored stop requested before Run")
	}

	go w.Run()
	deadline := time.After(time.Second)
	for {
		w.mu.Lock()
		running := w.running
		w.mu.Unlock()
		if running {
			break
		}
		select {
		case <-deadline:
			t.Fatal("worker did not restart")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	w.Stop()
}
