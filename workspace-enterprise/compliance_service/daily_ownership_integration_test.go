package compliance_service

import (
	"context"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestDailyExportLostOwnerCannotOverwriteSuccessor(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	db := th.Store.GetInternalMasterDB()
	primary := *th.App.Config().SqlSettings.DataSource
	proxy, proxied := newComplianceSQLProxy(t, primary)
	defer proxy.Close()
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.SqlSettings.DataSource = model.NewPointer(proxied) })
	end := time.Date(2072, 1, 2, 0, 0, 0, 0, time.UTC)
	job, _, err := findOrCreateDailyJob(context.Background(), db, &model.Compliance{Type: model.ComplianceTypeDaily,
		Desc: "2072-01-01", StartAt: end.Add(-24 * time.Hour).UnixMilli(), EndAt: end.UnixMilli()})
	require.NoError(t, err)
	_, err = th.Store.Post().Save(th.Context, &model.Post{ChannelId: th.BasicChannel.Id,
		UserId: th.BasicUser.Id, CreateAt: job.StartAt, Message: "نتيجة العامل البديل"})
	require.NoError(t, err)
	blocker, err := db.Begin()
	require.NoError(t, err)
	defer blocker.Rollback()
	_, err = blocker.Exec(`LOCK TABLE Posts IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldDone := make(chan error, 1)
	go func() { oldDone <- service.runDailyInterval(ctx, end) }()
	defer func() { cancel(); proxy.Close(); <-oldDone }()
	var ownerPID int
	require.Eventually(t, func() bool {
		var reading bool
		if db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
            AND wait_event_type='Lock' AND query ILIKE '%TeamName%' AND query ILIKE '%Posts%')`).Scan(&reading) != nil || !reading {
			return false
		}
		return db.QueryRow(`SELECT pid FROM pg_locks WHERE locktype='advisory'
            AND classid=11283544 AND objid=3 AND objsubid=2 AND granted`).Scan(&ownerPID) == nil
	}, 5*time.Second, 10*time.Millisecond)
	// The old report's read is in PostgreSQL. Drop its eventual result, kill
	// only the session owning the export lock, then let a real successor run.
	proxy.blocked.Store(true)
	var terminated bool
	require.NoError(t, db.QueryRow(`SELECT pg_terminate_backend($1)`, ownerPID).Scan(&terminated))
	require.True(t, terminated)
	require.NoError(t, blocker.Rollback())
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.SqlSettings.DataSource = model.NewPointer(primary) })
	require.Eventually(t, func() bool {
		if service.runDailyInterval(context.Background(), end) != nil {
			return false
		}
		stored, err := th.Store.Compliance().Get(job.Id)
		return err == nil && stored.Status == model.ComplianceStatusFinished && stored.Count == 1
	}, 8*time.Second, 50*time.Millisecond, "a live successor must finish the same daily report")
	before := readReportCSV(t, th, job)
	// An old implementation now marks this finished report failed through the
	// unrelated application pool when its silent read is canceled.
	cancel()
	select {
	case oldErr := <-oldDone:
		require.Error(t, oldErr)
		oldDone <- oldErr // leave cleanup a terminal result to drain
	case <-time.After(5 * time.Second):
		t.Fatal("the old report did not stop after cancellation")
	}
	stored, err := th.Store.Compliance().Get(job.Id)
	require.NoError(t, err)
	t.Logf("successor report after old owner returned: status=%s count=%d", stored.Status, stored.Count)
	require.Equal(t, model.ComplianceStatusFinished, stored.Status, "the old owner overwrote its successor's confirmed result")
	require.Equal(t, 1, stored.Count)
	require.Equal(t, before, readReportCSV(t, th, job))
}
