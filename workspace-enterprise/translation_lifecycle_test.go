package enterprise_test

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestTranslationLifecycleCentralRegistration(t *testing.T) {
	// This package imports the actual root init, rather than the lightweight
	// registration used by the translation package's own test harness.
	th := setupEnterpriseTest(t).InitBasic(t)
	require.Nil(t, th.Server.License())
	db := th.Store.GetInternalMasterDB()
	var triggers int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_trigger WHERE
		tgrelid IN ('posts'::regclass,'translations'::regclass) AND NOT tgisinternal
		AND tgname IN ('workspace_translation_source_guard','workspace_delete_post_translations','workspace_truncate_post_translations')
		AND tgenabled='O'`).Scan(&triggers))
	require.Equal(t, 3, triggers, "central startup must install deletion hygiene even without a license")
	translation := &model.Translation{ObjectType: model.TranslationObjectTypePost, ObjectID: th.BasicPost.Id,
		Lang: "ar", Provider: "libretranslate", Type: model.TranslationTypeString,
		Text: "derived private text", State: model.TranslationStateReady}
	require.NoError(t, th.Store.AutoTranslation().Save(translation))
	_, err := th.SystemAdminClient.DeletePost(th.Context.Context(), th.BasicPost.Id)
	require.NoError(t, err)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM translations WHERE objecttype='post' AND objectid=$1`, th.BasicPost.Id).Scan(&count))
	require.Zero(t, count)
	require.Error(t, th.Store.AutoTranslation().Save(translation), "a delayed Save must not resurrect the derived text")
}
