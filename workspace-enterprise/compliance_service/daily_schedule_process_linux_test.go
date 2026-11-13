package compliance_service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestDailyScheduleResumesAfterSIGKILLInsideTransaction(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	end := time.Date(2068, 1, 1, 0, 0, 0, 0, time.UTC)
	cutoff := end.Add(72 * time.Hour)
	require.NoError(t, service.prepareDailyIntervals(context.Background(), end))
	db := th.Store.GetInternalMasterDB()
	// Pause the second INSERT after the first one has succeeded in the same
	// real transaction. No production callback or mock replaces its SQL.
	_, err := db.Exec(fmt.Sprintf(`CREATE FUNCTION workspace_test_daily_schedule_pause() RETURNS trigger AS $$
        BEGIN IF NEW.EndAt=%d THEN PERFORM pg_advisory_xact_lock(11283544,99); END IF;
        RETURN NEW; END $$ LANGUAGE plpgsql`, end.Add(48*time.Hour).UnixMilli()))
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER workspace_test_daily_schedule_pause BEFORE INSERT ON Compliances
        FOR EACH ROW EXECUTE FUNCTION workspace_test_daily_schedule_pause()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS workspace_test_daily_schedule_pause ON Compliances`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS workspace_test_daily_schedule_pause()`)
	})
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `SELECT pg_advisory_lock(11283544,99)`)
	require.NoError(t, err)
	var once sync.Once
	unlock := func() {
		once.Do(func() {
			_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(11283544,99)`)
			_ = conn.Close()
		})
	}
	t.Cleanup(unlock)
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
	type child struct {
		cmd                 *exec.Cmd
		done                chan error
		drained             bool
		resultPath, logPath string
	}
	start := func() *child {
		dir := t.TempDir()
		c := &child{done: make(chan error, 1), resultPath: filepath.Join(dir, "result.json"), logPath: filepath.Join(dir, "child.log")}
		data, err := json.Marshal(dailyScheduleProcessInput{Config: cfg, EndAt: cutoff.UnixMilli(), ResultPath: c.resultPath})
		require.NoError(t, err)
		input := filepath.Join(dir, "input.json")
		require.NoError(t, os.WriteFile(input, data, 0o600))
		log, err := os.OpenFile(c.logPath, os.O_CREATE|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		c.cmd = exec.Command(os.Args[0])
		c.cmd.Env = append(os.Environ(), dailyScheduleProcessEnv+"="+input)
		c.cmd.Stdout, c.cmd.Stderr = log, log
		require.NoError(t, c.cmd.Start())
		go func() { err := c.cmd.Wait(); _ = log.Close(); c.done <- err }()
		t.Cleanup(func() {
			if !c.drained {
				_ = c.cmd.Process.Kill()
				unlock()
				select {
				case <-c.done:
				case <-time.After(10 * time.Second):
					t.Error("compliance scheduling child did not stop")
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
		var waiting int
		return db.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype='advisory'
            AND classid=11283544 AND objid=99 AND NOT granted`).Scan(&waiting) == nil && waiting == 1
	}, 20*time.Second, 20*time.Millisecond, "the child must reach its second real daily INSERT")
	require.Equal(t, end.UnixMilli(), dailyThrough(t, th))
	require.Equal(t, 1, dailyCount(t, th, end, cutoff), "the first INSERT must remain uncommitted")
	require.NoError(t, first.cmd.Process.Kill())
	select {
	case err := <-first.done:
		first.drained = true
		require.Error(t, err, "SIGKILL must bypass deferred rollback")
	case <-time.After(10 * time.Second):
		t.Fatal("killed scheduling process did not exit")
	}
	unlock()
	// Let PostgreSQL observe the disconnected client after its trigger resumes.
	// Acquiring the creation lock proves the killed transaction has released it.
	require.Eventually(t, func() bool {
		check, err := db.Conn(context.Background())
		if err != nil {
			return false
		}
		defer check.Close()
		var acquired bool
		if err := check.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock(11283544,2)`).Scan(&acquired); err != nil || !acquired {
			return false
		}
		_, err = check.ExecContext(context.Background(), `SELECT pg_advisory_unlock(11283544,2)`)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, end.UnixMilli(), dailyThrough(t, th))
	require.Equal(t, 1, dailyCount(t, th, end, cutoff), "the killed transaction must leave no partial daily prefix")
	second := start()
	select {
	case err := <-second.done:
		second.drained = true
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("successor process did not prepare the missed daily reports")
	}
	data, err := os.ReadFile(second.resultPath)
	require.NoError(t, err)
	var result dailyScheduleProcessResult
	require.NoError(t, json.Unmarshal(data, &result))
	require.Equal(t, second.cmd.Process.Pid, result.PID)
	require.NotEqual(t, first.cmd.Process.Pid, result.PID)
	require.Equal(t, cutoff.UnixMilli(), result.Through)
	require.Equal(t, result.Through, dailyThrough(t, th))
	require.Equal(t, 4, dailyCount(t, th, end, cutoff))
	t.Logf("daily scheduling after SIGKILL: first_pid=%d successor_pid=%d through=%d", first.cmd.Process.Pid, result.PID, result.Through)
}
