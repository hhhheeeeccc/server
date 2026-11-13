package access_control_job

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

func (w *Worker) resultFor(resourceID string) *membershipSyncResult {
	if w.results == nil {
		w.results = make(map[string]*membershipSyncResult)
	}
	if w.results[resourceID] == nil {
		w.results[resourceID] = &membershipSyncResult{MembersAdded: []string{}, MembersRemoved: []string{}}
	}
	return w.results[resourceID]
}

func (w *Worker) saveResults(job *model.Job) error {
	encoded, err := json.Marshal(w.results)
	if err != nil {
		return err
	}
	if job.Data == nil {
		job.Data = make(map[string]string)
	}
	job.Data["sync_results"] = string(encoded)
	job.LastActivityAt = model.GetMillis()
	// Preserve a concurrent cancellation while retaining any changes already
	// applied; the job must never be changed back to in-progress.
	for _, status := range []string{model.JobStatusInProgress, model.JobStatusCancelRequested} {
		job.Status = status
		updated, err := w.jobServer.Store.Job().UpdateOptimistically(job, status)
		if err != nil {
			return err
		}
		if updated != nil {
			if status == model.JobStatusCancelRequested {
				w.interruption = jobCanceled
			}
			return nil
		}
	}
	return fmt.Errorf("membership sync job %s is no longer running", job.Id)
}

func (w *Worker) chainChannelSync(rctx request.CTX, parentJobID string, cancelRequested <-chan struct{}) (stopReason, error) {
	if len(w.results) == 0 {
		return continueWork, nil
	}
	if reason := w.stopRequested(cancelRequested); reason != continueWork {
		return reason, nil
	}
	pending, err := w.jobServer.Store.Job().GetByTypeAndData(rctx, model.JobTypeAccessControlSync, nil, true,
		model.JobStatusPending, model.JobStatusInProgress)
	if err != nil {
		return continueWork, err
	}
	for _, job := range pending {
		if job.Data["team_id"] == "" && job.Data["policy_id"] == "" && job.Data["channel_ids"] == "" {
			// An existing global channel sync already covers these teams.
			return continueWork, nil
		}
	}
	teamIDs := make([]string, 0, len(w.results))
	for teamID := range w.results {
		teamIDs = append(teamIDs, teamID)
	}
	sort.Strings(teamIDs)
	for _, teamID := range teamIDs {
		if reason := w.stopRequested(cancelRequested); reason != continueWork {
			return reason, nil
		}
		_, appErr := w.jobServer.CreateJobOnceByTypeAndData(rctx, model.JobTypeAccessControlSync,
			map[string]string{"team_id": teamID, "parent_team_job_id": parentJobID}, map[string]string{"team_id": teamID})
		if appErr != nil {
			return continueWork, fmt.Errorf("chain channel sync for team %s: %w", teamID, appErr)
		}
	}
	return continueWork, nil
}
