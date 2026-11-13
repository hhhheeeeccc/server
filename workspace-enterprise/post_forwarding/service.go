package post_forwarding

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/gorilla/mux"
	"github.com/mattermost/enterprise/disappearing_messages"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

type Service struct {
	slots chan struct{}
}

type fileRequest struct {
	ChannelID string   `json:"channel_id"`
	FileIDs   []string `json:"file_ids"`
}

// Register keeps attachment bytes inside the server/storage boundary. Each copy
// still goes through the native upload pipeline and gets independent paths;
// sharing FileInfo paths would let deleting one message break another.
func Register(a *app.App) {
	s := &Service{slots: make(chan struct{}, 4)}
	a.Srv().Router.Handle(model.APIURLSuffix+"/workspace/posts/{post_id:[A-Za-z0-9]+}/files/forward", &web.Handler{
		Srv: a.Srv(), HandleFunc: s.forwardFiles, HandlerName: "ForwardPostFiles",
		RequireSession: true, RequireMfa: true,
	}).Methods(http.MethodPost)
}

func (s *Service) forwardFiles(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if c.App.License() == nil {
		c.Err = model.NewAppError("ForwardPostFiles", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	if !*c.App.Config().FileSettings.EnableFileAttachments {
		c.Err = model.NewAppError("ForwardPostFiles", "api.file.attachments.disabled.app_error", nil, "", http.StatusForbidden)
		return
	}
	var body fileRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := decoder.Decode(&body); err != nil {
		c.SetInvalidParamWithErr("file_ids", err)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		c.SetInvalidParam("file_ids")
		return
	}
	postID := mux.Vars(r)["post_id"]
	if !model.IsValidId(postID) || !model.IsValidId(body.ChannelID) || len(body.FileIDs) == 0 || len(model.ArrayToJSON(body.FileIDs)) > model.PostFileidsMaxRunes {
		c.SetInvalidParam("file_ids")
		return
	}
	if allowed, _ := c.App.SessionHasPermissionToReadPost(c.AppContext, *c.AppContext.Session(), postID); !allowed {
		c.SetPermissionError(model.PermissionReadChannelContent)
		return
	}
	// Reject protected posts before the normal getter attempts to reveal them.
	// Keep the getter below for native visibility filters and consumption hooks.
	source, err := c.App.Srv().Store().Post().GetSingle(c.AppContext, postID, false)
	if err != nil {
		status := http.StatusInternalServerError
		var notFound *store.ErrNotFound
		if errors.As(err, &notFound) {
			status = http.StatusNotFound
		}
		c.Err = model.NewAppError("ForwardPostFiles", "app.post.get.app_error", nil, "", status).Wrap(err)
		return
	}
	if source.DeleteAt != 0 || source.Type == model.PostTypeBurnOnRead {
		c.SetPermissionError(model.PermissionReadChannelContent)
		return
	}
	if appErr := disappearing_messages.CheckPostAccess(r.Context(), c.App, postID); appErr != nil {
		c.Err = appErr
		return
	}
	post, appErr := c.App.GetSinglePost(c.AppContext, postID, false)
	if appErr != nil {
		c.Err = appErr
		return
	}
	if post.DeleteAt != 0 || post.Type == model.PostTypeBurnOnRead {
		c.SetPermissionError(model.PermissionReadChannelContent)
		return
	}
	if allowed, _ := c.App.SessionHasPermissionToChannel(c.AppContext, *c.AppContext.Session(), body.ChannelID, model.PermissionUploadFile); !allowed {
		c.SetPermissionError(model.PermissionUploadFile)
		return
	}
	if allowed, _ := c.App.SessionHasPermissionToChannel(c.AppContext, *c.AppContext.Session(), body.ChannelID, model.PermissionCreatePost); !allowed {
		c.SetPermissionError(model.PermissionCreatePost)
		return
	}
	if !c.App.HasPermissionToChannelAction(c.AppContext, c.AppContext.Session().UserId, c.AppContext.Session().Roles, post.ChannelId, model.AccessControlPolicyActionDownloadFileAttachment) ||
		!c.App.HasPermissionToChannelAction(c.AppContext, c.AppContext.Session().UserId, c.AppContext.Session().Roles, body.ChannelID, model.AccessControlPolicyActionUploadFileAttachment) {
		c.SetPermissionError(model.PermissionUploadFile)
		return
	}
	channel, appErr := c.App.GetChannel(c.AppContext, body.ChannelID)
	if appErr != nil {
		c.Err = appErr
		return
	}
	restricted, appErr := c.App.CheckIfChannelIsRestrictedDM(c.AppContext, channel)
	if appErr != nil {
		c.Err = appErr
		return
	}
	if restricted || channel.DeleteAt != 0 {
		c.SetPermissionError(model.PermissionUploadFile)
		return
	}
	files := make([]*model.FileInfo, 0, len(body.FileIDs))
	seen := make(map[string]bool, len(body.FileIDs))
	for _, id := range body.FileIDs {
		if !model.IsValidId(id) || seen[id] || !slices.Contains(post.FileIds, id) {
			c.SetInvalidParam("file_ids")
			return
		}
		seen[id] = true
		info, appErr := c.App.GetFileInfo(c.AppContext, id)
		if appErr != nil {
			c.Err = appErr
			return
		}
		if info.DeleteAt != 0 || info.PostId != post.Id || info.ChannelId != post.ChannelId {
			c.SetPermissionError(model.PermissionReadChannelContent)
			return
		}
		if reason := c.App.RunFileWillBeDownloadedHook(c.AppContext, info, c.AppContext.Session().UserId, r.Header.Get(model.ConnectionId), model.FileDownloadTypeFile); reason != "" {
			c.Err = model.NewAppError("ForwardPostFiles", "api.file.get_file.rejected_by_plugin", map[string]any{"Reason": reason}, "", http.StatusForbidden)
			return
		}
		files = append(files, info)
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		c.Err = model.NewAppError("ForwardPostFiles", "api.context.server_busy.app_error", nil, "", http.StatusTooManyRequests)
		return
	}
	result := &model.FileUploadResponse{FileInfos: []*model.FileInfo{}, ClientIds: []string{}}
	defer func() {
		if c.Err != nil {
			for _, info := range result.FileInfos {
				s.cleanup(c, info)
			}
		}
	}()
	for _, info := range files {
		if r.Context().Err() != nil || c.App.License() == nil {
			c.SetPermissionError(model.PermissionUploadFile)
			return
		}
		if appErr := disappearing_messages.CheckPostAccess(r.Context(), c.App, postID); appErr != nil {
			c.Err = appErr
			return
		}
		reader, appErr := c.App.FileReader(info.Path)
		if appErr != nil {
			c.Err = appErr
			return
		}
		copy, appErr := c.App.UploadFileX(c.AppContext, body.ChannelID, info.Name,
			contextReader{Context: r.Context(), Reader: reader},
			app.UploadFileSetTeamId("noteam"), app.UploadFileSetUserId(c.AppContext.Session().UserId),
			app.UploadFileSetTimestamp(time.Now()), app.UploadFileSetContentLength(info.Size))
		_ = reader.Close()
		if appErr != nil {
			c.Err = appErr
			return
		}
		result.FileInfos = append(result.FileInfos, copy)
	}
	// A large upload may cross the source deadline. Reject it and let the
	// existing cleanup remove every new copy before exposing any upload IDs.
	if appErr := disappearing_messages.CheckPostAccess(r.Context(), c.App, postID); appErr != nil {
		c.Err = appErr
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		c.Logger.Warn("Failed to write forwarded file response", mlog.Err(err))
	}
}

func (s *Service) cleanup(c *web.Context, info *model.FileInfo) {
	for _, path := range []string{info.Path, info.ThumbnailPath, info.PreviewPath} {
		if path != "" {
			if err := c.App.RemoveFile(path); err != nil {
				c.Logger.Warn("Failed to remove incomplete forwarded file", mlog.Err(err))
				return
			}
		}
	}
	if err := c.App.Srv().Store().FileInfo().PermanentDelete(c.AppContext, info.Id); err != nil {
		c.Logger.Warn("Failed to remove incomplete forwarded file metadata", mlog.Err(err))
	}
}

type contextReader struct {
	context.Context
	io.Reader
}

func (r contextReader) Read(data []byte) (int, error) {
	if err := r.Context.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(data)
}
