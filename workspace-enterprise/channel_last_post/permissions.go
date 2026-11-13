package channel_last_post

import (
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

// A user's normal channel list consists of memberships. The native batch check
// reads the cached membership map once instead of decoding the entire map for
// every channel, which otherwise grows quadratically with the channel count.
// Its system-role shortcuts are broader than the native content-read check:
// only accept the batch result when it must have checked channel memberships.
// Public nonmembers, partial grants, and privileged sessions retain the exact
// native content permission checks below.
func (s *Service) readablePreviewChannelIDs(rctx request.CTX, channelIDs []string) []string {
	session := *rctx.Session()
	if len(channelIDs) > 1 &&
		!s.app.SessionHasPermissionTo(session, model.PermissionManageSystem) &&
		!s.app.SessionHasPermissionTo(session, model.PermissionReadChannelContent) &&
		s.app.SessionHasPermissionToChannels(rctx, session, channelIDs, model.PermissionReadChannelContent) {
		return channelIDs
	}

	readable := make([]string, 0, len(channelIDs))
	for _, id := range channelIDs {
		// Discovery alone must not expose messages. This preserves compliance
		// mode, guests, and the native public-channel fallback.
		channel, appErr := s.app.GetChannel(rctx, id)
		if appErr != nil {
			continue
		}
		if allowed, _ := s.app.SessionHasPermissionToReadChannel(rctx, session, channel); allowed {
			readable = append(readable, id)
		}
	}
	return readable
}
