package user_no_email

import "github.com/mattermost/mattermost/server/public/model"

// The client stores last-active sharing in user props. A missing value means
// the default (shared), so removal also counts as a change.
func lastActiveVisibilityChanged(before, after model.StringMap) bool {
	return before["show_last_active"] != after["show_last_active"]
}
