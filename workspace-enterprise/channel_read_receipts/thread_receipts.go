package channel_read_receipts

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

// Observe only successful native thread-read updates. Opening a channel is not
// evidence that collapsed replies were viewed. Keep native authentication,
// authorization and thread read-cursor handling in the original handler.
func (s *Service) publishThreadMemberViewed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, threadID, ok := threadReadRequest(r, s.app.Config())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		capture := &statusResponseWriter{ResponseWriter: w}
		next.ServeHTTP(capture, r)
		if capture.statusCode < 200 || capture.statusCode >= 300 || s.app.License() == nil || !*s.app.Config().ServiceSettings.EnableChannelViewedMessages {
			return
		}
		session, ok := s.resolveSession(r)
		if !ok {
			return
		}
		if userID == model.Me {
			userID = session.UserId
		}
		rctx := request.EmptyContext(s.app.Log()).WithContext(r.Context()).With(app.RequestContextWithMaster)
		post, appErr := s.app.GetSinglePost(rctx, threadID, false)
		if appErr != nil {
			return
		}
		var viewedAt int64
		err := s.app.Srv().Store().GetInternalMasterDB().QueryRowContext(r.Context(), `
            SELECT tm.lastviewed FROM threadmemberships tm
            JOIN channelmembers cm ON cm.userid=tm.userid AND cm.channelid=$3
            WHERE tm.postid=$1 AND tm.userid=$2`, threadID, userID, post.ChannelId).Scan(&viewedAt)
		if err != nil {
			return
		}
		event := model.NewWebSocketEvent(channelMemberViewedEvent, "", post.ChannelId, "", nil, "")
		event.Add("channel_id", post.ChannelId)
		event.Add("thread_id", threadID)
		event.Add("user_id", userID)
		event.Add("last_viewed_at", viewedAt)
		s.app.Publish(event)
	})
}

func threadReadRequest(r *http.Request, cfg *model.Config) (string, string, bool) {
	if r.Method != http.MethodPut {
		return "", "", false
	}
	subpath, err := utils.GetSubpathFromConfig(cfg)
	if err != nil {
		return "", "", false
	}
	prefix := strings.TrimSuffix(subpath, "/") + model.APIURLSuffix + "/users/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if len(parts) != 7 || (parts[0] != model.Me && !model.IsValidId(parts[0])) || parts[1] != "teams" || !model.IsValidId(parts[2]) || parts[3] != "threads" || !model.IsValidId(parts[4]) || parts[5] != "read" {
		return "", "", false
	}
	timestamp, err := strconv.ParseInt(parts[6], 10, 64)
	if err != nil || timestamp < 0 {
		return "", "", false
	}
	return parts[0], parts[4], true
}

func (s *Service) getThreadReadReceipts(c *web.Context, w http.ResponseWriter, channelID, threadID string) {
	if !model.IsValidId(threadID) {
		c.SetInvalidURLParam("thread_id")
		return
	}
	post, appErr := c.App.GetSinglePost(c.AppContext, threadID, false)
	if appErr != nil || post.ChannelId != channelID || post.RootId != "" {
		c.SetInvalidURLParam("thread_id")
		return
	}
	receipts := make(map[string]int64)
	pageErr := walkChannelMembers(func(after string) (model.ChannelMembers, error) {
		rows, err := c.App.Srv().Store().GetInternalMasterDB().QueryContext(c.AppContext.Context(), `
            SELECT tm.userid,tm.lastviewed FROM threadmemberships tm
            JOIN channelmembers cm ON cm.userid=tm.userid AND cm.channelid=$1
            WHERE tm.postid=$2 AND tm.userid>$3 ORDER BY tm.userid LIMIT $4`, channelID, threadID, after, maxMembers)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		members := make(model.ChannelMembers, 0, maxMembers)
		for rows.Next() {
			var member model.ChannelMember
			if err := rows.Scan(&member.UserId, &member.LastViewedAt); err != nil {
				return nil, err
			}
			members = append(members, member)
		}
		return members, rows.Err()
	}, func(member model.ChannelMember) { receipts[member.UserId] = member.LastViewedAt })
	if pageErr != nil {
		c.Err = model.NewAppError("WorkspaceEnterprise.GetThreadReadReceipts", "app.channel.get_members.app_error", nil, "", http.StatusInternalServerError).Wrap(pageErr)
		return
	}
	if err := json.NewEncoder(w).Encode(receipts); err != nil {
		c.Logger.Warn("Failed to write thread read receipts", mlog.Err(err))
	}
}
