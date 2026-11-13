package file_storage

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestDownloadStatisticsCountOnlyServedFileResponses(t *testing.T) {
	fileID := model.NewId()
	prefix := "/api/v4/files/"

	get := httptest.NewRequest(http.MethodGet, prefix+fileID, nil)
	gotID, ok := downloadedFileID(get, prefix)
	require.True(t, ok)
	require.Equal(t, fileID, gotID)
	require.True(t, isServedFileResponse(http.StatusOK))
	require.True(t, isServedFileResponse(http.StatusPartialContent))
	require.False(t, isServedFileResponse(http.StatusNotModified))
	require.False(t, isServedFileResponse(http.StatusNotFound))

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodHead, prefix+fileID, nil),
		httptest.NewRequest(http.MethodGet, prefix+fileID+"/thumbnail", nil),
		httptest.NewRequest(http.MethodGet, prefix+"invalid", nil),
	} {
		_, ok = downloadedFileID(request, prefix)
		require.False(t, ok)
	}
}

func TestDownloadRecordBurstFallsBackToRequestWithoutLosingCount(t *testing.T) {
	slots := make(chan struct{}, 1)
	started := make(chan string, 1)
	release := make(chan struct{})
	dispatchDownloadRecord(slots, "first", func(fileID string) {
		started <- fileID
		<-release
	})
	require.Equal(t, "first", <-started)
	require.Len(t, slots, 1)

	called := false
	dispatchDownloadRecord(slots, "second", func(fileID string) {
		called = true
		require.Equal(t, "second", fileID)
	})
	require.True(t, called, "a full background pool must still count the download")
	require.Len(t, slots, 1)
	close(release)
	require.Eventually(t, func() bool { return len(slots) == 0 }, time.Second, time.Millisecond)
}

func TestCompressedExtraCountsPreserveEncodingAndAddFileCounts(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	service := &Service{app: th.App}
	original, err := json.Marshal(model.AnalyticsRows{&model.AnalyticsRow{Name: "existing", Value: 7}})
	require.NoError(t, err)
	var compressed bytes.Buffer
	compressor := gzip.NewWriter(&compressed)
	_, err = compressor.Write(original)
	require.NoError(t, err)
	require.NoError(t, compressor.Close())

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(compressed.Len()))
		w.Header().Set("ETag", `"old-body"`)
		_, _ = w.Write(compressed.Bytes())
	})
	response := httptest.NewRecorder()
	service.serveExtraCounts(next, response, httptest.NewRequest(http.MethodGet, "/api/v4/analytics/old?name=extra_counts", nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "gzip", response.Header().Get("Content-Encoding"))
	require.Empty(t, response.Header().Get("Content-Length"))
	require.Empty(t, response.Header().Get("ETag"))
	reader, err := gzip.NewReader(response.Body)
	require.NoError(t, err)
	defer reader.Close()
	var rows model.AnalyticsRows
	require.NoError(t, json.NewDecoder(reader).Decode(&rows))
	values := map[string]float64{}
	for _, row := range rows {
		values[row.Name] = row.Value
	}
	require.Equal(t, float64(7), values["existing"])
	require.Equal(t, float64(0), values[attachedFileCountName])
	require.Equal(t, float64(0), values[downloadedFileCountName])
}

func TestAnalyticsResponseLimitsPreserveOriginalBody(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, err := writer.Write(bytes.Repeat([]byte("x"), maxDecodedAnalyticsBytes+1))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	_, _, err = decodeAnalyticsResponse("gzip", compressed.Bytes())
	require.ErrorContains(t, err, "decoded size limit")

	dst := httptest.NewRecorder()
	buffer := newBufferedResponseWriter(dst)
	buffer.Header().Set("Content-Encoding", "gzip")
	buffer.WriteHeader(http.StatusAccepted)
	first := bytes.Repeat([]byte("a"), maxBufferedAnalyticsBytes)
	_, err = buffer.Write(first)
	require.NoError(t, err)
	require.False(t, buffer.streaming)
	_, err = buffer.Write([]byte("bc"))
	require.NoError(t, err)
	require.True(t, buffer.streaming)
	_, err = buffer.Write([]byte("d"))
	require.NoError(t, err)
	buffer.writeTo(dst)
	require.Equal(t, http.StatusAccepted, dst.Code)
	require.Equal(t, "gzip", dst.Header().Get("Content-Encoding"))
	require.Equal(t, append(first, 'b', 'c', 'd'), dst.Body.Bytes())
}
