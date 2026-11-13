package compliance_service

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestCreateDailyJobOnceAcrossConnections(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	db := th.Store.GetInternalMasterDB()
	start := time.Now().Add(100*365*24*time.Hour).UnixMilli() + int64(time.Now().Nanosecond())
	const callers = 8
	results := make(chan bool, callers)
	errors := make(chan error, callers)
	startCalls := make(chan struct{})
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			<-startCalls
			job := &model.Compliance{
				Desc:    "daily concurrency test",
				Type:    model.ComplianceTypeDaily,
				StartAt: start,
				EndAt:   start + int64((24*time.Hour)/time.Millisecond),
			}
			_, created, err := createDailyJobOnce(context.Background(), db, job)
			results <- created
			if err != nil {
				errors <- err
			}
		})
	}
	close(startCalls)
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	createdCount := 0
	for created := range results {
		if created {
			createdCount++
		}
	}
	require.Equal(t, 1, createdCount)
	var storedCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM Compliances WHERE Type=$1 AND StartAt=$2`, model.ComplianceTypeDaily, start).Scan(&storedCount))
	require.Equal(t, 1, storedCount)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM Compliances WHERE Type=$1 AND StartAt=$2`, model.ComplianceTypeDaily, start)
	})
}

func TestFindOrCreateDailyJobResumesSameRecord(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	db := th.Store.GetInternalMasterDB()
	start := time.Now().Add(101*365*24*time.Hour).UnixMilli() + int64(time.Now().Nanosecond())
	job := &model.Compliance{Desc: "daily resume test", Type: model.ComplianceTypeDaily,
		StartAt: start, EndAt: start + int64((24*time.Hour)/time.Millisecond)}
	created, run, err := findOrCreateDailyJob(context.Background(), db, job)
	require.NoError(t, err)
	require.True(t, run)
	require.NotEmpty(t, created.Id)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM Compliances WHERE Id=$1`, created.Id)
	})
	_, err = db.Exec(`UPDATE Compliances SET Status=$1 WHERE Id=$2`, model.ComplianceStatusRunning, created.Id)
	require.NoError(t, err)
	resumed, run, err := findOrCreateDailyJob(context.Background(), db, job)
	require.NoError(t, err)
	require.True(t, run)
	require.Equal(t, created.Id, resumed.Id)
	_, err = db.Exec(`UPDATE Compliances SET Status=$1 WHERE Id=$2`, model.ComplianceStatusFinished, created.Id)
	require.NoError(t, err)
	finished, run, err := findOrCreateDailyJob(context.Background(), db, job)
	require.NoError(t, err)
	require.False(t, run)
	require.Equal(t, created.Id, finished.Id)
}

func TestRunDailyIntervalRestoresInterruptedExport(t *testing.T) {
	directory := t.TempDir()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ComplianceSettings.Enable = model.NewPointer(true)
		cfg.ComplianceSettings.EnableDaily = model.NewPointer(true)
		cfg.ComplianceSettings.Directory = model.NewPointer(directory)
	}).InitBasic(t)
	license := model.NewTestLicense()
	license.Features.Compliance = model.NewPointer(true)
	th.App.Srv().SetLicense(license)
	service := &Service{app: th.App}
	end := time.Now().UTC().AddDate(80, 0, 0).Truncate(24 * time.Hour)
	require.NoError(t, service.runDailyInterval(context.Background(), end))
	db := th.Store.GetInternalMasterDB()
	var id, status string
	start := end.Add(-24 * time.Hour).UnixMilli()
	require.NoError(t, db.QueryRow(`SELECT Id,Status FROM Compliances WHERE Type=$1 AND StartAt=$2`,
		model.ComplianceTypeDaily, start).Scan(&id, &status))
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM Compliances WHERE Id=$1`, id)
	})
	require.Equal(t, model.ComplianceStatusFinished, status)
	job := &model.Compliance{Id: id, Type: model.ComplianceTypeDaily, Desc: end.Add(-24 * time.Hour).Format(time.DateOnly)}
	file := filepath.Join(directory, "compliance", job.JobName()+".zip")
	_, err := os.Stat(file)
	require.NoError(t, err)
	require.NoError(t, os.Remove(file))
	_, err = db.Exec(`UPDATE Compliances SET Status=$1 WHERE Id=$2`, model.ComplianceStatusRunning, id)
	require.NoError(t, err)
	require.NoError(t, service.runDailyInterval(context.Background(), end))
	_, err = os.Stat(file)
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT Status FROM Compliances WHERE Id=$1`, id).Scan(&status))
	require.Equal(t, model.ComplianceStatusFinished, status)
	require.NoError(t, os.Remove(file))
	_, err = db.Exec(`UPDATE Compliances SET Status=$1 WHERE Id=$2`, model.ComplianceStatusRunning, id)
	require.NoError(t, err)
	attempted, err := service.runPendingDailyInterval(context.Background(), end.Add(24*time.Hour), 0)
	require.NoError(t, err)
	require.Equal(t, end.UnixMilli(), attempted)
	_, err = os.Stat(file)
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT Status FROM Compliances WHERE Id=$1`, id).Scan(&status))
	require.Equal(t, model.ComplianceStatusFinished, status)
	require.NoError(t, service.runDailyInterval(context.Background(), end))
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM Compliances WHERE Type=$1 AND StartAt=$2`,
		model.ComplianceTypeDaily, start).Scan(&count))
	require.Equal(t, 1, count)
}

func TestPendingDailyRotationAfterExportFailure(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(directory, []byte("occupied"), 0o600))
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ComplianceSettings.Enable = model.NewPointer(true)
		cfg.ComplianceSettings.EnableDaily = model.NewPointer(true)
		cfg.ComplianceSettings.Directory = model.NewPointer(directory)
	}).InitBasic(t)
	license := model.NewTestLicense()
	license.Features.Compliance = model.NewPointer(true)
	th.App.Srv().SetLicense(license)
	service := &Service{app: th.App}
	db := th.Store.GetInternalMasterDB()
	firstEnd := time.Now().UTC().AddDate(81, 0, 0).Truncate(24 * time.Hour)
	secondEnd := firstEnd.Add(24 * time.Hour)
	for _, end := range []time.Time{firstEnd, secondEnd} {
		job := &model.Compliance{Desc: end.Add(-24 * time.Hour).Format(time.DateOnly), Type: model.ComplianceTypeDaily,
			StartAt: end.Add(-24 * time.Hour).UnixMilli(), EndAt: end.UnixMilli()}
		created, inserted, err := createDailyJobOnce(context.Background(), db, job)
		require.NoError(t, err)
		require.True(t, inserted)
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM Compliances WHERE Id=$1`, created.Id) })
	}
	before := secondEnd.Add(24 * time.Hour)
	attempted, err := service.runPendingDailyInterval(context.Background(), before, 0)
	require.Error(t, err)
	require.Equal(t, firstEnd.UnixMilli(), attempted)
	attempted, err = service.runPendingDailyInterval(context.Background(), before, attempted)
	require.Error(t, err)
	require.Equal(t, secondEnd.UnixMilli(), attempted)
}

func TestComplianceBatchRejectsRevokedLicense(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ComplianceSettings.Enable = model.NewPointer(true)
	}).InitBasic(t)
	license := model.NewTestLicense()
	license.Features.Compliance = model.NewPointer(true)
	th.App.Srv().SetLicense(license)
	service := &Service{app: th.App}
	require.NoError(t, service.checkComplianceExportEnabled())
	th.App.Srv().SetLicense(nil)
	_, _, err := service.fetchComplianceBatch(context.Background(), nil, nil, model.ComplianceExportCursor{}, 1)
	require.EqualError(t, err, "compliance export is disabled or unlicensed")
}

func TestDailySchedulerStopsOnServerContextCancellation(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ComplianceSettings.EnableDaily = model.NewPointer(false)
	}).InitBasic(t)
	service := &Service{app: th.App}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		service.runDailyScheduler(ctx, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("daily scheduler continued after shutdown cancellation")
	}
}

func TestDailyExportStopsBeforePublishingAfterCancellation(t *testing.T) {
	directory := t.TempDir()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ComplianceSettings.Enable = model.NewPointer(true)
		cfg.ComplianceSettings.EnableDaily = model.NewPointer(true)
		cfg.ComplianceSettings.Directory = model.NewPointer(directory)
	}).InitBasic(t)
	license := model.NewTestLicense()
	license.Features.Compliance = model.NewPointer(true)
	th.App.Srv().SetLicense(license)
	service := &Service{app: th.App}
	end := time.Now().UTC().AddDate(82, 0, 0).Truncate(24 * time.Hour)
	job := &model.Compliance{Desc: "daily cancellation test", Type: model.ComplianceTypeDaily,
		StartAt: end.Add(-24 * time.Hour).UnixMilli(), EndAt: end.UnixMilli()}
	db := th.Store.GetInternalMasterDB()
	created, inserted, err := createDailyJobOnce(context.Background(), db, job)
	require.NoError(t, err)
	require.True(t, inserted)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM Compliances WHERE Id=$1`, created.Id) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, service.runDailyInterval(ctx, end), context.Canceled)
	var status string
	require.NoError(t, db.QueryRow(`SELECT Status FROM Compliances WHERE Id=$1`, created.Id).Scan(&status))
	require.Equal(t, model.ComplianceStatusCreated, status, "a canceled caller cannot write a state without ownership")
	_, err = os.Stat(filepath.Join(directory, "compliance"))
	require.True(t, os.IsNotExist(err))
}

func TestMain(m *testing.M) {
	if path := os.Getenv(dailyFileLockProcessEnv); path != "" {
		os.Exit(runDailyFileLockProcess(path))
	}
	if input := os.Getenv(dailyScheduleProcessEnv); input != "" {
		os.Exit(runDailyScheduleProcess(input))
	}
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}
