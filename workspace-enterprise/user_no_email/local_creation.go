package user_no_email

import (
	"net/http"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

// NewLocalCreator reuses the email compatibility flow without registering routes.
func NewLocalCreator(a *app.App) *Service {
	return &Service{app: a}
}

// CreateLocalUser preserves native account validation and permits an empty
// email on licensed installations. Callers must authorize account creation.
func (s *Service) CreateLocalUser(rctx request.CTX, user *model.User) (*model.User, *model.AppError) {
	if s.app.License() == nil {
		return nil, model.NewAppError("CreateLocalUser", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
	}
	if strings.TrimSpace(user.Email) != "" {
		return s.app.CreateUser(rctx, user)
	}
	if err := s.ensureEmailSchema(rctx.Context()); err != nil {
		return nil, model.NewAppError("CreateLocalUser", "app.user.save.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	user = user.DeepCopy()
	user.Email = "no-email-" + model.NewId() + "@users.invalid"
	user.EmailVerified = false
	user.DisableWelcomeEmail = true
	created, appErr := s.app.CreateUser(rctx, user)
	if appErr != nil {
		return nil, appErr
	}
	state, err := s.clearStoredEmail(created.Id, created.Email)
	if err != nil {
		return nil, model.NewAppError("CreateLocalUser", "app.user.save.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	s.app.InvalidateCacheForUser(created.Id)
	return state.apply(created), nil
}
