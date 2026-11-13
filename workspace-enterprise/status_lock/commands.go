package status_lock

import (
	"sync"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/i18n"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

var registerCommandsOnce sync.Once

// RegisterCommands protects the built-in presence commands. Arabic aliases
// delegate to these same providers, so both command spellings share the lock.
func RegisterCommands() {
	registerCommandsOnce.Do(func() {
		for _, trigger := range []string{"away", "dnd", "offline", "online"} {
			if original := app.GetCommandProvider(trigger); original != nil {
				app.RegisterCommandProvider(&lockedPresenceCommand{original: original})
			}
		}
	})
}

type lockedPresenceCommand struct {
	original app.CommandProvider
}

var _ app.CommandProvider = (*lockedPresenceCommand)(nil)

func (p *lockedPresenceCommand) GetTrigger() string { return p.original.GetTrigger() }

func (p *lockedPresenceCommand) GetCommand(a *app.App, translate i18n.TranslateFunc) *model.Command {
	return p.original.GetCommand(a, translate)
}

func (p *lockedPresenceCommand) DoCommand(a *app.App, rctx request.CTX, args *model.CommandArgs, message string) *model.CommandResponse {
	if args != nil && presenceLocked(a) && !canEditPresence(a, rctx, args.UserId) {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         args.T("api.user.update_user.personal_data_locked.app_error"),
		}
	}
	return p.original.DoCommand(a, rctx, args, message)
}

func presenceLocked(a *app.App) bool {
	license := a.License()
	return license != nil && license.Features != nil &&
		model.SafeDereference(license.Features.LockTeammateNameDisplay) &&
		model.SafeDereference(a.Config().TeamSettings.LockTeammateNameDisplay)
}

func canEditPresence(a *app.App, rctx request.CTX, targetID string) bool {
	session := rctx.Session()
	return session != nil && (a.SessionHasPermissionTo(*session, model.PermissionManageSystem) ||
		userManagerCanEditStatus(a, rctx, targetID))
}
