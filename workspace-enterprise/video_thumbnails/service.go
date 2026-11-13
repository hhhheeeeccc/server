package video_thumbnails

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/jpeg"
	"io"
	"net/http"
	"path"
	"strings"

	"golang.org/x/sync/singleflight"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/app/imaging"
)

type Service struct {
	app  *app.App
	work singleflight.Group
}

func Register(a *app.App) {
	s := &Service{app: a}
	a.Srv().Router.Use(s.previews)
}

// Keep the core upload, file-access, ABAC, plugin, and download handlers in
// charge. Generate only after an authorized response, then let those same
// handlers serve thumbnails from the normal file store.
func (s *Service) previews(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metadata, fileID := requestKind(r)
		if (!metadata && fileID == "") || s.app.License() == nil || !isFFmpegAvailable() {
			next.ServeHTTP(w, r)
			return
		}
		plain := r.Clone(r.Context())
		plain.Header.Del("Accept-Encoding")
		buffer := &responseBuffer{ResponseWriter: w, errorsOnly: !metadata}
		next.ServeHTTP(buffer, plain)
		defer buffer.flush()
		if buffer.sent || buffer.Header().Get("Content-Encoding") != "" || s.app.License() == nil {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), thumbnailTimeout)
		defer cancel()
		rctx := request.EmptyContext(s.app.Log()).WithContext(ctx).With(app.RequestContextWithMaster)
		if !metadata {
			var appErr model.AppError
			if buffer.status != http.StatusBadRequest || json.Unmarshal(buffer.body.Bytes(), &appErr) != nil ||
				(appErr.Id != "api.file.get_file_thumbnail.no_thumbnail.app_error" && appErr.Id != "api.file.get_file_preview.no_preview.app_error") {
				return
			}
			info, err := s.ensurePreview(rctx, fileID)
			if err != nil || info == nil || !info.HasPreviewImage {
				return
			}
			buffer.sent = true
			w.Header().Del("Content-Length")
			next.ServeHTTP(w, plain)
			return
		}
		if buffer.status < 200 || buffer.status >= 300 {
			return
		}
		body, changed := s.enrichMetadata(rctx, buffer.body.Bytes())
		if changed {
			buffer.body.Reset()
			buffer.body.Write(body)
			w.Header().Del("Content-Length")
			w.Header().Del("ETag")
		}
	})
}

func requestKind(r *http.Request) (metadata bool, fileID string) {
	if !strings.HasPrefix(r.URL.Path, model.APIURLSuffix+"/") {
		return false, ""
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, model.APIURLSuffix+"/"), "/")
	if r.Method == http.MethodPost {
		return (len(parts) == 1 && parts[0] == "files") ||
			(len(parts) == 2 && parts[0] == "uploads" && model.IsValidId(parts[1])), ""
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false, ""
	}
	if len(parts) == 3 && parts[0] == "files" && model.IsValidId(parts[1]) {
		switch parts[2] {
		case "info":
			return r.Method == http.MethodGet, ""
		case "thumbnail", "preview":
			return false, parts[1]
		}
	}
	return r.Method == http.MethodGet && len(parts) == 4 && parts[0] == "posts" && model.IsValidId(parts[1]) && parts[2] == "files" && parts[3] == "info", ""
}

func (s *Service) enrichMetadata(rctx request.CTX, body []byte) ([]byte, bool) {
	var document any
	if json.Unmarshal(body, &document) != nil {
		return body, false
	}
	changed := false
	var visit func(any)
	visit = func(value any) {
		switch v := value.(type) {
		case []any:
			for _, item := range v {
				visit(item)
			}
		case map[string]any:
			if files, ok := v["file_infos"]; ok {
				visit(files)
				return
			}
			id, _ := v["id"].(string)
			mime, _ := v["mime_type"].(string)
			if !model.IsValidId(id) || !strings.HasPrefix(mime, "video/") || rctx.Context().Err() != nil {
				return
			}
			if preview, _ := v["has_preview_image"].(bool); preview {
				return
			}
			info, err := s.ensurePreview(rctx, id)
			if err != nil {
				rctx.Logger().Warn("Unable to generate video preview", mlog.String("file_id", id), mlog.Err(err))
				return
			}
			if info != nil && info.HasPreviewImage {
				v["has_preview_image"] = true
				v["mini_preview"] = info.MiniPreview
				v["update_at"] = info.UpdateAt
				changed = true
			}
		}
	}
	visit(document)
	if !changed {
		return body, false
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return body, false
	}
	return encoded, true
}

func (s *Service) ensurePreview(rctx request.CTX, id string) (*model.FileInfo, error) {
	result := s.work.DoChan(id, func() (any, error) { return s.generate(rctx, id) })
	select {
	case <-rctx.Context().Done():
		return nil, rctx.Context().Err()
	case res := <-result:
		if res.Err != nil {
			return nil, res.Err
		}
		info, _ := res.Val.(*model.FileInfo)
		return info, nil
	}
}

func (s *Service) generate(rctx request.CTX, id string) (*model.FileInfo, error) {
	info, err := s.app.Srv().Store().FileInfo().GetFromMaster(id)
	if err != nil {
		return nil, err
	}
	if s.app.License() == nil || info.DeleteAt != 0 || model.SafeDereference(info.RemoteId) != "" || !strings.HasPrefix(info.MimeType, "video/") || info.HasPreviewImage || info.Path == "" || info.ThumbnailPath != "" || info.PreviewPath != "" {
		return info, nil
	}
	limit := min(*s.app.Config().FileSettings.MaxFileSize, int64(512*1024*1024))
	if info.Size <= 0 || info.Size > limit {
		return nil, errors.New("video thumbnail: input limit exceeded")
	}
	frame, err := extractVideoThumbnailFromSource(rctx.Context(), func() (io.ReadCloser, error) {
		reader, appErr := s.app.FileReader(info.Path)
		if appErr != nil {
			return nil, appErr
		}
		return reader, nil
	}, limit)
	if err != nil {
		return nil, err
	}
	dimensions, err := jpeg.DecodeConfig(bytes.NewReader(frame))
	if err != nil || dimensions.Width <= 0 || dimensions.Height <= 0 || dimensions.Width > 1280 || dimensions.Height > 720 {
		return nil, errors.New("video thumbnail: invalid generated JPEG dimensions")
	}
	decoded, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		return nil, err
	}
	mini, err := imaging.GenerateMiniPreviewImage(decoded, 16, 16, 90)
	if err != nil {
		return nil, err
	}
	// Unique artifact paths avoid overwriting another node's successful output.
	prefix := path.Join(path.Dir(info.Path), model.NewId())
	thumb, preview := prefix+"_thumb.jpg", prefix+"_preview.jpg"
	committed := false
	defer func() {
		if !committed {
			_ = s.app.RemoveFile(thumb)
			_ = s.app.RemoveFile(preview)
		}
	}()
	for _, name := range []string{thumb, preview} {
		if _, appErr := s.app.WriteFileContext(rctx.Context(), bytes.NewReader(frame), name); appErr != nil {
			return nil, appErr
		}
	}
	if s.app.License() == nil {
		return nil, errors.New("video thumbnail: license removed during generation")
	}
	// Update only generated metadata; never resurrect a deleted file or overwrite
	// a concurrently attached post, indexed content, or an existing preview.
	now := model.GetMillis()
	result, err := s.app.Srv().Store().GetInternalMasterDB().ExecContext(rctx.Context(), `UPDATE FileInfo
		SET ThumbnailPath=$1, PreviewPath=$2, HasPreviewImage=true, MiniPreview=$3, UpdateAt=$4
		WHERE Id=$5 AND DeleteAt=0 AND Path=$6 AND HasPreviewImage=false
		AND ThumbnailPath='' AND PreviewPath=''`, thumb, preview, mini, now, id, info.Path)
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return s.app.Srv().Store().FileInfo().GetFromMaster(id)
	}
	committed = true
	updated, err := s.app.Srv().Store().FileInfo().GetFromMaster(id)
	if err == nil && updated.PostId != "" {
		s.app.Srv().Store().FileInfo().InvalidateFileInfosForPostCache(updated.PostId, false)
	}
	return updated, err
}

// JSON metadata is bounded; large responses and successful binary downloads
// pass through directly without buffering the uploaded/downloaded video.
type responseBuffer struct {
	http.ResponseWriter
	body       bytes.Buffer
	status     int
	sent       bool
	errorsOnly bool
}

func (b *responseBuffer) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		b.ResponseWriter.WriteHeader(status)
		return
	}
	if b.status != 0 {
		return
	}
	b.status = status
	if b.errorsOnly && status < 400 {
		b.flush()
	}
}

func (b *responseBuffer) Write(data []byte) (int, error) {
	if b.status == 0 {
		b.WriteHeader(http.StatusOK)
	}
	if !b.sent && b.body.Len()+len(data) > 2*1024*1024 {
		b.flush()
	}
	if b.sent {
		return b.ResponseWriter.Write(data)
	}
	return b.body.Write(data)
}

func (b *responseBuffer) flush() {
	if b.sent {
		return
	}
	b.sent = true
	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	b.ResponseWriter.WriteHeader(status)
	_, _ = b.body.WriteTo(b.ResponseWriter)
}
