package file_storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
)

const (
	attachedFileCountName        = "total_attached_file_count"
	downloadedFileCountName      = "total_downloaded_file_count"
	maxConcurrentDownloadRecords = 32
	maxBufferedAnalyticsBytes    = 4 << 20
	maxDecodedAnalyticsBytes     = 8 << 20
)

func (s *Service) initializeDownloadStatistics() error {
	_, err := s.app.Srv().Store().GetInternalMasterDB().Exec(`
		CREATE TABLE IF NOT EXISTS workspace_file_downloads (
			fileid VARCHAR(26) PRIMARY KEY,
			downloadcount BIGINT NOT NULL DEFAULT 0,
			lastdownloadat BIGINT NOT NULL DEFAULT 0
		)
	`)
	return err
}

func (s *Service) analyticsAndDownloadMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.app.License() == nil {
			next.ServeHTTP(w, r)
			return
		}

		analyticsPath, filesPathPrefix := s.statisticsPaths()
		if r.Method == http.MethodGet && r.URL.Path == analyticsPath && r.URL.Query().Get("name") == "extra_counts" {
			s.serveExtraCounts(next, w, r)
			return
		}

		if fileID, ok := downloadedFileID(r, filesPathPrefix); ok {
			writer := &statusResponseWriter{ResponseWriter: w}
			next.ServeHTTP(writer, r)
			if isServedFileResponse(writer.StatusCode()) {
				dispatchDownloadRecord(s.downloadRecordSlots, fileID, s.recordDownload)
			}
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Bound background writes under download bursts. When all slots are busy,
// write on the request goroutine so a successful download is still counted.
func dispatchDownloadRecord(slots chan struct{}, fileID string, record func(string)) {
	select {
	case slots <- struct{}{}:
		go func() {
			defer func() { <-slots }()
			record(fileID)
		}()
	default:
		record(fileID)
	}
}

func (s *Service) statisticsPaths() (string, string) {
	subpath, err := utils.GetSubpathFromConfig(s.app.Config())
	if err != nil {
		subpath = "/"
	}
	apiRoot := path.Join(subpath, model.APIURLSuffix)
	return path.Join(apiRoot, "analytics/old"), path.Join(apiRoot, "files") + "/"
}

func downloadedFileID(r *http.Request, filesPathPrefix string) (string, bool) {
	if r.Method != http.MethodGet {
		return "", false
	}
	if !strings.HasPrefix(r.URL.Path, filesPathPrefix) {
		return "", false
	}

	fileID := strings.TrimPrefix(r.URL.Path, filesPathPrefix)
	if strings.Contains(fileID, "/") || !model.IsValidId(fileID) {
		return "", false
	}
	return fileID, true
}

func isServedFileResponse(status int) bool {
	return status == http.StatusOK || status == http.StatusPartialContent
}

func (s *Service) serveExtraCounts(next http.Handler, w http.ResponseWriter, r *http.Request) {
	recorder := newBufferedResponseWriter(w)
	next.ServeHTTP(recorder, r)
	if recorder.streaming {
		return
	}

	if recorder.statusCode < http.StatusOK || recorder.statusCode >= http.StatusMultipleChoices {
		recorder.writeTo(w)
		return
	}

	responseBody, encodeResponse, err := decodeAnalyticsResponse(recorder.header.Get("Content-Encoding"), recorder.body.Bytes())
	if err != nil {
		s.app.Log().Warn("Unable to decode compressed file analytics", mlog.Err(err))
		recorder.writeTo(w)
		return
	}
	var rows model.AnalyticsRows
	if err := json.Unmarshal(responseBody, &rows); err != nil {
		s.app.Log().Warn("Unable to decode extra file analytics", mlog.Err(err))
		recorder.writeTo(w)
		return
	}

	attached, downloaded, err := s.fileCounts(r.Context())
	if err != nil {
		s.app.Log().Warn("Unable to read extra file analytics", mlog.Err(err))
		recorder.writeTo(w)
		return
	}
	rows = setAnalyticsValue(rows, attachedFileCountName, attached)
	rows = setAnalyticsValue(rows, downloadedFileCountName, downloaded)

	body, err := json.Marshal(rows)
	if err != nil {
		s.app.Log().Warn("Unable to encode extra file analytics", mlog.Err(err))
		recorder.writeTo(w)
		return
	}
	body = append(body, '\n')
	body, err = encodeResponse(body)
	if err != nil {
		s.app.Log().Warn("Unable to encode compressed file analytics", mlog.Err(err))
		recorder.writeTo(w)
		return
	}
	recorder.body.Reset()
	_, _ = recorder.body.Write(body)
	recorder.header.Del("Content-Length")
	recorder.header.Del("ETag")
	recorder.writeTo(w)
}

func decodeAnalyticsResponse(contentEncoding string, body []byte) ([]byte, func([]byte) ([]byte, error), error) {
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		if len(body) > maxDecodedAnalyticsBytes {
			return nil, nil, fmt.Errorf("analytics response exceeds decoded size limit")
		}
		return body, func(data []byte) ([]byte, error) { return data, nil }, nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		decoded, err := io.ReadAll(io.LimitReader(reader, maxDecodedAnalyticsBytes+1))
		closeErr := reader.Close()
		if err != nil {
			return nil, nil, err
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}
		if len(decoded) > maxDecodedAnalyticsBytes {
			return nil, nil, fmt.Errorf("analytics response exceeds decoded size limit")
		}
		return decoded, func(data []byte) ([]byte, error) {
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			if _, err := writer.Write(data); err != nil {
				return nil, err
			}
			if err := writer.Close(); err != nil {
				return nil, err
			}
			return compressed.Bytes(), nil
		}, nil
	default:
		return nil, nil, fmt.Errorf("unsupported analytics content encoding %q", contentEncoding)
	}
}

func (s *Service) fileCounts(ctx context.Context) (int64, int64, error) {
	db := s.app.Srv().Store().GetInternalMasterDB()
	var attached int64
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM FileInfo
		WHERE PostId <> '' AND DeleteAt = 0
	`).Scan(&attached); err != nil {
		return 0, 0, err
	}

	var downloaded int64
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM workspace_file_downloads downloads
		INNER JOIN FileInfo files ON files.Id = downloads.fileid
		WHERE downloads.downloadcount > 0 AND files.DeleteAt = 0
	`).Scan(&downloaded); err != nil {
		return attached, 0, err
	}

	return attached, downloaded, nil
}

func setAnalyticsValue(rows model.AnalyticsRows, name string, value int64) model.AnalyticsRows {
	for _, row := range rows {
		if row.Name == name {
			row.Value = float64(value)
			return rows
		}
	}
	return append(rows, &model.AnalyticsRow{Name: name, Value: float64(value)})
}

func (s *Service) recordDownload(fileID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := s.app.Srv().Store().GetInternalMasterDB().ExecContext(ctx, `
		INSERT INTO workspace_file_downloads (fileid, downloadcount, lastdownloadat)
		VALUES ($1, 1, $2)
		ON CONFLICT (fileid) DO UPDATE SET
			downloadcount = workspace_file_downloads.downloadcount + 1,
			lastdownloadat = EXCLUDED.lastdownloadat
	`, fileID, model.GetMillis())
	if err != nil {
		s.app.Log().Warn("Unable to record a file download", mlog.String("file_id", fileID), mlog.Err(err))
	}
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
	return &bufferedResponseWriter{header: make(http.Header), dst: dst, statusCode: http.StatusOK}
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
	if len(data) > maxBufferedAnalyticsBytes-w.body.Len() {
		w.streaming = true
		w.copyHeaders(w.dst)
		w.dst.WriteHeader(w.statusCode)
		if _, err := w.dst.Write(w.body.Bytes()); err != nil {
			return 0, err
		}
		w.body.Reset()
		return w.dst.Write(data)
	}
	return w.body.Write(data)
}

func (w *bufferedResponseWriter) writeTo(destination http.ResponseWriter) {
	if w.streaming {
		return
	}
	w.copyHeaders(destination)
	destination.WriteHeader(w.statusCode)
	_, _ = destination.Write(w.body.Bytes())
}

func (w *bufferedResponseWriter) copyHeaders(destination http.ResponseWriter) {
	for key, values := range w.header {
		for _, value := range values {
			destination.Header().Add(key, value)
		}
	}
}

type statusResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *statusResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *statusResponseWriter) Write(data []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func (w *statusResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *statusResponseWriter) StatusCode() int {
	if w.statusCode == 0 {
		return http.StatusOK
	}
	return w.statusCode
}
