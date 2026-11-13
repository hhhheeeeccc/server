package compliance_service

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestDailyExportResumesSameReportAfterSIGKILLDuringRead(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	end := time.Date(2074, 1, 2, 0, 0, 0, 0, time.UTC)
	db := th.Store.GetInternalMasterDB()
	job, _, err := findOrCreateDailyJob(context.Background(), db, &model.Compliance{Type: model.ComplianceTypeDaily,
		Desc: "2074-01-01", StartAt: end.Add(-24 * time.Hour).UnixMilli(), EndAt: end.UnixMilli()})
	require.NoError(t, err)
	_, err = th.Store.Post().Save(th.Context, &model.Post{ChannelId: th.BasicChannel.Id,
		UserId: th.BasicUser.Id, CreateAt: job.StartAt, Message: "تقرير بعد سقوط عملية التصدير"})
	require.NoError(t, err)
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
	directory := t.TempDir()
	input := dailyScheduleProcessInput{Config: cfg, EndAt: end.UnixMilli(), Mode: "export",
		ReadyPath: filepath.Join(directory, "ready"), StartPath: filepath.Join(directory, "start"),
		ResultPath: filepath.Join(directory, "result.json")}
	data, err := json.Marshal(input)
	require.NoError(t, err)
	path := filepath.Join(directory, "input.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	log, err := os.CreateTemp(directory, "export-child-*.log")
	require.NoError(t, err)
	defer log.Close()
	command := exec.Command(os.Args[0])
	command.Env = append(os.Environ(), dailyScheduleProcessEnv+"="+path)
	command.Stdout, command.Stderr = log, log
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
				t.Error("export process stayed live during test cleanup")
			}
		}
	}()
	require.Eventually(t, func() bool { _, err := os.Stat(input.ReadyPath); return err == nil }, 10*time.Second, 10*time.Millisecond)
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
	require.Len(t, abandoned, 1, "the child must have entered the actual ZIP export")
	// A second exporter must skip while the real child owns the report files.
	require.NoError(t, service.runDailyInterval(context.Background(), end))
	stored, err := th.Store.Compliance().Get(job.Id)
	require.NoError(t, err)
	require.Equal(t, model.ComplianceStatusRunning, stored.Status)
	require.NoError(t, command.Process.Kill())
	select {
	case err := <-done:
		drained = true
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("export child did not stop after SIGKILL")
	}
	require.NoError(t, blocker.Rollback())
	require.Eventually(t, func() bool {
		if service.runDailyInterval(context.Background(), end) != nil {
			return false
		}
		stored, err := th.Store.Compliance().Get(job.Id)
		return err == nil && stored.Status == model.ComplianceStatusFinished && stored.Count == 1
	}, 5*time.Second, 20*time.Millisecond)
	records := readReportCSV(t, th, job)
	require.Len(t, records, 2)
	require.Contains(t, records[1], "تقرير بعد سقوط عملية التصدير")
	remaining, err := filepath.Glob(pattern)
	require.NoError(t, err)
	require.Empty(t, remaining, "the confirmed successor must remove the previous process's abandoned temporary ZIP")
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM Compliances WHERE Type=$1 AND EndAt=$2`, model.ComplianceTypeDaily, end.UnixMilli()).Scan(&count))
	require.Equal(t, 1, count)
	t.Logf("daily export after SIGKILL: child_pid=%d successor_pid=%d report=%s", command.Process.Pid, os.Getpid(), job.Id)
}

func TestDailyTemporaryCleanupPreservesUnrelatedFiles(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	job := savedReadReport(t, th)
	job.Type = model.ComplianceTypeDaily
	e, acquired, err := service.acquireDailyExecution(context.Background())
	require.NoError(t, err)
	require.True(t, acquired)
	defer e.Close()
	old := filepath.Join(e.directory, job.JobName()+"-abandoned.tmp")
	keep := []string{
		filepath.Join(e.directory, "another-report-abandoned.tmp"),
		filepath.Join(e.directory, job.JobName()+".zip"),
		filepath.Join(e.directory, job.JobName()+"-notes.txt"),
	}
	require.NoError(t, os.WriteFile(old, []byte("incomplete"), 0o600))
	for _, path := range keep {
		require.NoError(t, os.WriteFile(path, []byte("preserved"), 0o600))
	}
	link := filepath.Join(e.directory, job.JobName()+"-linked.tmp")
	require.NoError(t, os.Symlink(keep[0], link))
	directory := filepath.Join(e.directory, job.JobName()+"-directory.tmp")
	require.NoError(t, os.Mkdir(directory, 0o700))
	require.NoError(t, e.cleanTemporary(job))
	_, err = os.Stat(old)
	require.True(t, os.IsNotExist(err))
	for _, path := range keep {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "preserved", string(data))
	}
	info, err := os.Lstat(link)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
	info, err = os.Stat(directory)
	require.NoError(t, err)
	require.True(t, info.IsDir())
}
