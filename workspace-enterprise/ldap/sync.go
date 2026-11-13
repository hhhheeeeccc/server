package ldap

import (
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	ejobs "github.com/mattermost/mattermost/server/v8/einterfaces/jobs"
)

type SyncJob struct{ service *Service }

var _ ejobs.LdapSyncInterface = (*SyncJob)(nil)
var _ ejobs.Scheduler = (*SyncJob)(nil)

func NewSyncJob(a *app.App) ejobs.LdapSyncInterface { return &SyncJob{service: newService(a)} }

func (s *Service) GetAllLdapUsers(rctx request.CTX) ([]*model.User, *model.AppError) {
	users, _, appErr := s.allLdapUsersAndAttributes(rctx, nil)
	return users, appErr
}

func (s *Service) allLdapUsersAndAttributes(rctx request.CTX, fields []ldapMappedField) ([]*model.User, map[string]map[string][]string, *model.AppError) {
	cfg, appErr := s.userSettings(false)
	if appErr != nil {
		return nil, nil, appErr
	}
	d, entries, appErr := s.userEntries(rctx, cfg, "", "", appendLDAPMappedAttributes(userAttributes(cfg), fields))
	if appErr != nil {
		return nil, nil, appErr
	}
	defer d.close()
	users := make([]*model.User, 0, len(entries))
	attributes := make(map[string]map[string][]string, len(entries))
	seen := map[string]bool{}
	usernames := map[string]bool{}
	emails := map[string]bool{}
	for _, entry := range entries {
		id := entryID(entry, *cfg.IdAttribute)
		if id == "" || seen[id] {
			return nil, nil, ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
		}
		seen[id] = true
		if directoryDisabled(entry) {
			continue
		}
		u, appErr := mappedUser(entry, cfg)
		if appErr != nil {
			return nil, nil, appErr
		}
		// A later unique-index failure would otherwise leave earlier users in
		// this snapshot updated. Reject duplicate target identities up front.
		if usernames[u.Username] || emails[u.Email] {
			return nil, nil, ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict)
		}
		usernames[u.Username] = true
		emails[u.Email] = true
		users = append(users, u)
		values := make(map[string][]string, len(fields))
		for _, field := range fields {
			values[field.attribute] = attributeValues(entry, field.attribute)
		}
		attributes[id] = values
	}
	return users, attributes, nil
}

// Never infer deletion from an incomplete/failed directory search. Both user and
// group snapshots must be validated before changing any local accounts.
func (s *Service) synchronize(rctx request.CTX, reAdd bool) *model.AppError {
	cfg, appErr := s.userSettings(false)
	if appErr != nil {
		return appErr
	}
	if !*cfg.EnableSync {
		return ldapError("ent.ldap.disabled.app_error", http.StatusForbidden)
	}
	samlCfg := s.app.Config().SamlSettings
	if model.SafeDereference(samlCfg.EnableSyncWithLdap) {
		license := s.app.License()
		if license == nil || license.Features == nil || !model.SafeDereference(license.Features.SAML) {
			return ldapError("ent.ldap.disabled.app_error", http.StatusForbidden)
		}
	}
	// Serialize snapshots and writes across job workers on all cluster nodes.
	release, err := s.acquireSyncLock(rctx.Context())
	if err != nil {
		return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
	}
	defer release()
	textFields, appErr := s.ldapMappedFields(rctx)
	if appErr != nil {
		return appErr
	}
	directoryUsers, directoryAttributes, appErr := s.allLdapUsersAndAttributes(rctx, textFields)
	if appErr != nil {
		return appErr
	}
	// Reject invalid mapped options across the entire directory snapshot before
	// changing any local account or SAML binding.
	for _, user := range directoryUsers {
		if err := rctx.Context().Err(); err != nil {
			return ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
		}
		if appErr = validateLDAPCustomAttributes(directoryAttributes[*user.AuthData], textFields); appErr != nil {
			return appErr
		}
	}
	users, err := s.app.Srv().Store().User().GetAllUsingAuthService(model.UserAuthServiceLdap)
	if err != nil {
		return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
	}
	byID := map[string]*model.User{}
	remoteIDs := map[string]string{}
	for _, user := range directoryUsers {
		byID[*user.AuthData] = user
	}
	for _, user := range users {
		remoteIDs[user.Id] = model.SafeDereference(user.AuthData)
	}
	var samlUsers []*model.User
	byEmail := map[string]*model.User{}
	if model.SafeDereference(samlCfg.EnableSyncWithLdap) {
		for _, user := range directoryUsers {
			key := strings.ToLower(user.Email)
			if key == "" || byEmail[key] != nil {
				return ldapError("ent.ldap.invalid_mapping.app_error", http.StatusConflict)
			}
			byEmail[key] = user
		}
		samlUsers, err = s.app.Srv().Store().User().GetAllUsingAuthService(model.UserAuthServiceSaml)
		if err != nil {
			return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
		}
		if model.SafeDereference(samlCfg.EnableSyncWithLdapIncludeAuth) && model.SafeDereference(samlCfg.IdAttribute) != "" {
			for _, user := range samlUsers {
				idMatch := byID[model.SafeDereference(user.AuthData)]
				emailMatch := byEmail[strings.ToLower(user.Email)]
				if idMatch != nil && emailMatch != nil && idMatch != emailMatch {
					return ldapError("ent.ldap.account_conflict.app_error", http.StatusConflict)
				}
			}
		}
		for _, user := range samlUsers {
			if model.SafeDereference(samlCfg.IgnoreGuestsLdapSync) && user.IsGuest() {
				continue
			}
			if mapped := mappedSAMLUser(user, samlCfg, byID, byEmail); mapped != nil {
				remoteIDs[user.Id] = *mapped.AuthData
			}
		}
	}
	if appErr = s.preflightIdentityOwnership(rctx, users, samlUsers, byID, byEmail, samlCfg); appErr != nil {
		return appErr
	}
	allUsers := append(append([]*model.User{}, users...), samlUsers...)
	groupPlan, appErr := s.planGroupMemberships(rctx, cfg, allUsers, byID, remoteIDs, model.SafeDereference(samlCfg.EnableSyncWithLdap))
	if appErr != nil {
		return appErr
	}
	if model.SafeDereference(samlCfg.EnableSyncWithLdap) && model.SafeDereference(samlCfg.EnableSyncWithLdapIncludeAuth) {
		if appErr = s.migrateSAMLBindings(rctx, cfg, samlCfg, samlUsers, byID, byEmail); appErr != nil {
			return appErr
		}
	}
	for _, user := range users {
		if err := rctx.Context().Err(); err != nil {
			return ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
		}
		if _, appErr = s.userSettings(false); appErr != nil {
			return appErr
		}
		if !reflect.DeepEqual(cfg, s.app.Config().LdapSettings) {
			return ldapError("ent.ldap.sync_config_changed.app_error", http.StatusConflict)
		}
		mapped := byID[model.SafeDereference(user.AuthData)]
		if mapped == nil {
			if user.DeleteAt == 0 {
				if appErr = s.app.UpdateUserActive(rctx, user.Id, false); appErr != nil {
					return appErr
				}
			}
			continue
		}
		if _, appErr = s.updateMappedUser(rctx, cfg, user, mapped); appErr != nil {
			return appErr
		}
		if appErr = s.syncCustomAttributes(rctx, user.Id, directoryAttributes[*mapped.AuthData], textFields); appErr != nil {
			return appErr
		}
		if user.DeleteAt != 0 {
			if appErr = s.app.UpdateUserActive(rctx, user.Id, true); appErr != nil {
				return appErr
			}
		}
	}
	for _, user := range samlUsers {
		if model.SafeDereference(samlCfg.IgnoreGuestsLdapSync) && user.IsGuest() {
			continue
		}
		if err := rctx.Context().Err(); err != nil {
			return ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
		}
		if _, appErr = s.userSettings(false); appErr != nil {
			return appErr
		}
		if !reflect.DeepEqual(cfg, s.app.Config().LdapSettings) || !reflect.DeepEqual(samlCfg, s.app.Config().SamlSettings) {
			return ldapError("ent.ldap.sync_config_changed.app_error", http.StatusConflict)
		}
		mapped := mappedSAMLUser(user, samlCfg, byID, byEmail)
		if mapped == nil {
			if user.DeleteAt == 0 {
				if appErr = s.app.UpdateUserActive(rctx, user.Id, false); appErr != nil {
					return appErr
				}
			}
			continue
		}
		if _, appErr = s.updateMappedUser(rctx, cfg, user, mapped); appErr != nil {
			return appErr
		}
		if appErr = s.syncCustomAttributes(rctx, user.Id, directoryAttributes[*mapped.AuthData], textFields); appErr != nil {
			return appErr
		}
		if user.DeleteAt != 0 {
			if appErr = s.app.UpdateUserActive(rctx, user.Id, true); appErr != nil {
				return appErr
			}
		}
	}
	if appErr = s.applyGroupMemberships(rctx, groupPlan); appErr != nil {
		return appErr
	}
	if len(groupPlan) != 0 {
		if rctx.Context().Err() != nil {
			return ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
		}
		if err = s.app.DeleteGroupConstrainedMemberships(rctx); err != nil {
			return ldapError("ent.ldap.sync_failed.app_error", http.StatusInternalServerError)
		}
		if err = s.app.CreateDefaultMemberships(rctx, model.CreateDefaultMembershipParams{ReAddRemovedMembers: reAdd}); err != nil {
			return ldapError("ent.ldap.sync_failed.app_error", http.StatusInternalServerError)
		}
		if rctx.Context().Err() != nil {
			return ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
		}
		if appErr = s.syncGroupRoles(rctx, groupPlan); appErr != nil {
			return appErr
		}
	}
	return nil
}

func (s *Service) FirstLoginSync(rctx request.CTX, user *model.User) *model.AppError {
	cfg, appErr := s.userSettings(false)
	if appErr != nil {
		return appErr
	}
	if user == nil || !user.IsLDAPUser() || user.AuthData == nil {
		return ldapError("ent.ldap.invalid_mapping.app_error", http.StatusBadRequest)
	}
	// Coordinate login-time membership writes with scheduled snapshots on other nodes.
	release, err := s.acquireSyncLock(rctx.Context())
	if err != nil {
		return ldapError("ent.ldap.store.app_error", http.StatusInternalServerError)
	}
	defer release()
	plans, appErr := s.planGroupMemberships(rctx, cfg, []*model.User{user}, map[string]*model.User{*user.AuthData: user}, map[string]string{user.Id: *user.AuthData}, false)
	if appErr != nil {
		return appErr
	}
	// Restrict deletions to this user; other members haven't been included in this snapshot.
	for i := range plans {
		for id := range plans[i].previous {
			if id != user.Id {
				delete(plans[i].previous, id)
			}
		}
	}
	if appErr = s.applyGroupMemberships(rctx, plans); appErr != nil {
		return appErr
	}
	if len(plans) != 0 {
		if rctx.Context().Err() != nil {
			return ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
		}
		if err := s.app.CreateDefaultMemberships(rctx, model.CreateDefaultMembershipParams{ScopedUserID: &user.Id}); err != nil {
			return ldapError("ent.ldap.sync_failed.app_error", http.StatusInternalServerError)
		}
		if rctx.Context().Err() != nil {
			return ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
		}
		if appErr = s.syncGroupRoles(rctx, plans); appErr != nil {
			return appErr
		}
	}
	return nil
}

func (s *Service) StartSynchronizeJob(rctx request.CTX, wait bool) (*model.Job, *model.AppError) {
	cfg, appErr := s.userSettings(false)
	if appErr != nil {
		return nil, appErr
	}
	if !*cfg.EnableSync {
		return nil, ldapError("ent.ldap.disabled.app_error", http.StatusForbidden)
	}
	job, appErr := s.app.Srv().Jobs.CreateJob(rctx, model.JobTypeLdapSync, nil)
	if appErr != nil || !wait {
		return job, appErr
	}
	// Use the same claim/status machinery as the background worker.
	worker := (&SyncJob{service: s}).MakeWorker().(*syncWorker)
	worker.DoJob(job)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, appErr = s.app.Srv().Jobs.GetJob(rctx, job.Id)
		if appErr != nil {
			return nil, appErr
		}
		switch job.Status {
		case model.JobStatusSuccess:
			return job, nil
		case model.JobStatusError, model.JobStatusCanceled:
			return job, ldapError("ent.ldap.sync_failed.app_error", http.StatusInternalServerError)
		}
		select {
		case <-rctx.Context().Done():
			return job, ldapError("ent.ldap.sync_failed.app_error", http.StatusRequestTimeout)
		case <-ticker.C:
		}
	}
}

func (j *SyncJob) MakeWorker() model.Worker {
	return newSyncWorker(j.service, j.Enabled)
}

func (j *SyncJob) MakeScheduler() ejobs.Scheduler { return j }
func (j *SyncJob) Enabled(cfg *model.Config) bool {
	l := j.service.app.License()
	return l != nil && l.Features != nil && model.SafeDereference(l.Features.LDAP) && model.SafeDereference(cfg.LdapSettings.EnableSync)
}
func (j *SyncJob) NextScheduleTime(cfg *model.Config, now time.Time, pending bool, last *model.Job) *time.Time {
	if !j.Enabled(cfg) || pending || model.SafeDereference(cfg.LdapSettings.SyncIntervalMinutes) <= 0 {
		return nil
	}
	period := time.Duration(*cfg.LdapSettings.SyncIntervalMinutes) * time.Minute
	next := now.Add(period)
	if last != nil && last.LastActivityAt > 0 {
		next = time.UnixMilli(last.LastActivityAt).Add(period)
		if next.Before(now) {
			next = now
		}
	}
	return &next
}
func (j *SyncJob) ScheduleJob(rctx request.CTX, cfg *model.Config, pending bool, _ *model.Job) (*model.Job, *model.AppError) {
	if !j.Enabled(cfg) || pending {
		return nil, nil
	}
	return j.service.StartSynchronizeJob(rctx, false)
}
