package bulk_reactions

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const (
	maxPostIDs          = 500
	maxBulkRequestBytes = 32 << 10
)

type Service struct {
	app *app.App
}

func Register(a *app.App) {
	service := &Service{app: a}
	handler := service.getBulkReactions

	a.Srv().Router.Handle(model.APIURLSuffix+"/posts/ids/reactions", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodPost)
}

func (s *Service) getBulkReactions(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if s.app.License() == nil {
		c.Err = model.NewAppError("GetBulkReactions", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBulkRequestBytes))
	var postIDs []string
	err := decoder.Decode(&postIDs)
	if err == nil {
		var trailing any
		if extraErr := decoder.Decode(&trailing); extraErr != io.EOF {
			err = extraErr
			if err == nil {
				err = errors.New("unexpected data after post IDs")
			}
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("GetBulkReactions", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.Err = model.NewAppError("GetBulkReactions", model.PayloadParseError, nil, "", http.StatusBadRequest).Wrap(err)
		return
	}
	if postIDs == nil {
		c.SetInvalidParam("post_ids")
		return
	}
	if len(postIDs) > maxPostIDs {
		c.SetInvalidParam("post_ids")
		return
	}
	postIDs = model.RemoveDuplicateStrings(postIDs)

	for _, postID := range postIDs {
		if !model.IsValidId(postID) {
			c.SetInvalidParam("post_ids")
			return
		}
		if allowed, _ := c.App.SessionHasPermissionToReadPost(c.AppContext, *c.AppContext.Session(), postID); !allowed {
			c.SetPermissionError(model.PermissionReadChannelContent)
			return
		}
	}

	reactionsByPost := make(map[string][]*model.Reaction, len(postIDs))
	for _, postID := range postIDs {
		reactionsByPost[postID] = []*model.Reaction{}
	}
	if len(postIDs) > 0 {
		if model.SafeDereference(s.app.Config().SqlSettings.DriverName) == model.DatabaseDriverPostgres {
			if err := s.loadReactionsBatch(r, postIDs, reactionsByPost); err != nil {
				c.Err = model.NewAppError("GetBulkReactions", "app.reaction.get_for_post.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
				return
			}
		} else {
			for _, postID := range postIDs {
				reactions, appErr := c.App.GetReactionsForPost(postID)
				if appErr != nil {
					c.Err = appErr
					return
				}
				if reactions != nil {
					reactionsByPost[postID] = reactions
				}
			}
		}
	}

	if err := json.NewEncoder(w).Encode(reactionsByPost); err != nil {
		c.Logger.Warn("Error writing bulk reactions", mlog.Err(err))
	}
}

func (s *Service) loadReactionsBatch(r *http.Request, postIDs []string, byPost map[string][]*model.Reaction) error {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return errors.New("master database connection is unavailable")
	}
	rows, err := db.QueryContext(r.Context(), `
		SELECT UserId, PostId, EmojiName, CreateAt,
			COALESCE(UpdateAt, CreateAt), COALESCE(DeleteAt, 0),
			RemoteId, COALESCE(ChannelId, '')
		FROM Reactions
		WHERE PostId = ANY($1) AND COALESCE(DeleteAt, 0) = 0
		ORDER BY CreateAt`, pq.Array(postIDs))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var reaction model.Reaction
		var remoteID sql.NullString
		if err := rows.Scan(&reaction.UserId, &reaction.PostId, &reaction.EmojiName,
			&reaction.CreateAt, &reaction.UpdateAt, &reaction.DeleteAt,
			&remoteID, &reaction.ChannelId); err != nil {
			return err
		}
		if remoteID.Valid {
			reaction.RemoteId = &remoteID.String
		}
		byPost[reaction.PostId] = append(byPost[reaction.PostId], &reaction)
	}
	return rows.Err()
}
