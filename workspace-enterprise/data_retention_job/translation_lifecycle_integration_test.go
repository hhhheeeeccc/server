package data_retention_job

import (
	"context"
	"testing"
	"time"

	"github.com/mattermost/enterprise/autotranslation"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestRetentionWorkerDeletesExpiredTranslationsAndKeepsProtectedSources(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.FeatureFlags.AutoTranslation = false
		cfg.AutoTranslationSettings.Enable = new(false)
		cfg.DataRetentionSettings.EnableMessageDeletion = new(true)
		cfg.DataRetentionSettings.MessageRetentionDays = new(0)
		cfg.DataRetentionSettings.MessageRetentionHours = new(1)
		cfg.DataRetentionSettings.PreservePinnedPosts = new(true)
		cfg.DataRetentionSettings.TimeBetweenBatchesMilliseconds = new(0)
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.AutoTranslation = new(false)
	require.True(t, th.Server.SetLicense(license))
	db := th.Store.GetInternalMasterDB()
	expired := th.BasicPost
	current := th.CreatePost(t)
	pinned, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: expired.ChannelId,
		UserId: expired.UserId, Message: "Pinned original retained by policy", IsPinned: true})
	require.NoError(t, err)
	reply, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: expired.ChannelId,
		UserId: th.BasicUser2.Id, RootId: expired.Id, Message: "Expired reply"})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE posts SET createat=$1 WHERE id IN ($2,$3,$4)`,
		time.Now().Add(-48*time.Hour).UnixMilli(), expired.Id, pinned.Id, reply.Id)
	require.NoError(t, err)
	seedRetentionPostTranslations(t, th, expired, reply, current, pinned)
	require.NoError(t, th.Store.AutoTranslation().Save(&model.Translation{
		ObjectType: "other_object", ObjectID: expired.Id, Lang: "ar", Provider: "libretranslate",
		Type: model.TranslationTypeString, Text: "independent object", State: model.TranslationStateReady,
	}))
	worker := New(th.Server).MakeWorker().(*Worker)
	ctx, cancel := context.WithTimeout(th.Context.Context(), 20*time.Second)
	defer cancel()
	reason, err := worker.execute(th.Context.WithContext(ctx), th.Context.Logger(), make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, continueWork, reason)
	for _, post := range []*model.Post{expired, reply, current, pinned} {
		wantPosts, wantTranslations := 0, 0
		if post.Id == current.Id || post.Id == pinned.Id {
			wantPosts, wantTranslations = 1, 2
		}
		var posts, translations int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts WHERE id=$1`, post.Id).Scan(&posts))
		require.Equal(t, wantPosts, posts)
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM translations WHERE objecttype='post' AND objectid=$1`, post.Id).Scan(&translations))
		require.Equal(t, wantTranslations, translations)
	}
	var others int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM translations WHERE objecttype='other_object' AND objectid=$1`, expired.Id).Scan(&others))
	require.Equal(t, 1, others)
}

func seedRetentionPostTranslations(t *testing.T, th *api4.TestHelper, posts ...*model.Post) {
	t.Helper()
	require.NoError(t, autotranslation.InstallPostLifecycle(th.Context.Context(), th.Store.GetInternalMasterDB()))
	for _, post := range posts {
		for _, language := range []string{"ar", "fr"} {
			require.NoError(t, th.Store.AutoTranslation().Save(&model.Translation{
				ObjectType: model.TranslationObjectTypePost, ObjectID: post.Id, ChannelID: post.ChannelId,
				Lang: language, Provider: "libretranslate", Type: model.TranslationTypeString,
				Text: "derived text", State: model.TranslationStateReady,
			}))
		}
	}
}
