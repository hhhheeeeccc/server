package data_retention_job

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

// Keep the durable FileInfo row until every path from its snapshot is gone.
// Unlike the batch application helper, retention cannot discard preview errors:
// that would remove the only record from which a later job can retry cleanup.
// An ambiguous error is still returned after confirming absence, so the caller
// completes only this file's metadata and stops before the next file.
func (w *Worker) removeRetainedFile(rctx request.CTX, file *model.FileInfo) (bool, error) {
	var ambiguous []error
	for _, path := range []string{file.Path, file.PreviewPath, file.ThumbnailPath} {
		if path == "" {
			continue
		}
		appErr := w.app.RemoveFileFromFileStore(rctx, path)
		if appErr == nil || appErr.StatusCode == http.StatusNotFound {
			continue
		}
		// A timeout or lost HTTP reply does not say whether DELETE took effect.
		// Confirm the path's absence through the same configured backend before
		// treating it as removed. An unavailable confirmation preserves FileInfo.
		exists, confirmationErr := w.app.FileExists(path)
		ambiguous = append(ambiguous, fmt.Errorf("remove retained object: %w", appErr))
		if confirmationErr != nil {
			return false, errors.Join(append(ambiguous, fmt.Errorf("confirm retained object absence: %w", confirmationErr))...)
		}
		if exists {
			return false, errors.Join(ambiguous...)
		}
	}
	return true, errors.Join(ambiguous...)
}
