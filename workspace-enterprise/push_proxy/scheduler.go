package push_proxy

import (
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
)

// Scheduler retries token initialization while keeping at most one outstanding
// auth job. The atomic SaveOnce in createJob also closes the gap
// between the scheduler's pending-job check and a concurrent scheduler.
type Scheduler struct {
	*jobs.PeriodicScheduler
	createJob func(request.CTX) (*model.Job, *model.AppError)
}

var _ jobs.Scheduler = (*Scheduler)(nil)

func (s *Scheduler) ScheduleJob(rctx request.CTX, _ *model.Config, pendingJobs bool, _ *model.Job) (*model.Job, *model.AppError) {
	if pendingJobs {
		return nil, nil
	}
	return s.createJob(rctx)
}
