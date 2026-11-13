package compliance_service

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

// Exercise the real scheduler with a durable last-prepared day three days
// behind UTC midnight. This models a previously active installation whose
// schedulers were all offline, rather than an initially empty installation.
func TestDailySchedulerCreatesMissedIntervals(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ComplianceSettings.Enable = model.NewPointer(true)
		cfg.ComplianceSettings.EnableDaily = model.NewPointer(true)
		cfg.ComplianceSettings.Directory = model.NewPointer(t.TempDir())
	}).InitBasic(t)
	license := model.NewTestLicense()
	license.Features.Compliance = model.NewPointer(true)
	require.True(t, th.Server.SetLicense(license))
	end := time.Now().UTC().Truncate(24 * time.Hour)
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec(`INSERT INTO Systems (Name,Value) VALUES ($1,$2)`,
		"WorkspaceComplianceDailyThroughV1", strconv.FormatInt(end.Add(-72*time.Hour).UnixMilli(), 10))
	require.NoError(t, err)
	service := &Service{app: th.App}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.runDailyScheduler(ctx, 10*time.Millisecond) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daily scheduler did not stop")
		}
		_, _ = db.Exec(`DELETE FROM Systems WHERE Name=$1`, "WorkspaceComplianceDailyThroughV1")
	})
	var count int
	defer func() { t.Logf("scheduled daily intervals in the three-day window: %d", count) }()
	require.Eventually(t, func() bool {
		err := db.QueryRow(`SELECT count(*) FROM Compliances WHERE Type=$1 AND StartAt>=$2 AND EndAt<=$3`,
			model.ComplianceTypeDaily, end.Add(-72*time.Hour).UnixMilli(), end.UnixMilli()).Scan(&count)
		return err == nil && count == 3
	}, 2*time.Second, 10*time.Millisecond, "daily reports for offline days were never scheduled")
}

func dailyScheduleFixture(t *testing.T) (*api4.TestHelper, *Service) {
	t.Helper()
	directory := t.TempDir()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ComplianceSettings.Enable = model.NewPointer(true)
		cfg.ComplianceSettings.EnableDaily = model.NewPointer(true)
		cfg.ComplianceSettings.Directory = model.NewPointer(directory)
	}).InitBasic(t)
	license := model.NewTestLicense()
	license.Features.Compliance = model.NewPointer(true)
	require.True(t, th.Server.SetLicense(license))
	db := th.Store.GetInternalMasterDB()
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM Systems WHERE Name=$1`, dailyScheduleKey) })
	return th, &Service{app: th.App}
}

func dailyThrough(t *testing.T, th *api4.TestHelper) int64 {
	t.Helper()
	var raw string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT Value FROM Systems WHERE Name=$1`, dailyScheduleKey).Scan(&raw))
	through, err := strconv.ParseInt(raw, 10, 64)
	require.NoError(t, err)
	return through
}

func dailyCount(t *testing.T, th *api4.TestHelper, from, to time.Time) int {
	t.Helper()
	var count int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM Compliances
        WHERE Type=$1 AND EndAt>=$2 AND EndAt<=$3`, model.ComplianceTypeDaily, from.UnixMilli(), to.UnixMilli()).Scan(&count))
	return count
}

func TestDailyScheduleRestartsAndExportsMissingDays(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	end := time.Date(2060, 3, 7, 0, 0, 0, 0, time.UTC)
	require.NoError(t, service.prepareDailyIntervals(context.Background(), end))
	require.Equal(t, 1, dailyCount(t, th, end.Add(-100*24*time.Hour), end))
	require.Equal(t, end.UnixMilli(), dailyThrough(t, th), "first activation must not invent older daily obligations")
	for day := range 4 {
		_, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: th.BasicChannel.Id,
			UserId: th.BasicUser.Id, Message: fmt.Sprintf("استدراك اليوم %d", day),
			CreateAt: end.Add(time.Duration(day)*24*time.Hour - 12*time.Hour).UnixMilli()})
		require.NoError(t, err)
	}
	// No scheduling state survives in the Service; a fresh instance must resume
	// from PostgreSQL and prepare the three days spent offline.
	service = &Service{app: th.App}
	cutoff := end.Add(72 * time.Hour)
	var retried int64
	for range 4 {
		attempted, err := service.runDailyCycle(context.Background(), cutoff, retried)
		require.NoError(t, err)
		if attempted != 0 {
			retried = attempted
		}
	}
	require.Equal(t, cutoff.UnixMilli(), dailyThrough(t, th))
	require.Equal(t, 4, dailyCount(t, th, end, cutoff))
	for day := range 4 {
		var job model.Compliance
		intervalEnd := end.Add(time.Duration(day) * 24 * time.Hour)
		require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT Id,Status,"desc",Count FROM Compliances
            WHERE Type=$1 AND EndAt=$2`, model.ComplianceTypeDaily, intervalEnd.UnixMilli()).Scan(&job.Id, &job.Status, &job.Desc, &job.Count))
		require.Equal(t, model.ComplianceStatusFinished, job.Status)
		require.Equal(t, 1, job.Count)
		job.Type = model.ComplianceTypeDaily
		archive, err := zip.OpenReader(filepath.Join(*th.App.Config().ComplianceSettings.Directory, "compliance", job.JobName()+".zip"))
		require.NoError(t, err)
		require.Len(t, archive.File, 1)
		reader, err := archive.File[0].Open()
		require.NoError(t, err)
		records, err := csv.NewReader(reader).ReadAll()
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		require.NoError(t, archive.Close())
		require.Len(t, records, 2)
		require.Contains(t, records[1], fmt.Sprintf("استدراك اليوم %d", day))
	}
}

func TestDailyScheduleLimitsEachTransactionAndUsesUTC(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	end := time.Date(2061, 3, 7, 0, 0, 0, 0, time.UTC)
	require.NoError(t, service.prepareDailyIntervals(context.Background(), end))
	cutoff := end.Add(65 * 24 * time.Hour).In(time.FixedZone("UTC+14", 14*60*60))
	for _, offset := range []int{31, 62, 65} {
		require.NoError(t, service.prepareDailyIntervals(context.Background(), cutoff))
		require.Equal(t, end.Add(time.Duration(offset)*24*time.Hour).UnixMilli(), dailyThrough(t, th))
		require.Equal(t, offset+1, dailyCount(t, th, end, cutoff))
	}
	var invalid int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM Compliances
        WHERE Type=$1 AND EndAt BETWEEN $2 AND $3
        AND (EndAt-StartAt<>$4 OR StartAt%$4<>0 OR "desc"<>to_char(to_timestamp(StartAt/1000) AT TIME ZONE 'UTC','YYYY-MM-DD'))`,
		model.ComplianceTypeDaily, end.UnixMilli(), cutoff.UnixMilli(), dailyMillis).Scan(&invalid))
	require.Zero(t, invalid, "DST and the server timezone must not alter UTC report boundaries")
}

func TestDailyScheduleConcurrentPreparersDoNotDuplicateReports(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	end := time.Date(2062, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, service.prepareDailyIntervals(context.Background(), end))
	cutoff := end.Add(65 * 24 * time.Hour)
	const callers = 8
	errors := make(chan error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			<-start
			errors <- (&Service{app: th.App}).prepareDailyIntervals(context.Background(), cutoff)
		})
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.Equal(t, cutoff.UnixMilli(), dailyThrough(t, th))
	require.Equal(t, 66, dailyCount(t, th, end, cutoff))
	var duplicates int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM (
        SELECT StartAt,EndAt FROM Compliances WHERE Type=$1 AND EndAt BETWEEN $2 AND $3
        GROUP BY StartAt,EndAt HAVING count(*)<>1) d`, model.ComplianceTypeDaily, end.UnixMilli(), cutoff.UnixMilli()).Scan(&duplicates))
	require.Zero(t, duplicates)
}

func TestDailyScheduleRollsBackReportsAndWatermarkTogether(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	end := time.Date(2063, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, service.prepareDailyIntervals(context.Background(), end))
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec(fmt.Sprintf(`ALTER TABLE Compliances ADD CONSTRAINT workspace_test_daily_schedule_reject CHECK (EndAt<>%d)`, end.Add(48*time.Hour).UnixMilli()))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`ALTER TABLE Compliances DROP CONSTRAINT IF EXISTS workspace_test_daily_schedule_reject`)
	})
	cutoff := end.Add(72 * time.Hour)
	require.ErrorContains(t, service.prepareDailyIntervals(context.Background(), cutoff), "workspace_test_daily_schedule_reject")
	require.Equal(t, end.UnixMilli(), dailyThrough(t, th))
	require.Equal(t, 1, dailyCount(t, th, end, cutoff), "even the first successful INSERT must roll back when the second fails")
	_, err = db.Exec(`ALTER TABLE Compliances DROP CONSTRAINT workspace_test_daily_schedule_reject`)
	require.NoError(t, err)
	require.NoError(t, (&Service{app: th.App}).prepareDailyIntervals(context.Background(), cutoff))
	require.Equal(t, cutoff.UnixMilli(), dailyThrough(t, th))
	require.Equal(t, 4, dailyCount(t, th, end, cutoff))
}

func TestDailySchedulePreservesFinishedAndRemovedReports(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	end := time.Date(2064, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, service.prepareDailyIntervals(context.Background(), end))
	db := th.Store.GetInternalMasterDB()
	for day, status := range []string{model.ComplianceStatusFinished, model.ComplianceStatusRemoved} {
		start := end.Add(time.Duration(day) * 24 * time.Hour)
		job := &model.Compliance{Type: model.ComplianceTypeDaily, Desc: start.Format(time.DateOnly), StartAt: start.UnixMilli(), EndAt: start.Add(24 * time.Hour).UnixMilli(), Status: status}
		created, inserted, err := createDailyJobOnce(context.Background(), db, job)
		require.NoError(t, err)
		require.True(t, inserted)
		require.NoError(t, service.prepareDailyIntervals(context.Background(), start.Add(24*time.Hour)))
		var foundID, foundStatus string
		require.NoError(t, db.QueryRow(`SELECT Id,Status FROM Compliances WHERE Type=$1 AND EndAt=$2`, model.ComplianceTypeDaily, job.EndAt).Scan(&foundID, &foundStatus))
		require.Equal(t, created.Id, foundID)
		require.Equal(t, status, foundStatus)
	}
}

func TestDailyScheduleRejectsInvalidWatermarkWithoutResetting(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	end := time.Date(2065, 1, 1, 0, 0, 0, 0, time.UTC)
	db := th.Store.GetInternalMasterDB()
	for _, raw := range []string{"", "broken", "-1", "1", strconv.FormatInt(end.Add(24*time.Hour).UnixMilli(), 10)} {
		t.Run(raw, func(t *testing.T) {
			_, err := db.Exec(`INSERT INTO Systems (Name,Value) VALUES ($1,$2) ON CONFLICT (Name) DO UPDATE SET Value=EXCLUDED.Value`, dailyScheduleKey, raw)
			require.NoError(t, err)
			require.ErrorContains(t, service.prepareDailyIntervals(context.Background(), end), "watermark")
			var stored string
			require.NoError(t, db.QueryRow(`SELECT Value FROM Systems WHERE Name=$1`, dailyScheduleKey).Scan(&stored))
			require.Equal(t, raw, stored)
			require.Zero(t, dailyCount(t, th, end.Add(-24*time.Hour), end))
		})
	}
}

func TestDailyScheduleRequiresLicenseAndBothSettings(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	end := time.Date(2066, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, mode := range []string{"no_license", "no_feature", "disabled", "daily_disabled"} {
		t.Run(mode, func(t *testing.T) {
			license := model.NewTestLicense()
			license.Features.Compliance = model.NewPointer(mode != "no_feature")
			if mode == "no_license" {
				th.Server.SetLicense(nil)
			} else {
				require.True(t, th.Server.SetLicense(license))
			}
			th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.ComplianceSettings.Enable = model.NewPointer(mode != "disabled")
				cfg.ComplianceSettings.EnableDaily = model.NewPointer(mode != "daily_disabled")
			})
			require.NoError(t, service.prepareDailyIntervals(context.Background(), end))
			_, err := service.runDailyCycle(context.Background(), end, 0)
			require.NoError(t, err)
			require.Zero(t, dailyCount(t, th, end.Add(-24*time.Hour), end))
			var count int
			require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM Systems WHERE Name=$1`, dailyScheduleKey).Scan(&count))
			require.Zero(t, count)
		})
	}
}

func TestDailyScheduleWaitingTransactionHonorsCancellationAndDisable(t *testing.T) {
	for _, mode := range []string{"cancel", "disable"} {
		t.Run(mode, func(t *testing.T) {
			th, service := dailyScheduleFixture(t)
			end := time.Date(2067, 1, 1, 0, 0, 0, 0, time.UTC)
			require.NoError(t, service.prepareDailyIntervals(context.Background(), end))
			db := th.Store.GetInternalMasterDB()
			conn, err := db.Conn(context.Background())
			require.NoError(t, err)
			_, err = conn.ExecContext(context.Background(), `SELECT pg_advisory_lock(11283544,2)`)
			require.NoError(t, err)
			var once sync.Once
			unlock := func() {
				once.Do(func() {
					_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(11283544,2)`)
					_ = conn.Close()
				})
			}
			t.Cleanup(unlock)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() { done <- service.prepareDailyIntervals(ctx, end.Add(48*time.Hour)) }()
			require.Eventually(t, func() bool {
				var waiting int
				return db.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype='advisory'
                    AND classid=11283544 AND objid=2 AND NOT granted`).Scan(&waiting) == nil && waiting > 0
			}, 2*time.Second, 10*time.Millisecond)
			if mode == "cancel" {
				cancel()
			} else {
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.ComplianceSettings.EnableDaily = model.NewPointer(false) })
				unlock()
			}
			select {
			case err := <-done:
				if mode == "cancel" {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.ErrorContains(t, err, "disabled")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("daily scheduling did not leave the interrupted transaction")
			}
			unlock()
			require.Equal(t, end.UnixMilli(), dailyThrough(t, th))
			require.Equal(t, 1, dailyCount(t, th, end, end.Add(48*time.Hour)))
		})
	}
}
