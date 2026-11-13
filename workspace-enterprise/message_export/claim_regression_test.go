package message_export

import (
	"testing"

	"github.com/mattermost/enterprise/message_export/shared"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

// A recovered claim can again be in_progress. Checking only that status is
// insufficient: callbacks from the first attempt must not overwrite the next.
func TestMessageExportObsoleteAttemptCannotChangeNewClaim(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	worker := NewJob(th.Server).MakeWorker().(*MessageExportWorker)
	for _, action := range []string{"progress", "message", "success", "warning", "error", "pending", "cancel"} {
		t.Run(action, func(t *testing.T) {
			status := model.JobStatusInProgress
			if action == "cancel" {
				status = model.JobStatusCancelRequested
			}
			job, err := th.Store.Job().Save(&model.Job{Id: model.NewId(), Type: model.JobTypeMessageExport,
				CreateAt: model.GetMillis(), Status: status, Progress: 40, Data: model.StringMap{
					"workspace_message_export_attempt_v1": model.NewId(), shared.JobDataBatchNumber: "2",
				}})
			require.NoError(t, err)
			stale := *job
			stale.Data = model.StringMap{"workspace_message_export_attempt_v1": model.NewId(), shared.JobDataBatchNumber: "1"}
			switch action {
			case "progress":
				_ = worker.setJobProgress(th.App.Log(), &stale, 20)
			case "message":
				worker.setJobProgressMessage(0, "obsolete callback", th.App.Log(), &stale)
			case "success":
				worker.setJobSuccess(th.App.Log(), &stale)
			case "warning":
				worker.setJobWarning(th.App.Log(), &stale)
			case "error":
				worker.setJobError(th.App.Log(), &stale, model.NewAppError("obsolete", "obsolete", nil, "old failure", 500))
			case "pending":
				worker.SetJobPending(th.App.Log(), &stale)
			case "cancel":
				worker.setJobCanceled(th.App.Log(), &stale)
			}
			stored, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			t.Logf("obsolete %s callback: stored status=%s batch=%s", action, stored.Status, stored.Data[shared.JobDataBatchNumber])
			require.Equal(t, job.Status, stored.Status, "an obsolete attempt changed the new claim's status")
			require.Equal(t, job.Progress, stored.Progress)
			require.Equal(t, job.Data, stored.Data, "an obsolete attempt replaced the new claim's saved cursor")
		})
	}
}
