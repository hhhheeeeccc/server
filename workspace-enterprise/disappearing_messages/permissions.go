package disappearing_messages

import (
	"sync"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

var permissionManageDisappearingMessages = &model.Permission{
	Id:          "manage_disappearing_messages",
	Name:        "Manage disappearing messages",
	Description: "Enable, disable, or change the timer in direct conversations",
	Scope:       model.PermissionScopeSystem,
}

var registerPermissionsOnce sync.Once

// RegisterPermissions extends the public permission catalog during startup,
// before role validation or server goroutines run. It never grants a role the
// permission: administrators control that through the native role editor.
func RegisterPermissions() {
	registerPermissionsOnce.Do(func() {
		model.AllPermissions = append(model.AllPermissions, permissionManageDisappearingMessages)
	})
}

func (s *Service) canManagePolicy(rctx request.CTX) bool {
	session := rctx.Session()
	return session != nil && (s.app.SessionHasPermissionTo(*session, permissionManageDisappearingMessages) ||
		s.app.SessionHasPermissionTo(*session, model.PermissionManageSystem))
}
