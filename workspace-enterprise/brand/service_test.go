package brand_test

import (
	"bytes"
	"context"
	_ "embed"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"testing"

	_ "github.com/mattermost/enterprise"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

//go:embed default_brand_image.png
var expectedDefaultImage []byte

func TestBrandImageLicenseFallbackAndUploadedImage(t *testing.T) {
	directory := t.TempDir()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.FileSettings.Directory = &directory
	}).InitBasic(t)
	ctx := context.Background()
	_, err := th.Client.Logout(ctx)
	require.NoError(t, err)
	_, response, err := th.Client.GetBrandImage(ctx)
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)

	th.App.Srv().SetLicense(model.NewTestLicense())
	fallback, _, err := th.Client.GetBrandImage(ctx)
	require.NoError(t, err, "the public brand route should work without a session")
	require.Equal(t, expectedDefaultImage, fallback)
	_, err = png.DecodeConfig(bytes.NewReader(fallback))
	require.NoError(t, err)

	custom := image.NewRGBA(image.Rect(0, 0, 2, 2))
	custom.Set(0, 0, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, custom))
	_, err = th.SystemAdminClient.UploadBrandImage(ctx, encoded.Bytes())
	require.NoError(t, err)
	uploaded, _, err := th.Client.GetBrandImage(ctx)
	require.NoError(t, err)
	require.NotEqual(t, fallback, uploaded)
	config, err := png.DecodeConfig(bytes.NewReader(uploaded))
	require.NoError(t, err)
	require.Equal(t, 2, config.Width)
	require.Equal(t, 2, config.Height)

	th.App.Srv().SetLicense(nil)
	_, response, err = th.Client.GetBrandImage(ctx)
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode, "a stored custom image must not bypass the license gate")
}

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}
