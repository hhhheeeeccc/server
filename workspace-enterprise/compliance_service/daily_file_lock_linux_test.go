package compliance_service

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDailyFileLockSurvivesDatabaseIndependentlyAndReleasesAfterSIGKILL(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	directory := filepath.Join(*th.App.Config().ComplianceSettings.Directory, "compliance")
	require.NoError(t, os.MkdirAll(directory, 0o700))
	path := filepath.Join(directory, ".daily-export.lock")
	command := exec.Command(os.Args[0])
	command.Env = append(os.Environ(), dailyFileLockProcessEnv+"="+path)
	input, err := command.StdinPipe()
	require.NoError(t, err)
	defer input.Close()
	output, err := command.StdoutPipe()
	require.NoError(t, err)
	defer output.Close()
	log, err := os.CreateTemp(t.TempDir(), "file-lock-child-*.log")
	require.NoError(t, err)
	defer log.Close()
	command.Stderr = log
	require.NoError(t, command.Start())
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	drained := false
	defer func() {
		_ = command.Process.Kill()
		if !drained {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("child process remained live after test cleanup")
			}
		}
	}()
	var child struct {
		PID    int
		Locked bool
	}
	decoded := make(chan error, 1)
	go func() { decoded <- json.NewDecoder(output).Decode(&child) }()
	select {
	case err := <-decoded:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("child did not acquire the shared file lock")
	}
	require.True(t, child.Locked)
	require.NotEqual(t, os.Getpid(), child.PID)
	before, err := os.Stat(path)
	require.NoError(t, err)
	// The child holds no database connection. Its OS lock must independently
	// prevent another exporter from publishing while the old process is alive.
	e, acquired, err := service.acquireDailyExecution(context.Background())
	require.NoError(t, err)
	require.False(t, acquired)
	require.Nil(t, e)
	require.NoError(t, command.Process.Kill())
	select {
	case err := <-done:
		drained = true
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("SIGKILL did not stop the file-lock child")
	}
	e, acquired, err = service.acquireDailyExecution(context.Background())
	require.NoError(t, err)
	require.True(t, acquired)
	defer e.Close()
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "the lock inode must never be unlinked and replaced")
	t.Logf("daily file lock after SIGKILL: child_pid=%d successor_pid=%d", child.PID, os.Getpid())
}
