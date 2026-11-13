package ldap

import (
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestSyncWorkerCanRestart(t *testing.T) {
	w := &syncWorker{
		jobs:    make(chan model.Job),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}

	for range 3 {
		go w.Run()
		require.Eventually(t, func() bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			return w.running
		}, 2*time.Second, time.Millisecond)
		w.Stop()
		w.Stop()
	}
}
