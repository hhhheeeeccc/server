package compliance_service

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

// Kill a real App during its ZIP export, then use the public startup hook in
// a second independent process with daily scheduling explicitly disabled.
func TestAdhocExportRecoversAfterSIGKILLWithDailyDisabled(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	job := savedReadReport(t, th)
	_, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: th.BasicChannel.Id,
		UserId: th.BasicUser.Id, CreateAt: job.StartAt, Message: "تقرير مخصص بعد سقوط العملية"})
	require.NoError(t, err)
	cfg := th.App.Config().Clone()
	cfg.ComplianceSettings.EnableDaily = model.NewPointer(false)
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
	startChild := func(mode string) (*exec.Cmd, <-chan error, dailyScheduleProcessInput) {
		t.Helper()
		dir := t.TempDir()
		input := dailyScheduleProcessInput{Config: cfg, Mode: mode, ReportID: job.Id,
			ReadyPath: filepath.Join(dir, "ready"), StartPath: filepath.Join(dir, "start"), ResultPath: filepath.Join(dir, "result.json")}
		data, err := json.Marshal(input)
		require.NoError(t, err)
		path := filepath.Join(dir, "input.json")
		require.NoError(t, os.WriteFile(path, data, 0o600))
		log, err := os.Create(filepath.Join(dir, "child.log"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = log.Close() })
		command := exec.Command(os.Args[0])
		command.Env = append(os.Environ(), dailyScheduleProcessEnv+"="+path)
		command.Stdout, command.Stderr = log, log
		require.NoError(t, command.Start())
		done := make(chan error, 1)
		go func() { done <- command.Wait(); close(done) }()
		t.Cleanup(func() {
			_ = command.Process.Kill()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("adhoc exporter child stayed live during cleanup")
			}
		})
		require.Eventually(t, func() bool { _, err := os.Stat(input.ReadyPath); return err == nil }, 10*time.Second, 10*time.Millisecond)
		return command, done, input
	}
	old, oldDone, input := startChild("adhoc-export")
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
            AND wait_event_type='Lock' AND query ILIKE '%TeamName%' AND query ILIKE '%Posts%')`).Scan(&waiting) == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	pattern := filepath.Join(*cfg.ComplianceSettings.Directory, "compliance", job.JobName()+"-*.tmp")
	abandoned, err := filepath.Glob(pattern)
	require.NoError(t, err)
	require.Len(t, abandoned, 1)
	// A second caller must skip while the child owns this exact report.
	require.Nil(t, service.RunComplianceJob(request.EmptyContext(th.App.Log()), job.DeepCopy()))
	stored, err := th.Store.Compliance().Get(job.Id)
	require.NoError(t, err)
	require.Equal(t, model.ComplianceStatusRunning, stored.Status)
	require.NoError(t, old.Process.Kill())
	select {
	case err := <-oldDone:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("original adhoc exporter survived SIGKILL")
	}
	require.NoError(t, blocker.Rollback())
	successor, successorDone, restart := startChild("adhoc-recover")
	require.NoError(t, os.WriteFile(restart.StartPath, []byte("start"), 0o600))
	select {
	case err := <-successorDone:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("independent restart did not recover the adhoc report")
	}
	data, err := os.ReadFile(restart.ResultPath)
	require.NoError(t, err)
	var result dailyScheduleProcessResult
	require.NoError(t, json.Unmarshal(data, &result))
	require.Equal(t, model.ComplianceStatusFinished, result.Status)
	require.Equal(t, 1, result.Count)
	require.Equal(t, successor.Process.Pid, result.PID)
	require.NotEqual(t, old.Process.Pid, result.PID)
	records := readReportCSV(t, th, job)
	require.Len(t, records, 2)
	require.Contains(t, records[1], "تقرير مخصص بعد سقوط العملية")
	remaining, err := filepath.Glob(pattern)
	require.NoError(t, err)
	require.Empty(t, remaining, "restart must remove the killed process's abandoned ZIP")
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM Compliances WHERE Id=$1`, job.Id).Scan(&count))
	require.Equal(t, 1, count)
	t.Logf("adhoc export after SIGKILL: old_pid=%d successor_pid=%d report=%s", old.Process.Pid, result.PID, job.Id)
}
