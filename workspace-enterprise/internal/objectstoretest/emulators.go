package objectstoretest

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
	"github.com/stretchr/testify/require"
)

const (
	minioUser      = "workspace_test"
	minioPassword  = "workspace_test_secret"
	azuriteAccount = "devstoreaccount1"
	azuriteKey     = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
)

type Emulator struct {
	Name        string
	EndpointEnv string
	Setup       func(*testing.T, string) model.FileSettings
}

func Cases() []Emulator {
	return []Emulator{
		{Name: "s3", EndpointEnv: "WORKSPACE_TEST_MINIO_ENDPOINT", Setup: setupS3},
		{Name: "azure", EndpointEnv: "WORKSPACE_TEST_AZURITE_ENDPOINT", Setup: setupAzure},
	}
}

func setupS3(t *testing.T, endpoint string) model.FileSettings {
	t.Helper()
	settings := filestore.FileBackendSettings{
		DriverName: model.ImageDriverS3, AmazonS3AccessKeyId: minioUser,
		AmazonS3SecretAccessKey: minioPassword, AmazonS3Endpoint: endpoint,
		AmazonS3Bucket: "workspace-" + model.NewId(), AmazonS3Region: "us-east-1",
		AmazonS3RequestTimeoutMilliseconds: 30000,
	}
	backend, err := filestore.NewS3FileBackend(settings)
	require.NoError(t, err)
	require.NoError(t, backend.MakeBucket())

	fs := model.FileSettings{}
	fs.SetDefaults(false)
	fs.DriverName = model.NewPointer(settings.DriverName)
	fs.AmazonS3AccessKeyId = model.NewPointer(settings.AmazonS3AccessKeyId)
	fs.AmazonS3SecretAccessKey = model.NewPointer(settings.AmazonS3SecretAccessKey)
	fs.AmazonS3Endpoint = model.NewPointer(settings.AmazonS3Endpoint)
	fs.AmazonS3Bucket = model.NewPointer(settings.AmazonS3Bucket)
	fs.AmazonS3Region = model.NewPointer(settings.AmazonS3Region)
	fs.AmazonS3SSL = model.NewPointer(false)
	return fs
}

func setupAzure(t *testing.T, endpoint string) model.FileSettings {
	t.Helper()
	settings := filestore.FileBackendSettings{
		DriverName: model.ImageDriverAzure, AzureStorageAccount: azuriteAccount,
		AzureAuthMode: model.AzureAuthModeSharedKey, AzureAccessKey: azuriteKey,
		AzureContainer: "workspace-" + model.NewId(), AzureCloud: model.AzureCloudCustom,
		AzureEndpoint: endpoint, AllowedUntrustedInternalConnections: "127.0.0.1",
		AzureRequestTimeoutMilliseconds: 30000,
	}
	backend, err := filestore.NewAzureFileBackend(settings)
	require.NoError(t, err)
	require.NoError(t, backend.MakeContainer())

	fs := model.FileSettings{}
	fs.SetDefaults(false)
	fs.DriverName = model.NewPointer(settings.DriverName)
	fs.AzureStorageAccount = model.NewPointer(settings.AzureStorageAccount)
	fs.AzureAuthMode = model.NewPointer(settings.AzureAuthMode)
	fs.AzureAccessKey = model.NewPointer(settings.AzureAccessKey)
	fs.AzureContainer = model.NewPointer(settings.AzureContainer)
	fs.AzureCloud = model.NewPointer(settings.AzureCloud)
	fs.AzureEndpoint = model.NewPointer(settings.AzureEndpoint)
	fs.AzureSSL = model.NewPointer(false)
	return fs
}
