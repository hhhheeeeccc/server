package channel_leave_policy

import (
	"strings"
	"sync"
	"unicode"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/i18n"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	_ "github.com/mattermost/mattermost/server/v8/channels/app/slashcommands" // initialize built-in providers before wrapping
)

var registerOnce sync.Once

// Arabic aliases resolve their delegates by trigger, so they share this guard.
func RegisterCommands() {
	registerOnce.Do(func() {
		for _, trigger := range []string{"leave", "remove", "kick"} {
			if original := app.GetCommandProvider(trigger); original != nil {
				app.RegisterCommandProvider(&leaveProvider{original: original, check: checkCommand})
			}
		}
	})
}

type leaveProvider struct {
	original app.CommandProvider
	check    func(*app.App, request.CTX, *model.CommandArgs, string, string) *model.AppError
}

func (p *leaveProvider) GetTrigger() string { return p.original.GetTrigger() }

func (p *leaveProvider) GetCommand(a *app.App, t i18n.TranslateFunc) *model.Command {
	return p.original.GetCommand(a, t)
}

func (p *leaveProvider) DoCommand(a *app.App, rctx request.CTX, args *model.CommandArgs, message string) *model.CommandResponse {
	if err := p.check(a, rctx, args, p.GetTrigger(), message); err != nil {
		text := err.Message
		if args != nil && args.T != nil {
			text = args.T(err.Id)
		}
		return &model.CommandResponse{Text: text, ResponseType: model.CommandResponseTypeEphemeral}
	}
	return p.original.DoCommand(a, rctx, args, message)
}

func checkCommand(a *app.App, rctx request.CTX, args *model.CommandArgs, trigger, message string) *model.AppError {
	if a == nil || args == nil {
		return denied()
	}
	return checkInvocation(a, rctx, args.UserId, args.ChannelId, trigger, message)
}

func splitCommand(command string) (string, string) {
	if !strings.HasPrefix(command, "/") {
		return "", ""
	}
	trigger, message := command, ""
	if index := strings.IndexFunc(command, unicode.IsSpace); index >= 0 {
		trigger, message = command[:index], command[index+1:]
	}
	return strings.TrimPrefix(strings.ToLower(trigger), "/"), message
}

func checkInvocation(a application, rctx request.CTX, userID, channelID, trigger, message string) *model.AppError {
	switch trigger {
	case "leave", "مغادرة":
		return check(a, rctx, userID, channelID)
	case "remove", "kick", "إزالة", "طرد":
		// Mirror the built-in parser: the first space-delimited username,
		// with one optional @. Username lookup in core is case insensitive.
		target := strings.TrimPrefix(strings.SplitN(message, " ", 2)[0], "@")
		if target == "" {
			return nil // original provider reports missing arguments
		}
		user, err := a.GetUser(rctx, userID)
		if err != nil {
			return err
		}
		if strings.EqualFold(target, user.Username) {
			return check(a, rctx, userID, channelID)
		}
	}
	return nil // removal of somebody else retains original permissions
}
