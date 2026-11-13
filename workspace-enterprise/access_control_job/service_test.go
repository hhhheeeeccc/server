package access_control_job

import (
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/stretchr/testify/require"
)

func TestWorkerStopCanBeCalledTwice(t *testing.T) {
	logger, err := mlog.NewLogger()
	require.NoError(t, err)
	worker := &Worker{
		logger:  logger,
		jobs:    make(chan model.Job),
		stop:    make(chan bool, 1),
		stopped: make(chan bool, 1),
	}
	go worker.Run()
	done := make(chan struct{}, 2)
	go func() { worker.Stop(); done <- struct{}{} }()
	go func() { worker.Stop(); done <- struct{}{} }()
	for range 2 {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("repeated Stop must return after the worker exits")
		}
	}
}

func TestWorkerCanRestartAfterStop(t *testing.T) {
	logger, err := mlog.NewLogger()
	require.NoError(t, err)
	worker := &Worker{
		logger:  logger,
		jobs:    make(chan model.Job),
		stop:    make(chan bool, 1),
		stopped: make(chan bool, 1),
	}

	for range 3 {
		go worker.Run()
		require.Eventually(t, func() bool {
			worker.lifecycleMu.Lock()
			defer worker.lifecycleMu.Unlock()
			return worker.running
		}, 2*time.Second, time.Millisecond)
		done := make(chan struct{})
		go func() {
			worker.Stop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("worker did not stop after restart")
		}
	}
}

func TestSplitIDs(t *testing.T) {
	require.Equal(t, []string{"channel-a", "channel-b"}, splitIDs(" channel-a,channel-b,channel-a,, "))
}

func TestSyncEnabled(t *testing.T) {
	cfg := &model.Config{}
	cfg.SetDefaults()
	*cfg.AccessControlSettings.EnableAttributeBasedAccessControl = true
	cfg.FeatureFlags.PermissionPolicies = true
	cfg.FeatureFlags.TeamMembershipAccessControl = true

	require.True(t, syncEnabled(cfg, model.JobTypeAccessControlSync))
	require.True(t, syncEnabled(cfg, model.JobTypeAccessControlTeamSync))

	cfg.FeatureFlags.TeamMembershipAccessControl = false
	require.True(t, syncEnabled(cfg, model.JobTypeAccessControlSync))
	require.False(t, syncEnabled(cfg, model.JobTypeAccessControlTeamSync))

	*cfg.AccessControlSettings.EnableAttributeBasedAccessControl = false
	require.False(t, syncEnabled(cfg, model.JobTypeAccessControlSync))
}

func TestTeamIsStrictUsesOpenInviteState(t *testing.T) {
	require.True(t, teamIsStrict(&model.Team{Type: model.TeamOpen, AllowOpenInvite: false}))
	require.False(t, teamIsStrict(&model.Team{Type: model.TeamInvite, AllowOpenInvite: true}))
}

func TestInterruptionSurvivesNestedLoops(t *testing.T) {
	for _, reason := range []stopReason{jobCanceled, workerStopped} {
		t.Run(map[stopReason]string{jobCanceled: "cancel", workerStopped: "stop"}[reason], func(t *testing.T) {
			worker := &Worker{stop: make(chan bool, 1)}
			canceled := make(chan struct{}, 1)
			if reason == jobCanceled {
				canceled <- struct{}{}
			} else {
				worker.stop <- true
			}
			require.Equal(t, reason, worker.stopRequested(canceled), "member loop observes interruption")
			require.Equal(t, reason, worker.stopRequested(canceled), "policy loop must also observe interruption")
			got, err := worker.execute(nil, nil, canceled)
			require.NoError(t, err)
			require.Equal(t, reason, got, "an interrupted job must not access the store or report success")
		})
	}
}

func TestTeamChannelChainingStopsBeforeCreatingJobs(t *testing.T) {
	for _, reason := range []stopReason{jobCanceled, workerStopped} {
		t.Run(map[stopReason]string{jobCanceled: "cancel", workerStopped: "stop"}[reason], func(t *testing.T) {
			worker := &Worker{
				stop:    make(chan bool, 1),
				results: map[string]*membershipSyncResult{"team-id": {}},
			}
			canceled := make(chan struct{}, 1)
			if reason == jobCanceled {
				canceled <- struct{}{}
			} else {
				worker.stop <- true
			}
			got, err := worker.chainChannelSync(nil, "parent-id", canceled)
			require.NoError(t, err)
			require.Equal(t, reason, got)
		})
	}
}
