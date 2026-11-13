package message_export

import (
	"context"
	"net/http"
	"os"
	"path"
	"testing"
	"time"

	"github.com/mattermost/enterprise/internal/objectstoretest"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/stretchr/testify/require"
)

func TestCSVExportWithObjectStoreEmulators(t *testing.T) {
	for _, emulator := range objectstoretest.Cases() {
		t.Run(emulator.Name, func(t *testing.T) {
			endpoint := os.Getenv(emulator.EndpointEnv)
			if endpoint == "" {
				t.Skipf("set %s to test CSV export with this object store", emulator.EndpointEnv)
			}
			for _, dedicated := range []bool{false, true} {
				name := "shared"
				if dedicated {
					name = "dedicated"
				}
				t.Run(name, func(t *testing.T) {
					testCSVExportWithObjectStore(t, emulator, endpoint, dedicated)
				})
			}
		})
	}
}

func testCSVExportWithObjectStore(t *testing.T, emulator objectstoretest.Emulator, endpoint string, dedicated bool) {
	t.Helper()
	previousPolling := jobs.DefaultWatcherPollingInterval
	jobs.DefaultWatcherPollingInterval = 100
	t.Cleanup(func() { jobs.DefaultWatcherPollingInterval = previousPolling })

	fileSettings := emulator.Setup(t, endpoint)
	var exportSettings model.FileSettings
	if dedicated {
		exportSettings = emulator.Setup(t, endpoint)
		if *fileSettings.DriverName == model.ImageDriverS3 {
			require.NotEqual(t, *fileSettings.AmazonS3Bucket, *exportSettings.AmazonS3Bucket)
		} else {
			require.NotEqual(t, *fileSettings.AzureContainer, *exportSettings.AzureContainer)
		}
	}
	format := model.ComplianceExportTypeCsv
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.FileSettings = fileSettings
		if dedicated {
			configureDedicatedExportStore(&cfg.FileSettings, exportSettings)
		}
		cfg.MessageExportSettings.EnableExport = new(true)
		cfg.MessageExportSettings.DownloadExportResults = new(true)
		cfg.MessageExportSettings.ExportFormat = &format
		cfg.ServiceSettings.AllowedUntrustedInternalConnections = new("127.0.0.1")
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("message_export")))
	require.NoError(t, th.Server.Jobs.StartWorkers())
	t.Cleanup(func() { require.NoError(t, th.Server.Jobs.StopWorkers()) })

	start := model.GetMillis() - 1
	content := []byte("workspace enterprise object store export attachment")
	uploaded, _, err := th.Client.UploadFile(context.Background(), content, th.BasicChannel.Id, "object-store-proof.txt")
	require.NoError(t, err)
	require.Len(t, uploaded.FileInfos, 1)
	fileID := uploaded.FileInfos[0].Id
	message := "workspace enterprise object store export " + emulator.Name
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{
		ChannelId: th.BasicChannel.Id,
		Message:   message,
		FileIds:   []string{fileID},
	})
	require.NoError(t, err)
	storedFile, fileErr := th.App.GetFileInfo(th.Context, fileID)
	require.Nil(t, fileErr)
	require.NotEmpty(t, storedFile.Path)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	job, appErr := th.App.MessageExport().StartSynchronizeJob(th.Context.WithContext(ctx), start)
	require.Nil(t, appErr)
	require.Equal(t, model.JobStatusSuccess, job.Status, "job data: %#v", job.Data)
	archive, response, err := th.SystemAdminClient.DownloadJob(ctx, job.Id)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.True(t, archiveContainsCSVMessage(t, archive, message))
	expectedPath := path.Join("files", post.Id, fileID+"-"+path.Base(storedFile.Path))
	require.True(t, archiveContainsFile(t, archive, expectedPath, content))
}

func configureDedicatedExportStore(fileSettings *model.FileSettings, export model.FileSettings) {
	fileSettings.DedicatedExportStore = new(true)
	fileSettings.ExportDriverName = export.DriverName
	if *export.DriverName == model.ImageDriverS3 {
		fileSettings.ExportAmazonS3AccessKeyId = export.AmazonS3AccessKeyId
		fileSettings.ExportAmazonS3SecretAccessKey = export.AmazonS3SecretAccessKey
		fileSettings.ExportAmazonS3Bucket = export.AmazonS3Bucket
		fileSettings.ExportAmazonS3Region = export.AmazonS3Region
		fileSettings.ExportAmazonS3Endpoint = export.AmazonS3Endpoint
		fileSettings.ExportAmazonS3SSL = export.AmazonS3SSL
	} else {
		fileSettings.ExportAzureStorageAccount = export.AzureStorageAccount
		fileSettings.ExportAzureAuthMode = export.AzureAuthMode
		fileSettings.ExportAzureAccessKey = export.AzureAccessKey
		fileSettings.ExportAzureContainer = export.AzureContainer
		fileSettings.ExportAzureCloud = export.AzureCloud
		fileSettings.ExportAzureEndpoint = export.AzureEndpoint
		fileSettings.ExportAzureSSL = export.AzureSSL
	}
}
