package disappearing_messages

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
)

func (s *Service) protectFiles(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := fileRouteParts(r.URL.Path, s.app.Config())
		if len(parts) < 4 {
			next.ServeHTTP(w, r)
			return
		}
		// Byte, preview, thumbnail, info, link and public routes share this ID.
		// Keep permission enforcement in the original native handlers.
		ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
		var expired bool
		err := s.repo.db.QueryRowContext(ctx, `SELECT m.expires_at<=floor(extract(epoch FROM clock_timestamp())*1000)::bigint FROM fileinfo f JOIN workspace_message_expiry.messages m ON m.post_id=f.postid WHERE f.id=$1`, parts[3]).Scan(&expired)
		cancel()
		if errors.Is(err, sql.ErrNoRows) {
			next.ServeHTTP(w, r)
			return
		}
		if err != nil {
			writeFileError(w, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		r.Header.Del("If-None-Match")
		r.Header.Del("If-Modified-Since")
		if expired || (len(parts) > 4 && (parts[4] == "public" || parts[4] == "link")) {
			writeFileError(w, http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeFileError(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError(status, "الملف غير متاح."))
}

func fileRouteParts(requestPath string, config *model.Config) []string {
	// Router middleware sees the full URL path, including the configured subpath.
	subpath, err := utils.GetSubpathFromConfig(config)
	if err != nil {
		return nil
	}
	subpath = strings.TrimSuffix(subpath, "/")
	if subpath != "" && !strings.HasPrefix(requestPath, subpath+"/") {
		return nil
	}
	requestPath = strings.TrimPrefix(requestPath, subpath)
	parts := strings.Split(strings.Trim(requestPath, "/"), "/")
	if len(parts) < 4 || parts[0] != "api" || parts[1] != "v4" || parts[2] != "files" {
		return nil
	}
	return parts
}
