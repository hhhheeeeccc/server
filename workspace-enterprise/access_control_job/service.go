// Package access_control_job synchronizes channel and team membership with
// the ABAC policies evaluated by the Enterprise access-control service.
package access_control_job

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	ejobs "github.com/mattermost/mattermost/server/v8/einterfaces/jobs"
)

const (
	policyPageSize = 1000
	userPageSize   = 200
)

type Builder struct {
	jobServer *jobs.JobServer
	app       *app.App
	jobType   string
}

type Worker struct {
	jobServer   *jobs.JobServer
	app         *app.App
	jobType     string
	jobs        chan model.Job
	stop        chan bool
	stopped     chan bool
	lifecycleMu sync.Mutex
	running     bool
	logger      mlog.LoggerIFace
	// Only the worker goroutine reads/writes this. Keep an observed stop or
	// cancellation visible to every enclosing loop until the job completes.
	interruption stopReason
	results      map[string]*membershipSyncResult
}

// Keep these JSON field names aligned with ChannelMembersSyncResults and
// TeamMembersSyncResults consumed by Mattermost's existing job-details modal.
type membershipSyncResult struct {
	MembersAdded       []string
	MembersRemoved     []string
	MassRemovalWarning bool
}

type stopReason uint8

const (
	continueWork stopReason = iota
	jobCanceled
	workerStopped
)

var _ ejobs.AccessControlSyncJobInterface = (*Builder)(nil)
var _ model.Worker = (*Worker)(nil)

func NewChannel(server *app.Server) ejobs.AccessControlSyncJobInterface {
	return newBuilder(server, model.JobTypeAccessControlSync)
}

func NewTeam(server *app.Server) ejobs.AccessControlSyncJobInterface {
	return newBuilder(server, model.JobTypeAccessControlTeamSync)
}

func newBuilder(server *app.Server, jobType string) *Builder {
	return &Builder{
		jobServer: server.Jobs,
		app:       app.New(app.ServerConnector(server.Channels())),
		jobType:   jobType,
	}
}

func (b *Builder) MakeWorker() model.Worker {
	return &Worker{
		jobServer: b.jobServer,
		app:       b.app,
		jobType:   b.jobType,
		jobs:      make(chan model.Job),
		stop:      make(chan bool, 1),
		stopped:   make(chan bool, 1),
		logger:    b.jobServer.Logger().With(mlog.String("worker_name", workerName(b.jobType))),
	}
}

func (b *Builder) MakeScheduler() ejobs.Scheduler {
	period := time.Hour
	if cfg := b.app.Config(); cfg != nil && cfg.AccessControlSettings.SyncJobIntervalSeconds != nil {
		period = time.Duration(*cfg.AccessControlSettings.SyncJobIntervalSeconds) * time.Second
	}
	jobType := b.jobType
	return jobs.NewPeriodicScheduler(b.jobServer, jobType, period, func(cfg *model.Config) bool {
		return syncEnabled(cfg, jobType)
	})
}

func workerName(jobType string) string {
	if jobType == model.JobTypeAccessControlTeamSync {
		return "WorkspaceAccessControlTeamSync"
	}
	return "WorkspaceAccessControlChannelSync"
}

func syncEnabled(cfg *model.Config, jobType string) bool {
	if cfg == nil || cfg.AccessControlSettings.EnableAttributeBasedAccessControl == nil || !*cfg.AccessControlSettings.EnableAttributeBasedAccessControl {
		return false
	}
	if !cfg.FeatureFlags.PermissionPolicies {
		return false
	}
	return jobType != model.JobTypeAccessControlTeamSync || cfg.FeatureFlags.TeamMembershipAccessControl
}

func (w *Worker) Run() {
	w.lifecycleMu.Lock()
	if w.running {
		w.lifecycleMu.Unlock()
		return
	}
	// The same worker is restarted when access control is re-enabled.
	select {
	case <-w.stopped:
		w.stop = make(chan bool, 1)
		w.stopped = make(chan bool, 1)
	default:
	}
	w.running = true
	w.lifecycleMu.Unlock()

	w.logger.Debug("Worker started")
	defer func() {
		w.logger.Debug("Worker finished")
		w.lifecycleMu.Lock()
		w.running = false
		close(w.stopped)
		w.lifecycleMu.Unlock()
	}()
	for {
		select {
		case <-w.stop:
			return
		case job := <-w.jobs:
			if w.doJob(&job) {
				return
			}
		}
	}
}

func (w *Worker) Stop() {
	w.logger.Debug("Worker stopping")
	w.lifecycleMu.Lock()
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	stopped := w.stopped
	w.lifecycleMu.Unlock()
	<-stopped
}

func (w *Worker) JobChannel() chan<- model.Job {
	return w.jobs
}

func (w *Worker) IsEnabled(cfg *model.Config) bool {
	return syncEnabled(cfg, w.jobType)
}

func (w *Worker) doJob(job *model.Job) bool {
	w.interruption = continueWork
	w.results = make(map[string]*membershipSyncResult)
	logger := w.logger.With(jobs.JobLoggerFields(job)...)
	defer w.jobServer.HandleJobPanic(logger, job)

	claimed, appErr := w.jobServer.ClaimJob(job)
	if appErr != nil {
		logger.Warn("Unable to claim access-control sync job", mlog.Err(appErr))
		return false
	}
	if claimed == nil {
		return false
	}
	job = claimed

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelRequested := make(chan struct{}, 1)
	rctx := request.EmptyContext(logger).WithContext(ctx)
	go w.jobServer.CancellationWatcher(rctx, job.Id, cancelRequested)

	reason, err := w.execute(rctx, job.Data, cancelRequested)
	if saveErr := w.saveResults(job); saveErr != nil {
		logger.Error("Unable to save access-control membership results", mlog.Err(saveErr))
		if err == nil {
			err = saveErr
		}
	}
	if reason == continueWork {
		reason = w.stopRequested(cancelRequested)
	}
	// An explicit cancellation wins if it arrived while results were saved.
	if w.interruption == jobCanceled {
		reason = jobCanceled
	}
	if reason == continueWork && err == nil && w.jobType == model.JobTypeAccessControlTeamSync {
		reason, err = w.chainChannelSync(rctx, job.Id, cancelRequested)
		if reason == continueWork {
			reason = w.stopRequested(cancelRequested)
		}
	}
	if reason != continueWork {
		return w.finishInterruption(logger, job, reason)
	}
	if err != nil {
		jobErr := model.NewAppError(workerName(w.jobType), "app.job.error", nil, "", http.StatusInternalServerError).Wrap(err)
		if appErr := w.jobServer.SetJobError(job, jobErr); appErr != nil {
			logger.Error("Unable to mark access-control sync job as failed", mlog.Err(appErr))
		}
		return false
	}
	if appErr := w.jobServer.SetJobProgress(job, 100); appErr != nil {
		logger.Error("Unable to set access-control sync job progress", mlog.Err(appErr))
		if setErr := w.jobServer.SetJobError(job, appErr); setErr != nil {
			logger.Error("Unable to mark access-control sync job as failed", mlog.Err(setErr))
		}
		return false
	}
	if _, finishErr := w.finishJobStatus(job, model.JobStatusInProgress, model.JobStatusSuccess); finishErr != nil {
		logger.Error("Unable to mark access-control sync job as successful", mlog.Err(finishErr))
	}
	return false
}

func (w *Worker) finishInterruption(logger mlog.LoggerIFace, job *model.Job, reason stopReason) bool {
	if reason == workerStopped {
		// Cancellation can arrive after the worker last checked its channel.
		// A conditional update keeps that administrator decision intact.
		returnedToPending, err := w.finishJobStatus(job, model.JobStatusInProgress, model.JobStatusPending)
		if err != nil {
			logger.Error("Unable to return stopped access-control sync job to pending", mlog.Err(err))
			return true
		}
		if returnedToPending {
			return true
		}
		if _, err := w.finishJobStatus(job, model.JobStatusCancelRequested, model.JobStatusCanceled); err != nil {
			logger.Error("Unable to finish canceled access-control sync job", mlog.Err(err))
		}
		return true
	}
	if reason == jobCanceled {
		if _, err := w.finishJobStatus(job, model.JobStatusCancelRequested, model.JobStatusCanceled); err != nil {
			logger.Error("Unable to mark access-control sync job as canceled", mlog.Err(err))
		}
		return false
	}
	return false
}

func (w *Worker) finishJobStatus(job *model.Job, from, to string) (bool, error) {
	updated, err := w.jobServer.Store.Job().UpdateStatusOptimistically(job.Id, from, to)
	if err != nil || updated == nil {
		return false, err
	}
	if metrics := w.app.Srv().GetMetrics(); metrics != nil {
		metrics.DecrementJobActive(updated.Type)
	}
	// Mirror the JobServer status event, including its ABAC permission scope.
	jobJSON, err := json.Marshal(updated)
	if err != nil {
		return true, err
	}
	message := model.NewWebSocketEvent(model.WebsocketEventJobUpdated, "", "", "", nil, "")
	message.Add("job", string(jobJSON))
	message.GetBroadcast().ContainsSensitiveData = true
	permission := model.PermissionManageSystem
	if updated.Type == model.JobTypeAccessControlTeamSync {
		permission = model.PermissionManageTeamAccessRules
	}
	message.GetBroadcast().RequiredPermissions = []string{permission.Id}
	w.app.Publish(message)
	return true, nil
}

func (w *Worker) execute(rctx request.CTX, data map[string]string, cancelRequested <-chan struct{}) (stopReason, error) {
	if reason := w.stopRequested(cancelRequested); reason != continueWork {
		return reason, nil
	}
	w.results = make(map[string]*membershipSyncResult)
	if w.app.License() == nil {
		return continueWork, fmt.Errorf("access control is unavailable without an Enterprise license")
	}
	if !syncEnabled(w.app.Config(), w.jobType) {
		return continueWork, fmt.Errorf("attribute-based access control is disabled")
	}
	if w.app.Srv().Channels().AccessControl == nil {
		return continueWork, fmt.Errorf("access-control service is not initialized")
	}
	if err := w.jobServer.Store.Attributes().RefreshAttributes(); err != nil {
		return continueWork, fmt.Errorf("refresh access-control attributes: %w", err)
	}

	if w.jobType == model.JobTypeAccessControlTeamSync {
		return w.syncTeams(rctx, data, cancelRequested)
	}
	return w.syncChannels(rctx, data, cancelRequested)
}

func (w *Worker) syncChannels(rctx request.CTX, data map[string]string, cancelRequested <-chan struct{}) (stopReason, error) {
	policies, err := w.channelPolicies(rctx, data)
	if err != nil {
		return continueWork, err
	}
	for _, policy := range policies {
		if reason := w.stopRequested(cancelRequested); reason != continueWork {
			return reason, nil
		}
		if err := w.syncChannel(rctx, policy, cancelRequested); err != nil {
			return continueWork, err
		}
		if reason := w.stopRequested(cancelRequested); reason != continueWork {
			return reason, nil
		}
	}
	return continueWork, nil
}

func (w *Worker) channelPolicies(rctx request.CTX, data map[string]string) ([]*model.AccessControlPolicy, error) {
	if ids := splitIDs(data["channel_ids"]); len(ids) > 0 {
		return w.policiesByIDs(rctx, ids, model.AccessControlPolicyTypeChannel)
	}
	if policyID := data["policy_id"]; policyID != "" {
		policy, err := w.jobServer.Store.AccessControlPolicy().Get(rctx, policyID)
		if err != nil {
			return nil, fmt.Errorf("get access-control policy %s: %w", policyID, err)
		}
		switch policy.Type {
		case model.AccessControlPolicyTypeChannel:
			return []*model.AccessControlPolicy{policy}, nil
		case model.AccessControlPolicyTypeParent:
			return w.searchPolicies(rctx, model.AccessControlPolicySearch{Type: model.AccessControlPolicyTypeChannel, ParentID: policyID})
		default:
			return nil, fmt.Errorf("policy %s is not a channel or parent membership policy", policyID)
		}
	}
	return w.searchPolicies(rctx, model.AccessControlPolicySearch{Type: model.AccessControlPolicyTypeChannel, TeamID: data["team_id"]})
}

func (w *Worker) syncChannel(rctx request.CTX, policy *model.AccessControlPolicy, cancelRequested <-chan struct{}) error {
	actions, err := w.jobServer.Store.AccessControlPolicy().GetActionsForPolicy(rctx, policy.ID)
	if err != nil {
		return fmt.Errorf("get actions for channel policy %s: %w", policy.ID, err)
	}
	if !actions[model.AccessControlPolicyActionMembership] {
		return nil
	}
	channel, appErr := w.app.GetChannel(rctx, policy.ID)
	if appErr != nil {
		return fmt.Errorf("get channel %s: %w", policy.ID, appErr)
	}
	if appErr := w.app.ValidateChannelEligibilityForAccessControl(rctx, channel); appErr != nil {
		w.logger.Warn("Skipping ineligible channel during access-control sync", mlog.String("channel_id", channel.Id), mlog.Err(appErr))
		return nil
	}
	result := w.resultFor(channel.Id)

	var systemBot *model.Bot
	getSystemBot := func() (*model.Bot, error) {
		if systemBot != nil {
			return systemBot, nil
		}
		var botErr *model.AppError
		systemBot, botErr = w.app.GetSystemBot(rctx)
		if botErr != nil {
			return nil, botErr
		}
		return systemBot, nil
	}

	// Private channels are strict. Public-channel policies are advisory and
	// must never remove an existing member.
	if channel.Type == model.ChannelTypePrivate {
		members, appErr := w.app.Srv().Channels().AccessControl.GetChannelMembersToRemove(rctx, channel.Id)
		if appErr != nil {
			return fmt.Errorf("find channel members to remove from %s: %w", channel.Id, appErr)
		}
		for _, member := range members {
			if w.stopRequested(cancelRequested) != continueWork {
				return nil
			}
			bot, err := getSystemBot()
			if err != nil {
				return err
			}
			if appErr := w.app.RemoveUserFromChannel(rctx, member.UserId, bot.UserId, channel); appErr != nil {
				return fmt.Errorf("remove user %s from channel %s: %w", member.UserId, channel.Id, appErr)
			}
			result.MembersRemoved = append(result.MembersRemoved, member.UserId)
		}
	}

	// Enforcement and public recommendations remain enabled when auto-add is
	// off. The current server stores this setting on the membership rule rather
	// than in the legacy policy Active column.
	if !policy.AutoAddMembers() {
		return nil
	}
	cursor := model.SubjectCursor{}
	for {
		users, _, appErr := w.app.Srv().Channels().AccessControl.QueryUsersForResource(rctx, channel.Id, model.AccessControlPolicyActionMembership, model.SubjectSearchOptions{
			TeamID:                channel.TeamId,
			ExcludeChannelMembers: channel.Id,
			Limit:                 userPageSize,
			Cursor:                cursor,
			IgnoreCount:           true,
		})
		if appErr != nil {
			return fmt.Errorf("find users to add to channel %s: %w", channel.Id, appErr)
		}
		for _, user := range users {
			if w.stopRequested(cancelRequested) != continueWork {
				return nil
			}
			bot, err := getSystemBot()
			if err != nil {
				return err
			}
			if _, appErr := w.app.AddChannelMember(rctx, user.Id, channel, app.ChannelMemberOpts{UserRequestorID: bot.UserId}); appErr != nil {
				return fmt.Errorf("add user %s to channel %s: %w", user.Id, channel.Id, appErr)
			}
			result.MembersAdded = append(result.MembersAdded, user.Id)
		}
		if len(users) < userPageSize {
			break
		}
		cursor.TargetID = users[len(users)-1].Id
	}
	return nil
}

func (w *Worker) syncTeams(rctx request.CTX, data map[string]string, cancelRequested <-chan struct{}) (stopReason, error) {
	var policies []*model.AccessControlPolicy
	var err error
	if policyID := data["policy_id"]; policyID != "" {
		policies, err = w.policiesByIDs(rctx, []string{policyID}, model.AccessControlPolicyTypeTeam)
	} else {
		policies, err = w.searchPolicies(rctx, model.AccessControlPolicySearch{Type: model.AccessControlPolicyTypeTeam})
	}
	if err != nil {
		return continueWork, err
	}
	for _, policy := range policies {
		if reason := w.stopRequested(cancelRequested); reason != continueWork {
			return reason, nil
		}
		if err := w.syncTeam(rctx, policy, cancelRequested); err != nil {
			return continueWork, err
		}
		if reason := w.stopRequested(cancelRequested); reason != continueWork {
			return reason, nil
		}
	}
	return continueWork, nil
}

func (w *Worker) syncTeam(rctx request.CTX, policy *model.AccessControlPolicy, cancelRequested <-chan struct{}) error {
	actions, err := w.jobServer.Store.AccessControlPolicy().GetActionsForPolicy(rctx, policy.ID)
	if err != nil {
		return fmt.Errorf("get actions for team policy %s: %w", policy.ID, err)
	}
	if !actions[model.AccessControlPolicyActionMembership] {
		return nil
	}
	team, appErr := w.app.GetTeam(policy.ID)
	if appErr != nil {
		return fmt.Errorf("get team %s: %w", policy.ID, appErr)
	}
	if appErr := w.app.ValidateTeamEligibilityForAccessControl(rctx, team); appErr != nil {
		w.logger.Warn("Skipping ineligible team during access-control sync", mlog.String("team_id", team.Id), mlog.Err(appErr))
		return nil
	}
	result := w.resultFor(team.Id)
	var systemBot *model.Bot
	getSystemBot := func() (*model.Bot, error) {
		if systemBot != nil {
			return systemBot, nil
		}
		var botErr *model.AppError
		systemBot, botErr = w.app.GetSystemBot(rctx)
		if botErr != nil {
			return nil, botErr
		}
		return systemBot, nil
	}

	// Invite-only teams are strict. Open teams remain advisory and members are
	// never removed because of an ABAC mismatch.
	if teamIsStrict(team) {
		members, appErr := w.app.Srv().Channels().AccessControl.GetTeamMembersToRemove(rctx, team.Id)
		if appErr != nil {
			return fmt.Errorf("find team members to remove from %s: %w", team.Id, appErr)
		}
		if len(members) > 0 {
			count, err := w.jobServer.Store.Team().GetTotalMemberCount(team.Id, nil)
			if err != nil {
				return fmt.Errorf("count members of team %s: %w", team.Id, err)
			}
			result.MassRemovalWarning = int64(len(members))*2 > count
		}
		for _, member := range members {
			if w.stopRequested(cancelRequested) != continueWork {
				return nil
			}
			bot, err := getSystemBot()
			if err != nil {
				return err
			}
			// An empty requester marks this as a policy-driven removal in
			// Mattermost, including its channel-cascade audit records.
			if appErr := w.app.RemoveUserFromTeam(rctx, team.Id, member.UserId, ""); appErr != nil {
				return fmt.Errorf("remove user %s from team %s: %w", member.UserId, team.Id, appErr)
			}
			result.MembersRemoved = append(result.MembersRemoved, member.UserId)
			if notifyErr := w.app.SendTeamAccessControlRemovalNotification(rctx, bot, member.UserId, team); notifyErr != nil {
				w.logger.Warn("Unable to send team access-control removal notification", mlog.String("team_id", team.Id), mlog.String("user_id", member.UserId), mlog.Err(notifyErr))
			}
		}
	}
	if !policy.AutoAddMembers() {
		return nil
	}

	cursor := model.SubjectCursor{}
	for {
		users, _, appErr := w.app.Srv().Channels().AccessControl.QueryUsersForResource(rctx, team.Id, model.AccessControlPolicyActionMembership, model.SubjectSearchOptions{
			Limit:       userPageSize,
			Cursor:      cursor,
			IgnoreCount: true,
		})
		if appErr != nil {
			return fmt.Errorf("find users to add to team %s: %w", team.Id, appErr)
		}
		for _, user := range users {
			if w.stopRequested(cancelRequested) != continueWork {
				return nil
			}
			if member, err := w.jobServer.Store.Team().GetMember(rctx, team.Id, user.Id); err == nil && member.DeleteAt == 0 {
				continue
			} else if err != nil && !store.IsErrNotFound(err) {
				return fmt.Errorf("check membership of user %s in team %s: %w", user.Id, team.Id, err)
			}
			bot, err := getSystemBot()
			if err != nil {
				return err
			}
			if _, _, appErr := w.app.AddUserToTeam(rctx, team.Id, user.Id, bot.UserId); appErr != nil {
				return fmt.Errorf("add user %s to team %s: %w", user.Id, team.Id, appErr)
			}
			result.MembersAdded = append(result.MembersAdded, user.Id)
			if notifyErr := w.app.SendTeamAccessControlAdditionNotification(rctx, bot, user.Id, team); notifyErr != nil {
				w.logger.Warn("Unable to send team access-control addition notification", mlog.String("team_id", team.Id), mlog.String("user_id", user.Id), mlog.Err(notifyErr))
			}
		}
		if len(users) < userPageSize {
			break
		}
		cursor.TargetID = users[len(users)-1].Id
	}
	return nil
}

func teamIsStrict(team *model.Team) bool {
	// Mattermost's join and directory paths key privacy on
	// AllowOpenInvite. Team.Type can lag behind after a privacy update.
	return team != nil && !team.AllowOpenInvite
}

func (w *Worker) policiesByIDs(rctx request.CTX, ids []string, expectedType string) ([]*model.AccessControlPolicy, error) {
	policies := make([]*model.AccessControlPolicy, 0, len(ids))
	for _, id := range ids {
		policy, err := w.jobServer.Store.AccessControlPolicy().Get(rctx, id)
		if err != nil {
			return nil, fmt.Errorf("get access-control policy %s: %w", id, err)
		}
		if policy.Type != expectedType {
			return nil, fmt.Errorf("access-control policy %s has type %s instead of %s", id, policy.Type, expectedType)
		}
		policies = append(policies, policy)
	}
	return policies, nil
}

func (w *Worker) searchPolicies(rctx request.CTX, opts model.AccessControlPolicySearch) ([]*model.AccessControlPolicy, error) {
	result := []*model.AccessControlPolicy{}
	opts.Limit = policyPageSize
	for {
		policies, _, err := w.jobServer.Store.AccessControlPolicy().SearchPolicies(rctx, opts)
		if err != nil {
			return nil, fmt.Errorf("search access-control policies: %w", err)
		}
		result = append(result, policies...)
		if len(policies) < policyPageSize {
			break
		}
		opts.Cursor.ID = policies[len(policies)-1].ID
	}
	return result, nil
}

func splitIDs(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		id := strings.TrimSpace(part)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	return result
}

func (w *Worker) stopRequested(cancelRequested <-chan struct{}) stopReason {
	if w.interruption != continueWork {
		return w.interruption
	}
	select {
	case <-cancelRequested:
		w.interruption = jobCanceled
	case <-w.stop:
		w.interruption = workerStopped
	default:
	}
	return w.interruption
}
