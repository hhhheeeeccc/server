package compliance_service

import (
	"context"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

func TestAdhocStartupRecoversRunningReportWithDailyDisabled(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ComplianceSettings.EnableDaily = model.NewPointer(false) })
	job := savedReadReport(t, th)
	job.Status = model.ComplianceStatusRunning
	_, err := th.Store.Compliance().Update(job)
	require.NoError(t, err)
	_, err = th.Store.Post().Save(th.Context, &model.Post{ChannelId: th.BasicChannel.Id,
		UserId: th.BasicUser.Id, CreateAt: job.StartAt, Message: "استعادة التقرير المخصص"})
	require.NoError(t, err)
	// Invoke the same public hook used by the real server startup. The old
	// hook only schedules daily reports and never resumes this persisted row.
	go service.StartComplianceDailyJob()
	require.Eventually(t, func() bool {
		stored, err := th.Store.Compliance().Get(job.Id)
		return err == nil && stored.Status == model.ComplianceStatusFinished && stored.Count == 1
	}, 2*time.Second, 10*time.Millisecond, "startup did not recover the abandoned adhoc report with daily reports disabled")
	records := readReportCSV(t, th, job)
	require.Len(t, records, 2)
	require.Contains(t, records[1], "استعادة التقرير المخصص")
}

func TestAdhocCompletedReportRejectsReplayFromStaleCaller(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	job := savedReadReport(t, th)
	stale := job.DeepCopy()
	post, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: th.BasicChannel.Id,
		UserId: th.BasicUser.Id, CreateAt: job.StartAt, Message: "النص الذي أكمله التقرير"})
	require.NoError(t, err)
	require.Nil(t, service.RunComplianceJob(request.EmptyContext(th.App.Log()), job))
	confirmed := readReportCSV(t, th, job)
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE Posts SET Message=$1 WHERE Id=$2`, "نص تغير بعد اكتمال التقرير", post.Id)
	require.NoError(t, err)
	require.Nil(t, service.RunComplianceJob(request.EmptyContext(th.App.Log()), stale))
	after := readReportCSV(t, th, job)
	t.Logf("confirmed report changed after stale replay: %t", after[1][15] != confirmed[1][15])
	require.Equal(t, confirmed, after, "a completed report must not be rebuilt by a delayed caller")
}

func TestAdhocMetadataCancellationInterruptsBlockedSQL(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	job := savedReadReport(t, th)
	db := th.Store.GetInternalMasterDB()
	blocker, err := db.Begin()
	require.NoError(t, err)
	defer blocker.Rollback()
	_, err = blocker.Exec(`LOCK TABLE Compliances IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *model.AppError, 1)
	go func() { done <- service.runComplianceJob(ctx, request.EmptyContext(th.App.Log()), job) }()
	require.Eventually(t, func() bool {
		var blocked bool
		return db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
            AND wait_event_type='Lock' AND query ILIKE '%Compliances%')`).Scan(&blocked) == nil && blocked
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case appErr := <-done:
		require.NotNil(t, appErr)
		require.ErrorIs(t, appErr, context.Canceled)
	case <-time.After(time.Second):
		_ = blocker.Rollback()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("report did not return even after releasing its metadata lock")
		}
		t.Fatal("canceled adhoc metadata still waited for the Compliances lock")
	}
	require.NoError(t, blocker.Rollback())
	stored, err := th.Store.Compliance().Get(job.Id)
	require.NoError(t, err)
	require.Equal(t, model.ComplianceStatusCreated, stored.Status, "a canceled metadata read must not begin an export")
}
