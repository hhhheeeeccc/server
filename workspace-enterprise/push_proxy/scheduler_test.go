package push_proxy

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

func TestSchedulerSkipsPendingJob(t *testing.T) {
	created := 0
	scheduler := &Scheduler{createJob: func(request.CTX) (*model.Job, *model.AppError) {
		created++
		return &model.Job{Id: model.NewId()}, nil
	}}

	job, err := scheduler.ScheduleJob(nil, nil, true, nil)
	require.Nil(t, err)
	require.Nil(t, job)
	require.Zero(t, created)

	job, err = scheduler.ScheduleJob(nil, nil, false, nil)
	require.Nil(t, err)
	require.NotNil(t, job)
	require.Equal(t, 1, created)
}
