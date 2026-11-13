package notification

import (
	"net/http"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/i18n"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

const arabicIDLoadedMessage = "لقد تلقيت رسالة جديدة."

type Service struct {
	app *app.App
}

func New(a *app.App) *Service {
	return &Service{app: a}
}

func (s *Service) CheckLicense() *model.AppError {
	license := s.app.License()
	if license == nil || license.Features == nil || !model.SafeDereference(license.Features.IDLoadedPushNotifications) {
		return model.NewAppError("Notification.CheckLicense", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
	}
	return nil
}

func (s *Service) GetNotificationMessage(rctx request.CTX, ack *model.PushNotificationAck, userID string) (*model.PushNotification, *model.AppError) {
	if appErr := s.CheckLicense(); appErr != nil {
		return nil, appErr
	}
	if ack == nil || !model.IsValidId(ack.PostId) || !model.IsValidId(userID) {
		return nil, model.NewAppError("Notification.GetNotificationMessage", "api.push_notifications_ack.message.parse.app_error", nil, "", http.StatusBadRequest)
	}

	user, appErr := s.app.GetUser(rctx, userID)
	if appErr != nil {
		return nil, appErr
	}
	post, appErr := s.app.GetSinglePost(rctx, ack.PostId, false)
	if appErr != nil {
		return nil, appErr
	}
	channel, appErr := s.app.GetChannel(rctx, post.ChannelId)
	if appErr != nil {
		return nil, appErr
	}

	message := i18n.GetUserTranslations(user.Locale)("api.push_notification.id_loaded.default_message")
	if strings.HasPrefix(strings.ToLower(user.Locale), "ar") || strings.HasPrefix(strings.ToLower(rctx.AcceptLanguage()), "ar") {
		message = arabicIDLoadedMessage
	}

	return &model.PushNotification{
		AckId:        ack.Id,
		PostId:       post.Id,
		ChannelId:    post.ChannelId,
		RootId:       post.RootId,
		IsCRTEnabled: s.app.IsCRTEnabledForUser(rctx, user.Id),
		Category:     model.CategoryCanReply,
		Version:      model.PushMessageV2,
		TeamId:       channel.TeamId,
		Type:         model.PushTypeMessage,
		IsIdLoaded:   true,
		SenderId:     post.UserId,
		Message:      message,
	}, nil
}
