package file_storage_test

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	_ "github.com/mattermost/enterprise"
	"github.com/mattermost/enterprise/internal/objectstoretest"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestStorageCapacityRouteRequiresAdminAndReturnsLocalCapacity(t *testing.T) {
	directory := t.TempDir()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.FileSettings.Directory = &directory
	}).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())

	response, err := th.SystemAdminClient.DoAPIGet(th.Context.Context(), "/file_storage/usage", "")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var usage struct {
		DriverName      string `json:"driver_name"`
		HasCapacityInfo bool   `json:"has_capacity_info"`
		TotalBytes      int64  `json:"total_bytes"`
		FreeBytes       int64  `json:"free_bytes"`
		UsedBytes       int64  `json:"used_bytes"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&usage))
	require.Equal(t, model.ImageDriverLocal, usage.DriverName)
	require.True(t, usage.HasCapacityInfo)
	require.Positive(t, usage.TotalBytes)
	require.GreaterOrEqual(t, usage.FreeBytes, int64(0))
	require.Equal(t, usage.TotalBytes, usage.FreeBytes+usage.UsedBytes)

	denied, err := th.Client.DoAPIGet(th.Context.Context(), "/file_storage/usage", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, denied.StatusCode)
}

func TestDownloadStatisticsIgnoreHeadAndCountServedGet(t *testing.T) {
	directory := t.TempDir()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.FileSettings.Directory = &directory
		cfg.ServiceSettings.WebserverMode = new("gzip")
	}).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	ctx := th.Context.Context()
	uploaded, _, err := th.Client.UploadFile(ctx, []byte("download statistics"), th.BasicChannel.Id, "statistics.txt")
	require.NoError(t, err)
	require.Len(t, uploaded.FileInfos, 1)
	fileID := uploaded.FileInfos[0].Id
	_, _, err = th.Client.CreatePost(ctx, &model.Post{ChannelId: th.BasicChannel.Id, FileIds: []string{fileID}})
	require.NoError(t, err)

	response, err := th.Client.DoAPIRequestWithHeaders(ctx, http.MethodHead, "/files/"+fileID, "", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	var count int64
	db := th.Store.GetInternalMasterDB()
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM workspace_file_downloads WHERE fileid=$1`, fileID).Scan(&count))
	require.Zero(t, count)

	notModified, err := th.Client.DoAPIRequestWithHeaders(ctx, http.MethodGet, "/files/"+fileID, "", map[string]string{
		"If-Modified-Since": time.Now().Add(24 * time.Hour).UTC().Format(http.TimeFormat),
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusNotModified, notModified.StatusCode)
	require.NoError(t, notModified.Body.Close())
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM workspace_file_downloads WHERE fileid=$1`, fileID).Scan(&count))
	require.Zero(t, count)

	_, fileResponse, err := th.Client.GetFile(ctx, fileID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, fileResponse.StatusCode)
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT downloadcount FROM workspace_file_downloads WHERE fileid=$1`, fileID).Scan(&count) == nil && count == 1
	}, 5*time.Second, 25*time.Millisecond)

	ranged, err := th.Client.DoAPIRequestWithHeaders(ctx, http.MethodGet, "/files/"+fileID, "", map[string]string{"Range": "bytes=0-3"})
	require.NoError(t, err)
	require.Equal(t, http.StatusPartialContent, ranged.StatusCode)
	require.NoError(t, ranged.Body.Close())
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT downloadcount FROM workspace_file_downloads WHERE fileid=$1`, fileID).Scan(&count) == nil && count == 2
	}, 5*time.Second, 25*time.Millisecond)

	rows, _, err := th.SystemAdminClient.GetAnalyticsOld(ctx, "extra_counts", "")
	require.NoError(t, err)
	values := make(map[string]float64, len(rows))
	for _, row := range rows {
		values[row.Name] = row.Value
	}
	require.Equal(t, float64(1), values["total_attached_file_count"])
	require.Equal(t, float64(1), values["total_downloaded_file_count"])

	_, denied, err := th.Client.GetAnalyticsOld(ctx, "extra_counts", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, denied.StatusCode)

	th.App.Srv().SetLicense(nil)
	unlicensedRows, _, err := th.SystemAdminClient.GetAnalyticsOld(ctx, "extra_counts", "")
	require.NoError(t, err)
	for _, row := range unlicensedRows {
		require.NotEqual(t, "total_attached_file_count", row.Name)
		require.NotEqual(t, "total_downloaded_file_count", row.Name)
	}
}

func TestFileStatisticsWithObjectStoreEmulators(t *testing.T) {
	for _, emulator := range objectstoretest.Cases() {
		t.Run(emulator.Name, func(t *testing.T) {
			endpoint := os.Getenv(emulator.EndpointEnv)
			if endpoint == "" {
				t.Skipf("set %s to run against an object store emulator", emulator.EndpointEnv)
			}
			fileSettings := emulator.Setup(t, endpoint)
			th := api4.SetupConfig(t, func(actual *model.Config) {
				actual.CacheSettings.CacheType = new(model.CacheTypeLRU)
				actual.FileSettings = fileSettings
				actual.ServiceSettings.AllowedUntrustedInternalConnections = model.NewPointer("127.0.0.1")
			}).InitBasic(t)
			th.App.Srv().SetLicense(model.NewTestLicense())
			ctx := th.Context.Context()
			content := []byte("object store download statistics")
			uploaded, _, err := th.Client.UploadFile(ctx, content, th.BasicChannel.Id, "statistics.txt")
			require.NoError(t, err)
			require.Len(t, uploaded.FileInfos, 1)
			fileID := uploaded.FileInfos[0].Id
			_, _, err = th.Client.CreatePost(ctx, &model.Post{ChannelId: th.BasicChannel.Id, FileIds: []string{fileID}})
			require.NoError(t, err)
			got, _, err := th.Client.GetFile(ctx, fileID)
			require.NoError(t, err)
			require.Equal(t, content, got)
			var count int64
			db := th.Store.GetInternalMasterDB()
			require.Eventually(t, func() bool {
				return db.QueryRow(`SELECT downloadcount FROM workspace_file_downloads WHERE fileid=$1`, fileID).Scan(&count) == nil && count == 1
			}, 5*time.Second, 25*time.Millisecond)

			response, err := th.SystemAdminClient.DoAPIGet(ctx, "/file_storage/usage", "")
			require.NoError(t, err)
			defer response.Body.Close()
			var usage struct {
				DriverName      string `json:"driver_name"`
				HasCapacityInfo bool   `json:"has_capacity_info"`
				UsedBytes       int64  `json:"used_bytes"`
			}
			require.NoError(t, json.NewDecoder(response.Body).Decode(&usage))
			require.Equal(t, *fileSettings.DriverName, usage.DriverName)
			require.False(t, usage.HasCapacityInfo)
			require.GreaterOrEqual(t, usage.UsedBytes, int64(len(content)))

			rows, _, err := th.SystemAdminClient.GetAnalyticsOld(ctx, "extra_counts", "")
			require.NoError(t, err)
			values := make(map[string]float64, len(rows))
			for _, row := range rows {
				values[row.Name] = row.Value
			}
			require.Equal(t, float64(1), values["total_attached_file_count"])
			require.Equal(t, float64(1), values["total_downloaded_file_count"])
		})
	}
}

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}
