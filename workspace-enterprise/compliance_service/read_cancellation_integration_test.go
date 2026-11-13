package compliance_service

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func savedReadReport(t *testing.T, th *api4.TestHelper) *model.Compliance {
	t.Helper()
	start := time.Date(2070, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	job, err := th.Store.Compliance().Save(&model.Compliance{Desc: "قراءة تقرير الاختبار",
		Type: model.ComplianceTypeAdhoc, StartAt: start, EndAt: start + 86400000})
	require.NoError(t, err)
	return job
}

func readReportCSV(t *testing.T, th *api4.TestHelper, job *model.Compliance) [][]string {
	t.Helper()
	archive, err := zip.OpenReader(filepath.Join(*th.App.Config().ComplianceSettings.Directory, "compliance", job.JobName()+".zip"))
	require.NoError(t, err)
	defer archive.Close()
	require.Len(t, archive.File, 1)
	file, err := archive.File[0].Open()
	require.NoError(t, err)
	defer file.Close()
	records, err := csv.NewReader(file).ReadAll()
	require.NoError(t, err)
	return records
}

func TestComplianceReadCancellationInterruptsBlockedSQL(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	job := savedReadReport(t, th)
	db := th.Store.GetInternalMasterDB()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`LOCK TABLE Posts IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan *model.AppError, 1)
	go func() { finished <- service.runComplianceJob(ctx, request.EmptyContext(th.App.Log()), job) }()
	// Observe a real SQL read waiting for our relation lock before canceling.
	require.Eventually(t, func() bool {
		var waiting bool
		return db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			AND query ILIKE '%Posts%' AND query ILIKE '%TeamName%')`).Scan(&waiting) == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	var appErr *model.AppError
	select {
	case appErr = <-finished:
	case <-time.After(time.Second):
		// Always unblock the old implementation before fixture teardown.
		_ = tx.Rollback()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("report did not return even after releasing the test lock")
		}
		t.Fatal("canceled compliance read waited for the Posts lock to be released")
	}
	require.NotNil(t, appErr)
	require.ErrorIs(t, appErr, context.Canceled)
	stored, err := th.Store.Compliance().Get(job.Id)
	require.NoError(t, err)
	require.Equal(t, model.ComplianceStatusRunning, stored.Status, "cancellation leaves a recoverable report without an unowned metadata update")
	files, err := os.ReadDir(filepath.Join(*th.App.Config().ComplianceSettings.Directory, "compliance"))
	require.NoError(t, err)
	for _, file := range files {
		require.Equal(t, ".adhoc-"+job.Id+".lock", file.Name(), "cancellation may retain only the permanent lock inode")
	}
	require.NoError(t, tx.Rollback())
	next, recoverErr := service.runPendingAdhoc(context.Background(), adhocCursor{})
	require.NoError(t, recoverErr)
	require.Equal(t, job.Id, next.id)
	stored, err = th.Store.Compliance().Get(job.Id)
	require.NoError(t, err)
	require.Equal(t, model.ComplianceStatusFinished, stored.Status)
	require.Len(t, readReportCSV(t, th, job), 1)
	require.NoError(t, db.Ping(), "private reader cancellation must leave the app pool usable")
}

func TestComplianceMixedChannelAndDirectPagesKeepEveryPost(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ComplianceSettings.BatchSize = model.NewPointer(2) })
	job := savedReadReport(t, th)
	direct, appErr := th.App.GetOrCreateDirectChannel(th.Context, th.BasicUser.Id, th.BasicUser2.Id)
	require.Nil(t, appErr)
	group, err := th.Store.Channel().Save(th.Context, &model.Channel{Name: model.NewId(), Type: model.ChannelTypeGroup}, 0)
	require.NoError(t, err)
	var expected []string
	for i, channel := range []*model.Channel{th.BasicChannel, direct, group, direct, group, direct} {
		post, saveErr := th.Store.Post().Save(th.Context, &model.Post{ChannelId: channel.Id,
			UserId: th.BasicUser.Id, Message: fmt.Sprintf("رسالة %d", i), CreateAt: job.StartAt})
		require.NoError(t, saveErr)
		expected = append(expected, post.Id)
	}
	require.Nil(t, service.runComplianceJob(context.Background(), request.EmptyContext(th.App.Log()), job))
	records := readReportCSV(t, th, job)
	var actual []string
	for _, row := range records[1:] {
		actual = append(actual, row[9])
	}
	t.Logf("exported mixed channel/direct posts: %d of %d", len(actual), len(expected))
	require.ElementsMatch(t, expected, actual, "a partially filled channel page must not truncate the direct/group stream")
}
