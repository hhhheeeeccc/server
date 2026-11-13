package enterprise_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestAutoTranslationConditionalHistoryCentralRegistration(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.AutoTranslation = new(true)
	require.True(t, th.Server.SetLicense(license))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.FeatureFlags.AutoTranslation = true
		cfg.AutoTranslationSettings.Enable = new(true)
		cfg.AutoTranslationSettings.Provider = new("libretranslate")
		cfg.AutoTranslationSettings.LibreTranslate.URL = new("http://127.0.0.1:1")
	})
	channel := th.BasicChannel.DeepCopy()
	channel.AutoTranslation = true
	_, appErr := th.App.UpdateChannel(th.Context, channel)
	require.Nil(t, appErr)
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE users SET locale='ar' WHERE id=$1`, th.BasicUser.Id)
	require.NoError(t, err)
	th.Server.Store().AutoTranslation().InvalidateUserLocaleCache(th.BasicUser.Id)
	digest := sha256.Sum256([]byte(th.BasicPost.Message))
	require.NoError(t, th.Store.AutoTranslation().Save(&model.Translation{
		ObjectType: model.TranslationObjectTypePost, ObjectID: th.BasicPost.Id, ChannelID: channel.Id,
		Provider: "libretranslate", Lang: "ar", Type: model.TranslationTypeString,
		Text: "ترجمة اختبار التسجيل المركزي", NormHash: hex.EncodeToString(digest[:]), State: model.TranslationStateReady,
	}))
	posts, first, err := th.Client.GetPostsForChannel(th.Context.Context(), channel.Id, 0, 50, "", false, false)
	require.NoError(t, err)
	require.Contains(t, first.Etag, `"workspace-translations-`)
	require.NotEmpty(t, posts.Posts[th.BasicPost.Id].Metadata.Translations)
	_, err = th.Client.UpdateChannelMemberAutotranslation(th.Context.Context(), channel.Id, th.BasicUser.Id, true)
	require.NoError(t, err)
	updated, second, err := th.Client.GetPostsForChannel(th.Context.Context(), channel.Id, 0, 50, first.Etag, false, false)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, second.StatusCode)
	require.NotNil(t, updated)
	require.Empty(t, updated.Posts[th.BasicPost.Id].Metadata.Translations)
	_, unchanged, err := th.Client.GetPostsForChannel(th.Context.Context(), channel.Id, 0, 50, second.Etag, false, false)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotModified, unchanged.StatusCode)
}
