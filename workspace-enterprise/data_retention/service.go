package data_retention

import (
	"database/sql"
	"errors"
	"math"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

type Service struct {
	app *app.App
}

var _ einterfaces.DataRetentionInterface = (*Service)(nil)

func New(a *app.App) einterfaces.DataRetentionInterface {
	return &Service{app: a}
}

func (s *Service) GetGlobalPolicy() (*model.GlobalRetentionPolicy, *model.AppError) {
	if appErr := s.requireLicense("GetGlobalPolicy"); appErr != nil {
		return nil, appErr
	}

	cfg := s.app.Config()
	policy := &model.GlobalRetentionPolicy{
		MessageDeletionEnabled: model.SafeDereference(cfg.DataRetentionSettings.EnableMessageDeletion),
		FileDeletionEnabled:    model.SafeDereference(cfg.DataRetentionSettings.EnableFileDeletion),
	}

	now := model.GetMillis()
	if policy.MessageDeletionEnabled {
		cutoff, err := retentionCutoff(now, cfg.DataRetentionSettings.MessageRetentionHours,
			cfg.DataRetentionSettings.MessageRetentionDays, model.DataRetentionSettingsDefaultMessageRetentionDays)
		if err != nil {
			return nil, s.internalError("GetGlobalPolicy", err)
		}
		policy.MessageRetentionCutoff = cutoff
	}
	if policy.FileDeletionEnabled {
		cutoff, err := retentionCutoff(now, cfg.DataRetentionSettings.FileRetentionHours,
			cfg.DataRetentionSettings.FileRetentionDays, model.DataRetentionSettingsDefaultFileRetentionDays)
		if err != nil {
			return nil, s.internalError("GetGlobalPolicy", err)
		}
		policy.FileRetentionCutoff = cutoff
	}

	return policy, nil
}

// A cutoff before the Unix epoch cannot match any retained item. Clamp it to
// zero, and reject durations that cannot be represented instead of wrapping
// into a recent or future cutoff that could delete the wrong data.
func retentionCutoff(now int64, hours, days *int, defaultDays int) (int64, error) {
	const millisPerHour int64 = 60 * 60 * 1000
	const millisPerDay int64 = 24 * millisPerHour
	unit, count := millisPerDay, int64(defaultDays)
	if days != nil && *days > 0 {
		count = int64(*days)
	}
	if hours != nil && *hours > 0 {
		unit, count = millisPerHour, int64(*hours)
	}
	if count < 0 || count > math.MaxInt64/unit {
		return 0, errors.New("data retention duration exceeds supported range")
	}
	duration := count * unit
	if duration >= now {
		return 0, nil
	}
	return now - duration, nil
}

func (s *Service) GetPolicies(offset, limit int) (*model.RetentionPolicyWithTeamAndChannelCountsList, *model.AppError) {
	if appErr := s.requireLicense("GetPolicies"); appErr != nil {
		return nil, appErr
	}

	policies, err := s.app.Srv().Store().RetentionPolicy().GetAll(offset, limit)
	if err != nil {
		return nil, s.internalError("GetPolicies", err)
	}
	count, err := s.app.Srv().Store().RetentionPolicy().GetCount()
	if err != nil {
		return nil, s.internalError("GetPolicies", err)
	}

	return &model.RetentionPolicyWithTeamAndChannelCountsList{Policies: policies, TotalCount: count}, nil
}

func (s *Service) GetPoliciesCount() (int64, *model.AppError) {
	if appErr := s.requireLicense("GetPoliciesCount"); appErr != nil {
		return 0, appErr
	}

	count, err := s.app.Srv().Store().RetentionPolicy().GetCount()
	if err != nil {
		return 0, s.internalError("GetPoliciesCount", err)
	}
	return count, nil
}

func (s *Service) GetPolicy(policyID string) (*model.RetentionPolicyWithTeamAndChannelCounts, *model.AppError) {
	if appErr := s.requireLicense("GetPolicy"); appErr != nil {
		return nil, appErr
	}

	policy, err := s.app.Srv().Store().RetentionPolicy().Get(policyID)
	if err != nil {
		return nil, s.policyStoreError("GetPolicy", err)
	}
	return policy, nil
}

func (s *Service) CreatePolicy(policy *model.RetentionPolicyWithTeamAndChannelIDs) (*model.RetentionPolicyWithTeamAndChannelCounts, *model.AppError) {
	if appErr := s.requireLicense("CreatePolicy"); appErr != nil {
		return nil, appErr
	}

	created, err := s.app.Srv().Store().RetentionPolicy().Save(policy)
	if err != nil {
		return nil, s.internalError("CreatePolicy", err)
	}
	return created, nil
}

func (s *Service) PatchPolicy(patch *model.RetentionPolicyWithTeamAndChannelIDs) (*model.RetentionPolicyWithTeamAndChannelCounts, *model.AppError) {
	if appErr := s.requireLicense("PatchPolicy"); appErr != nil {
		return nil, appErr
	}

	updated, err := s.app.Srv().Store().RetentionPolicy().Patch(patch)
	if err != nil {
		return nil, s.policyStoreError("PatchPolicy", err)
	}
	return updated, nil
}

func (s *Service) DeletePolicy(policyID string) *model.AppError {
	if appErr := s.requireLicense("DeletePolicy"); appErr != nil {
		return appErr
	}
	if err := s.app.Srv().Store().RetentionPolicy().Delete(policyID); err != nil {
		return s.policyStoreError("DeletePolicy", err)
	}
	return nil
}

func (s *Service) GetTeamsForPolicy(policyID string, offset, limit int) (*model.TeamsWithCount, *model.AppError) {
	if appErr := s.requireLicense("GetTeamsForPolicy"); appErr != nil {
		return nil, appErr
	}

	teams, err := s.app.Srv().Store().RetentionPolicy().GetTeams(policyID, offset, limit)
	if err != nil {
		return nil, s.internalError("GetTeamsForPolicy", err)
	}
	count, err := s.app.Srv().Store().RetentionPolicy().GetTeamsCount(policyID)
	if err != nil {
		return nil, s.internalError("GetTeamsForPolicy", err)
	}
	return &model.TeamsWithCount{Teams: teams, TotalCount: count}, nil
}

func (s *Service) AddTeamsToPolicy(policyID string, teamIDs []string) *model.AppError {
	if appErr := s.requireLicense("AddTeamsToPolicy"); appErr != nil {
		return appErr
	}
	if err := s.app.Srv().Store().RetentionPolicy().AddTeams(policyID, teamIDs); err != nil {
		return s.internalError("AddTeamsToPolicy", err)
	}
	return nil
}

func (s *Service) RemoveTeamsFromPolicy(policyID string, teamIDs []string) *model.AppError {
	if appErr := s.requireLicense("RemoveTeamsFromPolicy"); appErr != nil {
		return appErr
	}
	if err := s.app.Srv().Store().RetentionPolicy().RemoveTeams(policyID, teamIDs); err != nil {
		return s.internalError("RemoveTeamsFromPolicy", err)
	}
	return nil
}

func (s *Service) GetChannelsForPolicy(policyID string, offset, limit int) (*model.ChannelsWithCount, *model.AppError) {
	if appErr := s.requireLicense("GetChannelsForPolicy"); appErr != nil {
		return nil, appErr
	}

	channels, err := s.app.Srv().Store().RetentionPolicy().GetChannels(policyID, offset, limit)
	if err != nil {
		return nil, s.internalError("GetChannelsForPolicy", err)
	}
	count, err := s.app.Srv().Store().RetentionPolicy().GetChannelsCount(policyID)
	if err != nil {
		return nil, s.internalError("GetChannelsForPolicy", err)
	}
	return &model.ChannelsWithCount{Channels: channels, TotalCount: count}, nil
}

func (s *Service) AddChannelsToPolicy(policyID string, channelIDs []string) *model.AppError {
	if appErr := s.requireLicense("AddChannelsToPolicy"); appErr != nil {
		return appErr
	}
	if err := s.app.Srv().Store().RetentionPolicy().AddChannels(policyID, channelIDs); err != nil {
		return s.internalError("AddChannelsToPolicy", err)
	}
	return nil
}

func (s *Service) RemoveChannelsFromPolicy(policyID string, channelIDs []string) *model.AppError {
	if appErr := s.requireLicense("RemoveChannelsFromPolicy"); appErr != nil {
		return appErr
	}
	if err := s.app.Srv().Store().RetentionPolicy().RemoveChannels(policyID, channelIDs); err != nil {
		return s.internalError("RemoveChannelsFromPolicy", err)
	}
	return nil
}

func (s *Service) GetTeamPoliciesForUser(userID string, offset, limit int) (*model.RetentionPolicyForTeamList, *model.AppError) {
	if appErr := s.requireLicense("GetTeamPoliciesForUser"); appErr != nil {
		return nil, appErr
	}

	policies, err := s.app.Srv().Store().RetentionPolicy().GetTeamPoliciesForUser(userID, offset, limit)
	if err != nil {
		return nil, s.internalError("GetTeamPoliciesForUser", err)
	}
	count, err := s.app.Srv().Store().RetentionPolicy().GetTeamPoliciesCountForUser(userID)
	if err != nil {
		return nil, s.internalError("GetTeamPoliciesForUser", err)
	}
	return &model.RetentionPolicyForTeamList{Policies: policies, TotalCount: count}, nil
}

func (s *Service) GetChannelPoliciesForUser(userID string, offset, limit int) (*model.RetentionPolicyForChannelList, *model.AppError) {
	if appErr := s.requireLicense("GetChannelPoliciesForUser"); appErr != nil {
		return nil, appErr
	}

	policies, err := s.app.Srv().Store().RetentionPolicy().GetChannelPoliciesForUser(userID, offset, limit)
	if err != nil {
		return nil, s.internalError("GetChannelPoliciesForUser", err)
	}
	count, err := s.app.Srv().Store().RetentionPolicy().GetChannelPoliciesCountForUser(userID)
	if err != nil {
		return nil, s.internalError("GetChannelPoliciesForUser", err)
	}
	return &model.RetentionPolicyForChannelList{Policies: policies, TotalCount: count}, nil
}

func (s *Service) requireLicense(method string) *model.AppError {
	license := s.app.License()
	if license != nil && license.Features != nil && model.SafeDereference(license.Features.DataRetention) {
		return nil
	}
	return model.NewAppError("data_retention."+method, "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
}

func (s *Service) internalError(method string, err error) *model.AppError {
	return model.NewAppError("data_retention."+method, "ent.data_retention.policies.internal_error", nil, "", http.StatusInternalServerError).Wrap(err)
}

func (s *Service) policyStoreError(method string, err error) *model.AppError {
	// Get and Patch return sql.ErrNoRows. The core store's Delete currently
	// returns an untyped "policy not found" error for zero affected rows.
	if errors.Is(err, sql.ErrNoRows) || err.Error() == "policy not found" {
		return model.NewAppError("data_retention."+method, "api.context.404.app_error", nil, "", http.StatusNotFound)
	}
	return s.internalError(method, err)
}
