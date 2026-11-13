package channel_read_receipts

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const (
	maxMembers                  = 200
	channelMemberViewedEvent    = model.WebsocketEventType("channel_member_viewed")
	channelMemberViewPathPrefix = model.APIURLSuffix + "/channels/members/"
	saveReactionPath            = model.APIURLSuffix + "/reactions"
)

type Service struct {
	app      *app.App
	receipts *receiptBatcher
}

func Register(a *app.App) {
	service := &Service{app: a}
	service.receipts = newReceiptBatcher(a.Srv().Platform().GoContext(), service.publishReceiptBatch)
	a.Srv().Go(service.receipts.run)
	handler := service.getChannelReadReceipts
	a.Srv().Router.Use(service.publishChannelMemberViewed)
	a.Srv().Router.Use(service.markReactedPostsRead)
	a.Srv().Router.Use(service.publishThreadMemberViewed)

	a.Srv().Router.Handle(model.APIURLSuffix+"/channels/{channel_id:[A-Za-z0-9]+}/read_receipts", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodGet)
	a.Srv().Router.Handle(model.APIURLSuffix+"/channels/{channel_id:[A-Za-z0-9]+}/threads/{thread_id:[A-Za-z0-9]+}/read_receipts", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodGet)
}

// markReactedPostsRead treats a successful reaction as direct proof that the
// reacting user viewed that post, not messages created afterwards. Only a
// reaction to the latest channel post may advance the channel's read cursor;
// search/permalink reactions to older posts leave unread state intact.
func (s *Service) markReactedPostsRead(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reaction, ok := saveReactionRequest(r, s.app.Config())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		capture := &statusResponseWriter{ResponseWriter: w}
		next.ServeHTTP(capture, r)
		if capture.statusCode < http.StatusOK || capture.statusCode >= http.StatusMultipleChoices || s.app.License() == nil {
			return
		}

		session, ok := s.resolveSession(r)
		if !ok || session.UserId != reaction.UserID {
			return
		}

		rctx := request.EmptyContext(s.app.Log()).WithContext(r.Context()).With(app.RequestContextWithMaster).WithSession(session)
		post, appErr := s.app.GetSinglePost(rctx, reaction.PostID, false)
		if appErr != nil {
			return
		}

		// Check the latest timestamp and update counters in one SQL statement.
		// A separate read followed by MarkChannelsAsViewed races with new posts.
		// A timestamp can belong to multiple posts. In that case the reaction
		// does not prove the other post was read, so preserve unread counters.
		var viewedAt int64
		err := s.app.Srv().Store().GetInternalMasterDB().QueryRowContext(rctx.Context(), `
			UPDATE channelmembers cm SET
				lastviewedat = GREATEST(cm.lastviewedat, $3),
				lastupdateat = GREATEST(cm.lastviewedat, $3),
				msgcount = GREATEST(cm.msgcount, c.totalmsgcount),
				msgcountroot = GREATEST(cm.msgcountroot, c.totalmsgcountroot),
				mentioncount = 0, mentioncountroot = 0, urgentmentioncount = 0
			FROM channels c
			WHERE cm.channelid = $1 AND cm.userid = $2 AND c.id = cm.channelid
				AND c.lastpostat <= $3 AND cm.lastviewedat < $3
				AND NOT EXISTS (
					SELECT 1 FROM posts p
					WHERE p.channelid = cm.channelid AND p.deleteat = 0
						AND p.createat >= $3 AND p.id <> $4
				)
			RETURNING cm.lastviewedat`, post.ChannelId, session.UserId, post.CreateAt, post.Id).Scan(&viewedAt)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				rctx.Logger().Warn("Unable to record latest-post reaction receipt", mlog.Err(err))
			}
			return
		}
		if *s.app.Config().ServiceSettings.EnableChannelViewedMessages {
			event := model.NewWebSocketEvent(model.WebsocketEventMultipleChannelsViewed, "", "", session.UserId, nil, "")
			event.Add("channel_times", map[string]int64{post.ChannelId: viewedAt})
			s.app.Publish(event)
		}

		s.publishReceipt(rctx, post.ChannelId, session.UserId)
	})
}

// publishChannelMemberViewed observes the existing Mattermost channel-view
// endpoints. It lets the original handlers perform authentication, permission
// checks, validation, and the LastViewedAt update, then publishes the receipt
// required by the workspace webapp after a successful response.
// Posting deliberately does not update other readers: Status.ActiveChannel can
// survive background tabs, and scanning/writing all members caused event storms.
func (s *Service) publishChannelMemberViewed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedUserID, operation, ok := channelViewRequest(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		channelIDs, bodyOK := readChannelIDs(r, operation, *s.app.Config().ServiceSettings.MaximumPayloadSizeBytes)
		if !bodyOK {
			next.ServeHTTP(w, r)
			return
		}

		capture := &statusResponseWriter{ResponseWriter: w}
		next.ServeHTTP(capture, r)
		if capture.statusCode < http.StatusOK || capture.statusCode >= http.StatusMultipleChoices || s.app.License() == nil {
			return
		}
		userID, ok := s.resolveUserID(r, requestedUserID)
		if !ok {
			return
		}

		rctx := request.EmptyContext(s.app.Log()).WithContext(r.Context()).With(app.RequestContextWithMaster)
		for _, channelID := range channelIDs {
			if !model.IsValidId(channelID) {
				continue
			}

			s.publishReceipt(rctx, channelID, userID)
		}
	})
}

func (s *Service) publishReceipt(rctx request.CTX, channelID, userID string) {
	if !*s.app.Config().ServiceSettings.EnableChannelViewedMessages {
		return
	}

	member, appErr := s.app.GetChannelMember(rctx, channelID, userID)
	if appErr != nil {
		return
	}

	s.receipts.add(channelID, userID, member.LastViewedAt)
}

func (s *Service) publishReceiptBatch(channelID string, receipts map[string]int64) {
	if s.app.License() == nil || !*s.app.Config().ServiceSettings.EnableChannelViewedMessages {
		return
	}
	event := model.NewWebSocketEvent(channelMemberViewedEvent, "", channelID, "", nil, "")
	event.Add("channel_id", channelID)
	if len(receipts) == 1 {
		// Keep the single-reader wire format for existing consumers.
		for userID, viewedAt := range receipts {
			event.Add("user_id", userID)
			event.Add("last_viewed_at", viewedAt)
		}
	} else {
		event.Add("receipts", receipts)
	}
	s.app.Publish(event)
}

func channelViewRequest(r *http.Request) (string, string, bool) {
	if r.Method != http.MethodPost {
		return "", "", false
	}

	pathIndex := strings.LastIndex(r.URL.Path, channelMemberViewPathPrefix)
	if pathIndex < 0 {
		return "", "", false
	}

	path := strings.TrimPrefix(r.URL.Path[pathIndex:], channelMemberViewPathPrefix)
	parts := strings.Split(path, "/")
	if len(parts) != 2 || (parts[0] != model.Me && !model.IsValidId(parts[0])) || (parts[1] != "view" && parts[1] != "mark_read") {
		return "", "", false
	}

	return parts[0], parts[1], true
}

func (s *Service) resolveUserID(r *http.Request, requestedUserID string) (string, bool) {
	if requestedUserID != model.Me {
		return requestedUserID, true
	}
	session, ok := s.resolveSession(r)
	if !ok {
		return "", false
	}
	return session.UserId, true
}

func (s *Service) resolveSession(r *http.Request) (*model.Session, bool) {
	token, _ := app.ParseAuthTokenFromRequest(r)
	if token == "" {
		return nil, false
	}
	session, appErr := s.app.GetSession(token)
	if appErr != nil || !model.IsValidId(session.UserId) {
		return nil, false
	}
	return session, true
}

func readChannelIDs(r *http.Request, operation string, maxPayloadSize int64) ([]string, bool) {
	body, ok := readRequestBody(r, maxPayloadSize)
	if !ok {
		return nil, false
	}

	if operation == "view" {
		var view model.ChannelView
		if err := json.Unmarshal(body, &view); err != nil {
			return nil, false
		}
		channelIDs := make([]string, 0, 2)
		if view.ChannelId != "" {
			channelIDs = append(channelIDs, view.ChannelId)
		}
		if view.PrevChannelId != "" && view.PrevChannelId != view.ChannelId {
			channelIDs = append(channelIDs, view.PrevChannelId)
		}
		return channelIDs, true
	}

	var channelIDs []string
	if err := json.Unmarshal(body, &channelIDs); err != nil {
		return nil, false
	}
	// The core handler still receives the original request. Only deduplicate
	// our receipt work, which otherwise queries membership once per occurrence.
	seen := make(map[string]struct{}, len(channelIDs))
	uniqueIDs := channelIDs[:0]
	for _, channelID := range channelIDs {
		if _, exists := seen[channelID]; exists {
			continue
		}
		seen[channelID] = struct{}{}
		uniqueIDs = append(uniqueIDs, channelID)
	}
	return uniqueIDs, true
}

type reactionRequest struct {
	UserID string `json:"user_id"`
	PostID string `json:"post_id"`
}

func saveReactionRequest(r *http.Request, config *model.Config) (reactionRequest, bool) {
	if r.Method != http.MethodPost {
		return reactionRequest{}, false
	}
	subpath, err := utils.GetSubpathFromConfig(config)
	if err != nil || strings.TrimSuffix(r.URL.Path, "/") != strings.TrimSuffix(subpath, "/")+saveReactionPath {
		return reactionRequest{}, false
	}

	body, ok := readRequestBody(r, *config.ServiceSettings.MaximumPayloadSizeBytes)
	if !ok {
		return reactionRequest{}, false
	}

	var reaction reactionRequest
	if err := json.Unmarshal(body, &reaction); err != nil || !model.IsValidId(reaction.UserID) || !model.IsValidId(reaction.PostID) {
		return reactionRequest{}, false
	}

	return reaction, true
}

func readRequestBody(r *http.Request, maxPayloadSize int64) ([]byte, bool) {
	originalBody := r.Body
	limit := maxPayloadSize + bytes.MinRead
	body, err := io.ReadAll(io.LimitReader(originalBody, limit+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.MultiReader(bytes.NewReader(body), originalBody), Closer: originalBody}
	if err != nil || int64(len(body)) > limit {
		return nil, false
	}
	return body, true
}

type statusResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *statusResponseWriter) WriteHeader(statusCode int) {
	if w.statusCode == 0 {
		w.statusCode = statusCode
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *statusResponseWriter) Write(data []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func (s *Service) getChannelReadReceipts(c *web.Context, w http.ResponseWriter, _ *http.Request) {
	if c.App.License() == nil {
		c.Err = model.NewAppError("WorkspaceEnterprise.GetChannelReadReceipts", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}

	channelID := c.Params.ChannelId
	if !model.IsValidId(channelID) {
		c.SetInvalidURLParam("channel_id")
		return
	}

	if allowed, _ := c.App.SessionHasPermissionToChannel(c.AppContext, *c.AppContext.Session(), channelID, model.PermissionReadChannel); !allowed {
		c.SetPermissionError(model.PermissionReadChannel)
		return
	}

	if threadID := c.Params.ThreadId; threadID != "" {
		s.getThreadReadReceipts(c, w, channelID, threadID)
		return
	}

	receipts := make(map[string]int64)
	pageErr := walkChannelMembers(func(after string) (model.ChannelMembers, error) {
		return s.channelMemberPage(c.AppContext.Context(), channelID, after)
	}, func(member model.ChannelMember) {
		receipts[member.UserId] = member.LastViewedAt
	})
	if pageErr != nil {
		c.Err = model.NewAppError("WorkspaceEnterprise.GetChannelReadReceipts", "app.channel.get_members.app_error", nil, "", http.StatusInternalServerError).Wrap(pageErr)
		return
	}

	if err := json.NewEncoder(w).Encode(receipts); err != nil {
		c.Logger.Warn("Failed to write channel read receipts", mlog.Err(err))
	}
}

// The channel/member primary key supports stable keyset paging without an
// unbounded OFFSET, including when a channel has more than 200 members.
func (s *Service) channelMemberPage(ctx context.Context, channelID, after string) (model.ChannelMembers, error) {
	rows, err := s.app.Srv().Store().GetInternalMasterDB().QueryContext(ctx, `
		SELECT userid, COALESCE(lastviewedat, 0)
		FROM channelmembers
		WHERE channelid = $1 AND userid > $2
		ORDER BY userid
		LIMIT $3
	`, channelID, after, maxMembers)
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
}

func walkChannelMembers(fetch func(after string) (model.ChannelMembers, error), visit func(model.ChannelMember)) error {
	after := ""
	for {
		members, err := fetch(after)
		if err != nil {
			return err
		}
		for _, member := range members {
			visit(member)
		}
		if len(members) < maxMembers {
			return nil
		}
		after = members[len(members)-1].UserId
	}
}
