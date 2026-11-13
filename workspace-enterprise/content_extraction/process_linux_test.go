package content_extraction_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestRegisteredExtractionRecoversKilledProcess(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%t", canceled), func(t *testing.T) { testExtractionProcessRecovery(t, canceled, false) })
	}
}

func TestRegisteredExtractionResumesTiedFileCursorAfterSIGKILL(t *testing.T) {
	testExtractionProcessRecovery(t, false, true)
}

func testExtractionProcessRecovery(t *testing.T, canceled, withCheckpoint bool) {
	t.Helper()
	th := setup(t)
	require.NoError(t, th.Server.Jobs.StopWorkers())
	info := upload(t, th, "before crash", "crash.txt", time.Now().Add(-time.Minute))
	var completed *model.FileInfo
	if withCheckpoint {
		completed = upload(t, th, "before crash", "completed.txt", time.UnixMilli(info.CreateAt))
		if completed.Id > info.Id {
			completed, info = info, completed
		}
	}
	path := filepath.Join(*th.App.Config().FileSettings.Directory, info.Path)
	require.NoError(t, os.Remove(path))
	require.NoError(t, syscall.Mkfifo(path, 0600))
	writer, err := os.OpenFile(path, os.O_RDWR, 0600)
	require.NoError(t, err)
	var once sync.Once
	release := func() { once.Do(func() { require.NoError(t, writer.Close()) }) }
	t.Cleanup(release)
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, nil)
	require.Nil(t, appErr)
	cfg := th.App.Config().Clone()
	cfg.ServiceSettings.ListenAddress = new("127.0.0.1:0")
	cfg.ServiceSettings.EnableLocalMode = new(false)
	cfg.JobSettings.RunJobs = new(false)
	cfg.JobSettings.RunScheduler = new(false)
	cfg.FileSettings.ExtractContentTimeout = new(0)
	cfg.PluginSettings.Enable = new(false)
	cfg.MetricsSettings.Enable = new(false)
	cfg.LogSettings.EnableFile = new(false)
	cfg.LogSettings.EnableConsole = new(false)
	cfg.LogSettings.EnableDiagnostics = new(false)
	cfg.EmailSettings.SendEmailNotifications = new(false)
	cfg.SqlSettings.MaxOpenConns = new(10)
	cfg.SqlSettings.MaxIdleConns = new(5)
	type child struct {
		cmd                 *exec.Cmd
		done                chan error
		drained             bool
		resultPath, logPath string
	}
	start := func() *child {
		dir := t.TempDir()
		c := &child{done: make(chan error, 1), resultPath: filepath.Join(dir, "result.json"), logPath: filepath.Join(dir, "child.log")}
		data, err := json.Marshal(extractionProcessInput{Config: cfg, JobID: job.Id, ResultPath: c.resultPath})
		require.NoError(t, err)
		inputPath := filepath.Join(dir, "input.json")
		require.NoError(t, os.WriteFile(inputPath, data, 0600))
		log, err := os.OpenFile(c.logPath, os.O_CREATE|os.O_WRONLY, 0600)
		require.NoError(t, err)
		c.cmd = exec.Command(os.Args[0])
		c.cmd.Env = append(os.Environ(), extractionProcessEnv+"="+inputPath)
		c.cmd.Stdout, c.cmd.Stderr = log, log
		require.NoError(t, c.cmd.Start())
		go func() { err := c.cmd.Wait(); _ = log.Close(); c.done <- err }()
		t.Cleanup(func() {
			if !c.drained {
				_ = c.cmd.Process.Kill()
				select {
				case <-c.done:
				case <-time.After(10 * time.Second):
					t.Error("extraction child did not terminate")
				}
			}
			if t.Failed() {
				data, _ := os.ReadFile(c.logPath)
				t.Logf("child log:\n%s", data)
			}
		})
		return c
	}
	first := start()
	require.Eventually(t, func() bool {
		entries, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", first.cmd.Process.Pid))
		for _, entry := range entries {
			if target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", first.cmd.Process.Pid, entry.Name())); err == nil && target == path {
				return true
			}
		}
		return false
	}, 30*time.Second, 20*time.Millisecond, "first child must actually open the native extraction FIFO")
	before := currentJob(t, th, job.Id)
	require.Equal(t, model.JobStatusInProgress, before.Status)
	require.NotEmpty(t, before.Data["workspace_extraction_attempt_v1"])
	if withCheckpoint {
		require.Equal(t, "before crash", get(t, th, completed.Id).Content)
		var checkpoint struct{ ID string }
		require.NoError(t, json.Unmarshal([]byte(before.Data["workspace_extraction_checkpoint_v1"]), &checkpoint))
		require.Equal(t, completed.Id, checkpoint.ID, "the smaller ID at the tied timestamp must already be confirmed")
		require.Equal(t, "1", before.Data["processed"])
	}
	// A second real watcher must leave the first process's live claim alone.
	require.NoError(t, th.Server.Jobs.StartWorkers())
	run(t, th, model.StringMap{"from": fmt.Sprint(time.Now().Add(time.Hour).Unix()), "to": fmt.Sprint(time.Now().Add(2 * time.Hour).Unix())}, model.JobStatusSuccess)
	require.Equal(t, before.StartAt, currentJob(t, th, job.Id).StartAt)
	require.Equal(t, before.Data["workspace_extraction_attempt_v1"], currentJob(t, th, job.Id).Data["workspace_extraction_attempt_v1"])
	require.NoError(t, th.Server.Jobs.StopWorkers())
	if canceled {
		require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
	}
	require.NoError(t, first.cmd.Process.Kill())
	select {
	case err := <-first.done:
		first.drained = true
		require.Error(t, err, "SIGKILL must bypass deferred cleanup")
	case <-time.After(10 * time.Second):
		t.Fatal("killed extraction process did not exit")
	}
	release()
	if withCheckpoint {
		require.Nil(t, th.App.RemoveFile(completed.Path))
	}
	require.NoError(t, os.Remove(path))
	_, err = th.Server.FileBackend().WriteFile(strings.NewReader("after crash recovery"), info.Path)
	require.NoError(t, err)
	second := start()
	select {
	case err := <-second.done:
		second.drained = true
		require.NoError(t, err)
	case <-time.After(45 * time.Second):
		t.Fatal("successor did not complete the interrupted extraction job")
	}
	data, err := os.ReadFile(second.resultPath)
	require.NoError(t, err)
	var result extractionProcessResult
	require.NoError(t, json.Unmarshal(data, &result))
	require.Equal(t, second.cmd.Process.Pid, result.PID)
	require.NotEqual(t, first.cmd.Process.Pid, result.PID)
	final := currentJob(t, th, job.Id)
	want := model.JobStatusSuccess
	if canceled {
		want = model.JobStatusCanceled
	}
	require.Equal(t, want, result.Status)
	require.Equal(t, want, final.Status)
	if canceled {
		require.Equal(t, before.Data["workspace_extraction_attempt_v1"], final.Data["workspace_extraction_attempt_v1"])
		require.Empty(t, get(t, th, info.Id).Content)
	} else {
		require.NotEqual(t, before.Data["workspace_extraction_attempt_v1"], final.Data["workspace_extraction_attempt_v1"])
		require.Equal(t, "after crash recovery", get(t, th, info.Id).Content)
		if withCheckpoint {
			require.Equal(t, "before crash", get(t, th, completed.Id).Content)
			require.Equal(t, "2", final.Data["processed"])
			require.Equal(t, "0", final.Data["errors"])
		}
	}
	t.Logf("same extraction Job %s after SIGKILL: first_pid=%d successor_pid=%d status=%s", job.Id, first.cmd.Process.Pid, result.PID, result.Status)
}
