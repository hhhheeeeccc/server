package video_thumbnails

import (
	"bytes"
	"context"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattermost/enterprise/access_control"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	app.RegisterComplianceInterface(func(a *app.App) einterfaces.ComplianceInterface { Register(a); return nil })
	app.RegisterAccessControlServiceInterface(access_control.New)
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func videoFixture(t *testing.T) []byte {
	t.Helper()
	if !isFFmpegAvailable() {
		t.Skip("FFmpeg is required for the real video integration test")
	}
	file := filepath.Join(t.TempDir(), "short.mp4")
	cmd := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=red:s=64x48:r=10", "-t", "0.2", "-threads", "1", "-c:v", "mpeg4", "-an", file)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	return data
}

func setupVideoAPI(t *testing.T) *api4.TestHelper {
	t.Helper()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	return th
}

func TestVideoUploadPreviewAndPostMetadata(t *testing.T) {
	data := videoFixture(t)
	th := setupVideoAPI(t)
	ctx := th.Context.Context()
	upload, _, err := th.Client.UploadFile(ctx, data, th.BasicChannel.Id, "short.mp4")
	require.NoError(t, err)
	require.Len(t, upload.FileInfos, 1)
	info := upload.FileInfos[0]
	require.True(t, info.HasPreviewImage)
	require.NotNil(t, info.MiniPreview)
	mini, err := jpeg.DecodeConfig(bytes.NewReader(*info.MiniPreview))
	require.NoError(t, err)
	require.Equal(t, 16, mini.Width)
	require.Equal(t, 16, mini.Height)
	stored, err := th.Store.FileInfo().GetFromMaster(info.Id)
	require.NoError(t, err)
	require.NotEmpty(t, stored.ThumbnailPath)
	require.NotEmpty(t, stored.PreviewPath)
	original, appErr := th.App.ReadFile(stored.Path)
	require.Nil(t, appErr)
	require.Equal(t, data, original)
	thumb, response, err := th.Client.GetFileThumbnail(ctx, info.Id)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	frame, err := jpeg.DecodeConfig(bytes.NewReader(thumb))
	require.NoError(t, err)
	require.LessOrEqual(t, frame.Width, 1280)
	require.LessOrEqual(t, frame.Height, 720)
	preview, _, err := th.Client.GetFilePreview(ctx, info.Id)
	require.NoError(t, err)
	require.Equal(t, thumb, preview)
	post, _, err := th.Client.CreatePost(ctx, &model.Post{ChannelId: th.BasicChannel.Id, Message: "video fixture", FileIds: []string{info.Id}})
	require.NoError(t, err)
	infos, _, err := th.Client.GetFileInfosForPost(ctx, post.Id, "")
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.True(t, infos[0].HasPreviewImage)
	_, _, err = th.Client.GetFileInfo(ctx, info.Id)
	require.NoError(t, err)
	updated, err := th.Store.FileInfo().GetFromMaster(info.Id)
	require.NoError(t, err)
	require.Equal(t, stored.ThumbnailPath, updated.ThumbnailPath, "repeated reads must reuse the persisted preview")
	require.Equal(t, post.Id, updated.PostId)

	// The resumable upload path returns a single FileInfo, not file_infos.
	session, _, err := th.Client.CreateUpload(ctx, &model.UploadSession{ChannelId: th.BasicChannel.Id, Filename: "resumable.mp4", FileSize: int64(len(data)), Type: model.UploadTypeAttachment})
	require.NoError(t, err)
	resumed, _, err := th.Client.UploadData(ctx, session.Id, bytes.NewReader(data))
	require.NoError(t, err)
	require.True(t, resumed.HasPreviewImage)
}

func TestVideoExistingFilesRespectLicenseMembershipAndABAC(t *testing.T) {
	data := videoFixture(t)
	th := setupVideoAPI(t)
	ctx := th.Context.Context()
	channel := th.CreateChannelWithClient(t, th.Client, model.ChannelTypePrivate)
	th.Server.SetLicense(nil)
	upload, _, err := th.Client.UploadFile(ctx, data, channel.Id, "old.mp4")
	require.NoError(t, err)
	id := upload.FileInfos[0].Id
	require.False(t, upload.FileInfos[0].HasPreviewImage)
	_, response, err := th.Client.GetFileThumbnail(ctx, id)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	other := th.CreateClient()
	_, _, err = other.Login(ctx, th.BasicUser2.Email, th.BasicUser2.Password)
	require.NoError(t, err)
	_, response, err = other.GetFileInfo(ctx, id)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	_, response, err = other.GetFileThumbnail(ctx, id)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	stored, err := th.Store.FileInfo().GetFromMaster(id)
	require.NoError(t, err)
	require.False(t, stored.HasPreviewImage, "unauthorized reads must not generate output")

	th.ConfigStore.SetReadOnlyFF(false)
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.FeatureFlags.PermissionPolicies = true
		cfg.FeatureFlags.ChannelPermissionPolicies = true
		cfg.AccessControlSettings.EnableAttributeBasedAccessControl = new(true)
	})
	_, _, err = th.SystemAdminClient.CreateAccessControlPolicy(ctx, &model.AccessControlPolicy{
		ID: channel.Id, Type: model.AccessControlPolicyTypeChannel,
		Rules: []model.AccessControlPolicyRule{{Name: "download", Role: model.ChannelUserRoleId, Actions: []string{model.AccessControlPolicyActionDownloadFileAttachment}, Expression: "false"}},
	})
	require.NoError(t, err)
	_, response, err = th.Client.GetFileThumbnail(ctx, id)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	stored, err = th.Store.FileInfo().GetFromMaster(id)
	require.NoError(t, err)
	require.False(t, stored.HasPreviewImage)
	_, _, err = th.SystemAdminClient.CreateAccessControlPolicy(ctx, &model.AccessControlPolicy{
		ID: channel.Id, Type: model.AccessControlPolicyTypeChannel,
		Rules: []model.AccessControlPolicyRule{{Name: "download", Role: model.ChannelUserRoleId, Actions: []string{model.AccessControlPolicyActionDownloadFileAttachment}, Expression: "true"}},
	})
	require.NoError(t, err)
	thumb, response, err := th.Client.GetFileThumbnail(ctx, id)
	require.NoError(t, err, "the normal thumbnail API generates a missing authorized video preview")
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Contains(t, response.Header.Get("Content-Type"), "image/jpeg")
	_, err = jpeg.DecodeConfig(bytes.NewReader(thumb))
	require.NoError(t, err)
}

func TestVideoUnavailableDecoderAndInvalidInputPreserveUpload(t *testing.T) {
	data := videoFixture(t)
	th := setupVideoAPI(t)
	ctx := th.Context.Context()
	t.Run("missing decoder", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		upload, _, err := th.Client.UploadFile(ctx, data, th.BasicChannel.Id, "no-decoder.mp4")
		require.NoError(t, err)
		require.False(t, upload.FileInfos[0].HasPreviewImage)
	})
	upload, _, err := th.Client.UploadFile(ctx, data[:48], th.BasicChannel.Id, "broken.mp4")
	require.NoError(t, err)
	require.False(t, upload.FileInfos[0].HasPreviewImage)
	stored, err := th.Store.FileInfo().GetFromMaster(upload.FileInfos[0].Id)
	require.NoError(t, err)
	require.Empty(t, stored.ThumbnailPath)
	require.Empty(t, stored.PreviewPath)
}

func TestVideoResponseBufferStreamsLargeAndBinaryResponses(t *testing.T) {
	for _, errorsOnly := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		buffer := &responseBuffer{ResponseWriter: recorder, errorsOnly: errorsOnly}
		data := strings.Repeat("x", 3*1024*1024)
		_, err := buffer.Write([]byte(data))
		require.NoError(t, err)
		buffer.flush()
		require.True(t, buffer.sent)
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Equal(t, data, recorder.Body.String())
		require.Zero(t, buffer.body.Len())
	}
}

func TestRealShortVideoFallsBackToFirstFrame(t *testing.T) {
	data := videoFixture(t)
	frame, err := extractVideoThumbnail(context.Background(), io.NopCloser(bytes.NewReader(data)), int64(len(data)))
	require.NoError(t, err)
	_, err = jpeg.DecodeConfig(bytes.NewReader(frame))
	require.NoError(t, err)
}
