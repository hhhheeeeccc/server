package post_ack_batch

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const (
	maxPostIDs        = 500
	maxBatchBodyBytes = 32 << 10
)

type Service struct {
	app *app.App
}

type request struct {
	PostIDs []string `json:"post_ids"`
}

func Register(a *app.App) {
	service := &Service{app: a}
	handler := service.acknowledgePosts

	a.Srv().Router.Handle(model.APIURLSuffix+"/users/{user_id:[A-Za-z0-9]+}/posts/ack_batch", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodPost)
}

func (s *Service) acknowledgePosts(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !model.MinimumProfessionalLicense(s.app.License()) {
		c.Err = model.NewAppError("AcknowledgePostsBatch", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}

	userID := c.Params.UserId
	if !model.IsValidId(userID) {
		c.SetInvalidURLParam("user_id")
		return
	}

	var body request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBatchBodyBytes))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&body)
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
			c.Err = model.NewAppError("AcknowledgePostsBatch", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("post_ids", err)
		return
	}
	if len(body.PostIDs) == 0 || len(body.PostIDs) > maxPostIDs {
		c.SetInvalidParam("post_ids")
		return
	}

	if !c.App.SessionHasPermissionToUser(c.AppContext, *c.AppContext.Session(), userID) {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}

	// Validate every post before changing anything so a denied post cannot
	// leave a partially acknowledged request behind.
	seen := make(map[string]struct{}, len(body.PostIDs))
	postIDs := make([]string, 0, len(body.PostIDs))
	for _, postID := range body.PostIDs {
		if !model.IsValidId(postID) {
			c.SetInvalidParam("post_ids")
			return
		}
		if _, duplicate := seen[postID]; duplicate {
			continue
		}
		seen[postID] = struct{}{}
		if allowed, _ := c.App.SessionHasPermissionToReadPost(c.AppContext, *c.AppContext.Session(), postID); !allowed {
			c.SetPermissionError(model.PermissionReadChannelContent)
			return
		}
		post, appErr := c.App.GetSinglePost(c.AppContext, postID, false)
		if appErr != nil {
			c.Err = appErr
			return
		}
		channel, appErr := c.App.GetChannel(c.AppContext, post.ChannelId)
		if appErr != nil {
			c.Err = appErr
			return
		}
		if channel.DeleteAt > 0 {
			c.Err = model.NewAppError("AcknowledgePostsBatch", "api.acknowledgement.save.archived_channel.app_error", nil, "", http.StatusForbidden)
			return
		}
		postIDs = append(postIDs, postID)
	}

	acknowledgements := make([]*model.PostAcknowledgement, 0, len(postIDs))
	for _, postID := range postIDs {
		acknowledgement, appErr := c.App.SaveAcknowledgementForPost(c.AppContext, postID, userID)
		if appErr != nil {
			c.Err = appErr
			return
		}
		acknowledgements = append(acknowledgements, acknowledgement)
	}

	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(acknowledgements); err != nil {
		c.Logger.Warn("Error writing batch acknowledgements", mlog.Err(err))
	}
}
