package channel_last_post

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

type Service struct {
	app *app.App
}

const (
	maxBufferedResponseBytes = 16 << 20
	maxDecodedResponseBytes  = 32 << 20
)

// Bound the work to the latest visible row per requested channel. DISTINCT ON
// over the complete histories sorts and visits every matching post. LATERAL
// lets the existing (channelid,deleteat,createat) index stop after its first
// visible row, with id as the deterministic tie breaker.
// Fetch only candidate identities here. The App read below is authoritative;
// transferring message/props/file JSON twice duplicates work and retains large
// content buffers for every channel before native filtering and preparation.
const latestPostsQuery = `
	SELECT latest.id,latest.channelid,latest.rootid
	FROM unnest($1::varchar[]) AS selected(channelid)
	CROSS JOIN LATERAL (
		SELECT id,channelid,rootid
		FROM posts
		WHERE channelid = selected.channelid AND deleteat = 0 AND rootid = ''
			AND type NOT LIKE 'system_%'
		ORDER BY createat DESC, id DESC
		LIMIT 1
	) latest`

// Register enriches Mattermost's existing channel responses with the latest
// visible user post. The original handlers continue to own authentication,
// permission checks, channel selection, and response status codes.
func Register(a *app.App) {
	service := &Service{app: a}
	a.Srv().Router.Use(service.enrichChannelResponses)
}

func (s *Service) enrichChannelResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.app.License() == nil || !isChannelResponseRequest(r) {
			next.ServeHTTP(w, r)
			return
		}

		recorder := newBufferedResponseWriter(w)
		next.ServeHTTP(recorder, r)
		if recorder.streaming {
			return
		}
		if recorder.statusCode < http.StatusOK || recorder.statusCode >= http.StatusMultipleChoices || recorder.body.Len() == 0 {
			recorder.writeTo(w)
			return
		}

		session, ok := s.resolveSession(r)
		if !ok {
			recorder.writeTo(w)
			return
		}

		responseBody, encodeResponse, err := decodeResponseBody(recorder.header.Get("Content-Encoding"), recorder.body.Bytes())
		if err != nil {
			recorder.writeTo(w)
			return
		}
		body, channels, err := decodeChannels(responseBody)
		if err != nil {
			recorder.writeTo(w)
			return
		}
		if len(channels) == 0 {
			recorder.writeTo(w)
			return
		}

		rctx := request.EmptyContext(s.app.Log()).
			WithContext(r.Context()).
			With(app.RequestContextWithMaster).
			WithSession(session)
		if err := s.addLastPosts(rctx, channels); err != nil {
			rctx.Logger().Warn("Unable to add last posts to channel response", mlog.Err(err))
			recorder.writeTo(w)
			return
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			rctx.Logger().Warn("Unable to encode channel response with last posts", mlog.Err(err))
			recorder.writeTo(w)
			return
		}

		encoded, err = encodeResponse(encoded)
		if err != nil {
			rctx.Logger().Warn("Unable to compress channel response with last posts", mlog.Err(err))
			recorder.writeTo(w)
			return
		}

		recorder.header.Del("Content-Length")
		// The upstream validator describes the original response, not the
		// enriched body containing the latest posts.
		recorder.header.Del("ETag")
		recorder.body.Reset()
		_, _ = recorder.body.Write(encoded)
		recorder.writeTo(w)
	})
}

func (s *Service) addLastPosts(rctx request.CTX, channels []map[string]json.RawMessage) error {
	channelIDs := make([]string, 0, len(channels))
	channelByID := make(map[string]map[string]json.RawMessage, len(channels))
	for _, channel := range channels {
		var channelID string
		if err := json.Unmarshal(channel["id"], &channelID); err != nil || !model.IsValidId(channelID) {
			continue
		}
		// Mark this response as authoritative even when the channel has no
		// visible user post. The web client can then distinguish an explicit
		// empty preview from a WebSocket channel update that simply omits these
		// Enterprise-only fields.
		channel["last_post_id"] = json.RawMessage(`""`)
		channel["last_post"] = json.RawMessage(`null`)
		channelIDs = append(channelIDs, channelID)
		channelByID[channelID] = channel
	}
	channelIDs = s.readablePreviewChannelIDs(rctx, channelIDs)
	if len(channelIDs) == 0 {
		return nil
	}

	rows, err := s.app.Srv().Store().GetInternalMasterDB().QueryContext(rctx.Context(), latestPostsQuery, pq.Array(channelIDs))
	if err != nil {
		return err
	}
	defer rows.Close()
	var posts []*model.Post
	for rows.Next() {
		var post model.Post
		if err := rows.Scan(&post.Id, &post.ChannelId, &post.RootId); err != nil {
			return err
		}
		channel := channelByID[post.ChannelId]
		if channel == nil {
			continue
		}
		posts = append(posts, &post)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Release the connection before querying metadata. This also works with
	// a pool limited to one connection.
	if err := rows.Close(); err != nil {
		return err
	}
	var priorityIDs []string
	if s.app.IsPostPriorityEnabled() {
		for _, post := range posts {
			if post.RootId == "" {
				priorityIDs = append(priorityIDs, post.Id)
			}
		}
	}
	metadata, err := previewPriorityMetadata(rctx.Context(), s.app.Srv().Store().GetInternalMasterDB(), priorityIDs)
	if err != nil {
		rctx.Logger().Warn("Unable to batch channel-preview priorities; using individual lookups", mlog.Err(err))
	}
	for _, candidate := range posts {
		channel := channelByID[candidate.ChannelId]
		// The SQL above selects candidates only. Reading through the App
		// preserves history limits, burn-on-read reveal rules and plugin
		// consumption hooks instead of exposing raw database content.
		post, appErr := s.app.GetSinglePost(rctx, candidate.Id, false)
		if appErr != nil || post == nil || post.DeleteAt != 0 || post.RootId != "" || post.ChannelId != candidate.ChannelId {
			continue
		}

		// Preview rows and batched priorities use the primary. A read replica
		// can lag after an edit, deletion, or newly created post.
		prepared := s.app.PreparePostForClient(rctx, post, &model.PreparePostForClientOpts{IncludePriority: err != nil})
		// Match the core preparation order: priority is attached after file,
		// reaction and burn-on-read metadata have been prepared.
		if value := metadata[post.Id]; value != nil {
			prepared.Metadata.Priority = value.Priority
			prepared.Metadata.Acknowledgements = value.Acknowledgements
		}
		prepared, _, sanitizeErr := s.app.SanitizePostMetadataForUser(rctx, prepared, rctx.Session().UserId)
		if sanitizeErr != nil {
			continue
		}
		encodedPost, err := json.Marshal(prepared)
		if err != nil {
			return err
		}
		encodedPostID, err := json.Marshal(post.Id)
		if err != nil {
			return err
		}
		channel["last_post_id"] = encodedPostID
		channel["last_post"] = encodedPost
	}
	return nil
}

func decodeResponseBody(contentEncoding string, data []byte) ([]byte, func([]byte) ([]byte, error), error) {
	identity := func(body []byte) ([]byte, error) { return body, nil }
	encoding := strings.TrimSpace(strings.ToLower(contentEncoding))
	if encoding == "" || encoding == "identity" {
		if len(data) > maxDecodedResponseBytes {
			return nil, nil, fmt.Errorf("channel response exceeds decoded size limit")
		}
		return data, identity, nil
	}

	switch encoding {
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, nil, err
		}
		decoded, err := readBoundedResponse(reader)
		closeErr := reader.Close()
		if err != nil {
			return nil, nil, err
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}

		encodeGzip := func(body []byte) ([]byte, error) {
			var encoded bytes.Buffer
			writer := gzip.NewWriter(&encoded)
			if _, err := writer.Write(body); err != nil {
				return nil, err
			}
			if err := writer.Close(); err != nil {
				return nil, err
			}
			return encoded.Bytes(), nil
		}
		return decoded, encodeGzip, nil
	case "zstd":
		decoder, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, nil, err
		}
		defer decoder.Close()
		decoded, err := readBoundedResponse(decoder)
		if err != nil {
			return nil, nil, err
		}

		encodeZstd := func(body []byte) ([]byte, error) {
			encoder, err := zstd.NewWriter(nil)
			if err != nil {
				return nil, err
			}
			defer encoder.Close()
			return encoder.EncodeAll(body, nil), nil
		}
		return decoded, encodeZstd, nil
	case "br":
		decoded, err := readBoundedResponse(brotli.NewReader(bytes.NewReader(data)))
		if err != nil {
			return nil, nil, err
		}

		encodeBrotli := func(body []byte) ([]byte, error) {
			var encoded bytes.Buffer
			writer := brotli.NewWriter(&encoded)
			if _, err := writer.Write(body); err != nil {
				return nil, err
			}
			if err := writer.Close(); err != nil {
				return nil, err
			}
			return encoded.Bytes(), nil
		}
		return decoded, encodeBrotli, nil
	case "deflate":
		reader, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, nil, err
		}
		decoded, err := readBoundedResponse(reader)
		closeErr := reader.Close()
		if err != nil {
			return nil, nil, err
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}

		encodeDeflate := func(body []byte) ([]byte, error) {
			var encoded bytes.Buffer
			writer, err := zlib.NewWriterLevel(&encoded, flate.DefaultCompression)
			if err != nil {
				return nil, err
			}
			if _, err := writer.Write(body); err != nil {
				return nil, err
			}
			if err := writer.Close(); err != nil {
				return nil, err
			}
			return encoded.Bytes(), nil
		}
		return decoded, encodeDeflate, nil
	default:
		return nil, nil, fmt.Errorf("unsupported content encoding %q", contentEncoding)
	}
}

func readBoundedResponse(reader io.Reader) ([]byte, error) {
	decoded, err := io.ReadAll(io.LimitReader(reader, maxDecodedResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(decoded) > maxDecodedResponseBytes {
		return nil, fmt.Errorf("channel response exceeds decoded size limit")
	}
	return decoded, nil
}

func (s *Service) resolveSession(r *http.Request) (*model.Session, bool) {
	token, _ := app.ParseAuthTokenFromRequest(r)
	if token == "" {
		return nil, false
	}
	session, appErr := s.app.GetSession(token)
	return session, appErr == nil && session != nil
}

func isChannelResponseRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	apiIndex := -1
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "api" && parts[i+1] == "v4" {
			apiIndex = i + 2
			break
		}
	}
	if apiIndex < 0 {
		return false
	}
	rest := parts[apiIndex:]

	if len(rest) == 1 && rest[0] == "channels" {
		return true
	}
	if len(rest) == 2 && rest[0] == "channels" && model.IsValidId(rest[1]) {
		return true
	}
	if len(rest) == 3 && rest[0] == "users" && rest[2] == "channels" {
		return rest[1] == model.Me || model.IsValidId(rest[1])
	}
	if len(rest) == 5 && rest[0] == "users" && rest[2] == "teams" && rest[4] == "channels" {
		return (rest[1] == model.Me || model.IsValidId(rest[1])) && model.IsValidId(rest[3])
	}
	if len(rest) == 3 && rest[0] == "teams" && model.IsValidId(rest[1]) && rest[2] == "channels" {
		return true
	}
	if len(rest) == 5 && rest[0] == "teams" && model.IsValidId(rest[1]) && rest[2] == "channels" && rest[3] == "name" {
		return true
	}
	if len(rest) == 6 && rest[0] == "teams" && rest[1] == "name" && rest[3] == "channels" && rest[4] == "name" {
		return true
	}
	return false
}

func decodeChannels(data []byte) (any, []map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("empty channel response")
	}

	if trimmed[0] == '[' {
		var channels []map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &channels); err != nil {
			return nil, nil, err
		}
		return channels, channels, nil
	}

	var channel map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &channel); err != nil {
		return nil, nil, err
	}
	return channel, []map[string]json.RawMessage{channel}, nil
}

type bufferedResponseWriter struct {
	header      http.Header
	body        bytes.Buffer
	dst         http.ResponseWriter
	statusCode  int
	wroteHeader bool
	streaming   bool
}

func newBufferedResponseWriter(dst http.ResponseWriter) *bufferedResponseWriter {
	return &bufferedResponseWriter{
		header:     make(http.Header),
		dst:        dst,
		statusCode: http.StatusOK,
	}
}

func (w *bufferedResponseWriter) Header() http.Header {
	return w.header
}

func (w *bufferedResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.statusCode = statusCode
	w.wroteHeader = true
}

func (w *bufferedResponseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.streaming {
		return w.dst.Write(data)
	}
	if len(data) > maxBufferedResponseBytes-w.body.Len() {
		w.startStreaming()
		if _, err := w.dst.Write(w.body.Bytes()); err != nil {
			return 0, err
		}
		w.body.Reset()
		return w.dst.Write(data)
	}
	return w.body.Write(data)
}

func (w *bufferedResponseWriter) writeTo(dst http.ResponseWriter) {
	if w.streaming {
		return
	}
	w.copyHeader(dst)
	dst.WriteHeader(w.statusCode)
	_, _ = dst.Write(w.body.Bytes())
}

func (w *bufferedResponseWriter) startStreaming() {
	w.streaming = true
	w.copyHeader(w.dst)
	w.dst.WriteHeader(w.statusCode)
}

func (w *bufferedResponseWriter) copyHeader(dst http.ResponseWriter) {
	for key, values := range w.header {
		for _, value := range values {
			dst.Header().Add(key, value)
		}
	}
}
