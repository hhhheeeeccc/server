package message_export

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/mattermost/enterprise/message_export/shared"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestMessageExportProcessCrashIsRecoveredByActualWatcher(t *testing.T) {
	dir := t.TempDir()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.FileSettings.Directory = model.NewPointer(dir)
		cfg.MessageExportSettings.EnableExport = model.NewPointer(true)
		cfg.MessageExportSettings.DownloadExportResults = model.NewPointer(true)
		cfg.MessageExportSettings.ExportFormat = model.NewPointer(model.ComplianceExportTypeCsv)
		cfg.MessageExportSettings.BatchSize = model.NewPointer(1)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
	start := time.Date(2075, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	_, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: th.BasicChannel.Id, UserId: th.BasicUser.Id,
		CreateAt: start, UpdateAt: start, Message: "تصدير الرسائل بعد سقوط العملية"})
	require.NoError(t, err)
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeMessageExport, model.StringMap{
		shared.JobDataBatchStartTime: strconv.FormatInt(start-1, 10), shared.JobDataJobEndTime: strconv.FormatInt(start+1000, 10),
	})
	require.Nil(t, appErr)
	cfg := th.App.Config().Clone()
	cfg.ServiceSettings.ListenAddress = model.NewPointer("127.0.0.1:0")
	cfg.ServiceSettings.EnableLocalMode = model.NewPointer(false)
	cfg.JobSettings.RunJobs = model.NewPointer(false)
	cfg.JobSettings.RunScheduler = model.NewPointer(false)
	cfg.PluginSettings.Enable = model.NewPointer(false)
	cfg.MetricsSettings.Enable = model.NewPointer(false)
	cfg.LogSettings.EnableFile = model.NewPointer(false)
	cfg.LogSettings.EnableConsole = model.NewPointer(false)
	cfg.LogSettings.EnableDiagnostics = model.NewPointer(false)
	cfg.EmailSettings.SendEmailNotifications = model.NewPointer(false)
	cfg.SqlSettings.MaxOpenConns = model.NewPointer(8)
	cfg.SqlSettings.MaxIdleConns = model.NewPointer(4)
	cfg.SqlSettings.QueryTimeout = model.NewPointer(5)
	startChild := func() (*exec.Cmd, <-chan error, messageExportProcessInput) {
		t.Helper()
		childDir := t.TempDir()
		input := messageExportProcessInput{Config: cfg, JobID: job.Id,
			ReadyPath: filepath.Join(childDir, "ready"), StartPath: filepath.Join(childDir, "start"), ResultPath: filepath.Join(childDir, "result.json")}
		data, err := json.Marshal(input)
		require.NoError(t, err)
		inputPath := filepath.Join(childDir, "input.json")
		require.NoError(t, os.WriteFile(inputPath, data, 0o600))
		log, err := os.Create(filepath.Join(childDir, "child.log"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = log.Close() })
		command := exec.Command(os.Args[0])
		command.Env = append(os.Environ(), messageExportProcessEnv+"="+inputPath)
		command.Stdout, command.Stderr = log, log
		require.NoError(t, command.Start())
		done := make(chan error, 1)
		go func() { done <- command.Wait(); close(done) }()
		t.Cleanup(func() {
			_ = command.Process.Kill()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("message-export child stayed live during cleanup")
			}
		})
		require.Eventually(t, func() bool { _, err := os.Stat(input.ReadyPath); return err == nil }, 10*time.Second, 10*time.Millisecond)
		return command, done, input
	}
	old, oldDone, input := startChild()
	db := th.Store.GetInternalMasterDB()
	blocker, err := db.Begin()
	require.NoError(t, err)
	defer blocker.Rollback()
	_, err = blocker.Exec(`LOCK TABLE Posts IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(input.StartPath, []byte("start"), 0o600))
	require.Eventually(t, func() bool {
		var waiting bool
		return db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
            AND wait_event_type='Lock' AND query ILIKE '%Posts%')`).Scan(&waiting) == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	claimed, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusInProgress, claimed.Status)
	require.NoError(t, old.Process.Kill())
	select {
	case err := <-oldDone:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("message exporter survived SIGKILL")
	}
	require.NoError(t, blocker.Rollback())
	successor, successorDone, restart := startChild()
	require.NoError(t, os.WriteFile(restart.StartPath, []byte("start"), 0o600))
	select {
	case err := <-successorDone:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("replacement message-export process did not exit")
	}
	data, err := os.ReadFile(restart.ResultPath)
	require.NoError(t, err)
	var result messageExportProcessResult
	require.NoError(t, json.Unmarshal(data, &result))
	t.Logf("message export after SIGKILL: old_pid=%d successor_pid=%d status=%s error=%s", old.Process.Pid, result.PID, result.Status, result.Error)
	require.Empty(t, result.Error, "the actual watcher must redispatch the same crashed message-export job")
	require.Equal(t, model.JobStatusSuccess, result.Status)
	require.Equal(t, successor.Process.Pid, result.PID)
	require.NotEqual(t, old.Process.Pid, result.PID)
	stored, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, "1", stored.Data[shared.JobDataMessagesExported])
	require.Equal(t, claimed.Data[shared.JobDataJobEndTime], stored.Data[shared.JobDataJobEndTime])
	archive, _, err := th.SystemAdminClient.DownloadJob(context.Background(), job.Id)
	require.NoError(t, err)
	require.True(t, archiveContainsCSVMessage(t, archive, "تصدير الرسائل بعد سقوط العملية"))
}
