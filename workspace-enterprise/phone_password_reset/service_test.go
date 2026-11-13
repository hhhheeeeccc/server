package phone_password_reset

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
)

func TestIsLocalPasswordUserAllowsGuest(t *testing.T) {
	guest := &model.User{Roles: model.SystemGuestRoleId}
	if !guest.IsGuest() {
		t.Fatal("test user must be a guest")
	}
	if !isLocalPasswordUser(guest) {
		t.Fatal("a local guest with a password must be able to recover it")
	}

	guest.AuthService = model.UserAuthServiceMagicLink
	if isLocalPasswordUser(guest) {
		t.Fatal("a magic-link guest must not use password recovery")
	}
}
