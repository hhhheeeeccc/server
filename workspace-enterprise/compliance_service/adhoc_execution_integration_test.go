package compliance_service

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

func TestAdhocOwnerDeduplicatesSameIDAndBoundsDistinctIDs(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.SqlSettings.QueryTimeout = model.NewPointer(1) })
	first, second, queued := savedReadReport(t, th), savedReadReport(t, th), savedReadReport(t, th)
	e1, owned, err := service.acquireAdhocExecution(context.Background(), first.Id)
	require.NoError(t, err)
	require.True(t, owned)
	defer e1.Close()
	duplicate, owned, err := service.acquireAdhocExecution(context.Background(), first.Id)
	require.NoError(t, err)
	require.False(t, owned)
	require.Nil(t, duplicate)
	// The skipped duplicate must release its process permit.
	e2, owned, err := service.acquireAdhocExecution(context.Background(), second.Id)
	require.NoError(t, err)
	require.True(t, owned)
	defer e2.Close()
	_, owned, err = service.acquireAdhocExecution(context.Background(), queued.Id)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, owned)
	stored, err := th.Store.Compliance().Get(queued.Id)
	require.NoError(t, err)
	require.Equal(t, model.ComplianceStatusCreated, stored.Status)
	e1.Close()
	require.Nil(t, service.RunComplianceJob(request.EmptyContext(th.App.Log()), queued))
	require.Equal(t, model.ComplianceStatusFinished, queued.Status)
}

func TestAdhocOwnerCannotUpdateAnotherReportOrReconnectAfterLoss(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	first, other := savedReadReport(t, th), savedReadReport(t, th)
	e, owned, err := service.acquireAdhocExecution(context.Background(), first.Id)
	require.NoError(t, err)
	require.True(t, owned)
	defer e.Close()
	other.Status = model.ComplianceStatusRunning
	require.ErrorContains(t, e.update(other), "does not match")
	first.Status = model.ComplianceStatusRunning
	require.NoError(t, e.update(first))
	var pid int
	require.NoError(t, e.query(e.ctx, func(ctx context.Context, db *sql.DB) error {
		return db.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
	}))
	var killed bool
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT pg_terminate_backend($1)`, pid).Scan(&killed))
	require.True(t, killed)
	first.Status = model.ComplianceStatusFinished
	require.ErrorIs(t, e.update(first), errComplianceOwnershipLost)
	stored, err := th.Store.Compliance().Get(first.Id)
	require.NoError(t, err)
	require.Equal(t, model.ComplianceStatusRunning, stored.Status)
	e.Close()
	require.Nil(t, service.RunComplianceJob(request.EmptyContext(th.App.Log()), first))
	require.Equal(t, model.ComplianceStatusFinished, first.Status)
	stored, err = th.Store.Compliance().Get(other.Id)
	require.NoError(t, err)
	require.Equal(t, model.ComplianceStatusCreated, stored.Status)
}

func TestAdhocDatabaseLockDeduplicatesEvenWithDifferentDirectories(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	job := savedReadReport(t, th)
	e, owned, err := service.acquireAdhocExecution(context.Background(), job.Id)
	require.NoError(t, err)
	require.True(t, owned)
	defer e.Close()
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ComplianceSettings.Directory = model.NewPointer(t.TempDir()) })
	duplicate, owned, err := service.acquireAdhocExecution(context.Background(), job.Id)
	require.NoError(t, err)
	require.False(t, owned, "the primary session lock must also protect against a misconfigured second directory")
	require.Nil(t, duplicate)
	job.Status = model.ComplianceStatusRunning
	require.NoError(t, e.update(job), "the skipped second owner must not unlock the first owner's backend")
	e.Close()
	require.Nil(t, service.RunComplianceJob(request.EmptyContext(th.App.Log()), job))
	require.Equal(t, model.ComplianceStatusFinished, job.Status)
}

func TestAdhocUsesSavedFiltersAndPreservesRemovedOrMissingReports(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	job := savedReadReport(t, th)
	_, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: th.BasicChannel.Id,
		UserId: th.BasicUser.Id, CreateAt: job.StartAt, Message: "المرشح المحفوظ"})
	require.NoError(t, err)
	caller := job.DeepCopy()
	caller.StartAt, caller.EndAt = job.StartAt+1000, job.EndAt+1000
	caller.Keywords, caller.Emails, caller.Desc = "no match", "absent@example.com", "changed by caller"
	require.Nil(t, service.RunComplianceJob(request.EmptyContext(th.App.Log()), caller))
	require.Equal(t, job.StartAt, caller.StartAt)
	require.Equal(t, job.EndAt, caller.EndAt)
	require.Equal(t, job.Keywords, caller.Keywords)
	require.Equal(t, job.Emails, caller.Emails)
	require.Equal(t, job.Desc, caller.Desc)
	require.Contains(t, readReportCSV(t, th, job)[1], "المرشح المحفوظ")
	removed := savedReadReport(t, th)
	removed.Status = model.ComplianceStatusRemoved
	_, err = th.Store.Compliance().Update(removed)
	require.NoError(t, err)
	require.Nil(t, service.RunComplianceJob(request.EmptyContext(th.App.Log()), removed))
	_, err = os.Stat(filepath.Join(*th.App.Config().ComplianceSettings.Directory, "compliance", removed.JobName()+".zip"))
	require.True(t, os.IsNotExist(err))
	missing := &model.Compliance{Id: model.NewId()}
	require.NotNil(t, service.RunComplianceJob(request.EmptyContext(th.App.Log()), missing))
	_, err = th.Store.Compliance().Get(missing.Id)
	require.Error(t, err)
}

func TestAdhocRecoveryRotatesPastPoisonReportsWithTiedTimestamps(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	jobs := []*model.Compliance{savedReadReport(t, th), savedReadReport(t, th), savedReadReport(t, th)}
	db := th.Store.GetInternalMasterDB()
	for _, job := range jobs {
		_, err := db.Exec(`UPDATE Compliances SET CreateAt=1 WHERE Id=$1`, job.Id)
		require.NoError(t, err)
	}
	var poisonID string
	require.NoError(t, db.QueryRow(`SELECT Id FROM Compliances WHERE Type='adhoc' ORDER BY CreateAt,Id LIMIT 1`).Scan(&poisonID))
	_, err := db.Exec(`ALTER TABLE Compliances ADD CONSTRAINT workspace_test_adhoc_poison
        CHECK (Id<>'` + poisonID + `' OR Status<>'running')`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`ALTER TABLE Compliances DROP CONSTRAINT IF EXISTS workspace_test_adhoc_poison`)
	})
	cursor, err := service.runPendingAdhoc(context.Background(), adhocCursor{})
	require.ErrorContains(t, err, "workspace_test_adhoc_poison")
	require.Equal(t, poisonID, cursor.id)
	for range 2 {
		next, err := service.runPendingAdhoc(context.Background(), cursor)
		require.NoError(t, err)
		require.NotEqual(t, poisonID, next.id)
		require.NotEqual(t, cursor.id, next.id)
		stored, err := th.Store.Compliance().Get(next.id)
		require.NoError(t, err)
		require.Equal(t, model.ComplianceStatusFinished, stored.Status)
		cursor = next
	}
	wrapped, err := service.runPendingAdhoc(context.Background(), cursor)
	require.ErrorContains(t, err, "workspace_test_adhoc_poison")
	require.Equal(t, poisonID, wrapped.id)
}

func TestAdhocRecoveryRequiresLicenseAndSetting(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	job := savedReadReport(t, th)
	for _, mode := range []string{"unlicensed", "no_feature", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			license := model.NewTestLicense()
			license.Features.Compliance = model.NewPointer(mode != "no_feature")
			if mode == "unlicensed" {
				th.Server.SetLicense(nil)
			} else {
				require.True(t, th.Server.SetLicense(license))
			}
			th.App.UpdateConfig(func(cfg *model.Config) { cfg.ComplianceSettings.Enable = model.NewPointer(mode != "disabled") })
			next, err := service.runPendingAdhoc(context.Background(), adhocCursor{})
			require.NoError(t, err)
			require.Empty(t, next.id)
			require.NotNil(t, service.RunComplianceJob(request.EmptyContext(th.App.Log()), job))
			stored, err := th.Store.Compliance().Get(job.Id)
			require.NoError(t, err)
			require.Equal(t, model.ComplianceStatusCreated, stored.Status)
		})
	}
	_, err := os.Stat(filepath.Join(*th.App.Config().ComplianceSettings.Directory, "compliance"))
	require.True(t, os.IsNotExist(err))
}

func TestAdhocSchedulerStopsOnCancellation(t *testing.T) {
	_, service := dailyScheduleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { defer close(done); service.runAdhocScheduler(ctx, time.Hour) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("adhoc scheduler continued after shutdown cancellation")
	}
}
