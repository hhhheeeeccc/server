package enterprise_test

import (
	"testing"
	"time"

	"github.com/mattermost/enterprise/account_migration"
	"github.com/mattermost/enterprise/autotranslation"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestAutoTranslationSourceOrderingCentralRegistration(t *testing.T) {
	th := setupEnterpriseTest(t).InitBasic(t)
	require.Nil(t, th.Server.License())
	before := th.Server.Store()
	account_migration.RegisterPrimarySessionLookupsBeforeStart(th.Server)
	account_migration.RegisterPrimarySessionLookupsBeforeStart(th.Server)
	autotranslation.RegisterPostWrites(th.App)
	autotranslation.RegisterPostWrites(th.App)
	require.Same(t, before, th.Server.Store(), "registration through other Enterprise wrappers must remain idempotent")
	post, _, err := th.SystemAdminClient.CreatePost(th.Context.Context(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "Initial source for central ordering."})
	require.NoError(t, err)
	stamp := time.Now().Add(time.Minute).UnixMilli()
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET updateat=$1 WHERE id=$2`, stamp, post.Id)
	require.NoError(t, err)
	for i, message := range []string{"First edit after a clock moves backward.", "Second edit after a clock moves backward."} {
		post, _, err = th.SystemAdminClient.PatchPost(th.Context.Context(), post.Id, &model.PostPatch{Message: new(message)})
		require.NoError(t, err)
		require.Equal(t, message, post.Message)
		require.Equal(t, stamp+int64(i)+1, post.UpdateAt, "native responses must use the committed source order even without a license")
		if post.Metadata != nil {
			require.Empty(t, post.Metadata.Translations, "ordering hygiene must not unlock translation")
		}
		var committed int64
		require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT updateat FROM posts WHERE id=$1`, post.Id).Scan(&committed))
		require.Equal(t, committed, post.UpdateAt)
	}
}
