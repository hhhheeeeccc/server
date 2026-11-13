package account_linking

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestMergeOpenIDTrialDatabase(t *testing.T) {
	dsn := os.Getenv("WORKSPACE_OPENID_MERGE_TRIAL_DSN")
	if dsn == "" {
		t.Skip("isolated trial database not configured")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	var databaseName string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&databaseName))
	require.True(t, strings.HasPrefix(databaseName, "openid_merge_trial_"), "refusing to merge outside an isolated trial database")
	var sourceID string
	require.NoError(t, db.QueryRow(`SELECT id FROM users WHERE username='01011217315' AND authservice='openid' AND deleteat=0`).Scan(&sourceID))
	targetID := "43z8mnm71ibminrw3pkgn3thir"
	var targetUsername string
	require.NoError(t, db.QueryRow(`SELECT username FROM users WHERE id=$1 AND authservice='' AND deleteat=0`, targetID).Scan(&targetUsername))
	require.Equal(t, "don", targetUsername)
	const sourceChannelID = "jw81hof46pr85qgtsa3bgpbwpo"
	const targetChannelID = "d7gzb7zrkpbzpjnsy885a9e76o"
	var beforeSource, beforeTarget int
	var sourceTotal, targetTotal int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts WHERE channelid=$1`, sourceChannelID).Scan(&beforeSource))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts WHERE channelid=$1`, targetChannelID).Scan(&beforeTarget))
	require.NoError(t, db.QueryRow(`SELECT totalmsgcount FROM channels WHERE id=$1`, sourceChannelID).Scan(&sourceTotal))
	require.NoError(t, db.QueryRow(`SELECT totalmsgcount FROM channels WHERE id=$1`, targetChannelID).Scan(&targetTotal))
	require.Greater(t, beforeSource, 0)
	require.NoError(t, mergeAccountsDB(context.Background(), db, sourceID, targetID))
	var afterTarget, afterSource int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts WHERE channelid=$1`, targetChannelID).Scan(&afterTarget))
	require.Equal(t, beforeSource+beforeTarget, afterTarget)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts WHERE channelid=$1`, sourceChannelID).Scan(&afterSource))
	require.Zero(t, afterSource)
	var mergedTotal int
	require.NoError(t, db.QueryRow(`SELECT totalmsgcount FROM channels WHERE id=$1`, targetChannelID).Scan(&mergedTotal))
	require.Equal(t, sourceTotal+targetTotal, mergedTotal)
	var merged int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM workspace_openid_account_merges WHERE sourceid=$1 AND targetid=$2`, sourceID, targetID).Scan(&merged))
	require.Equal(t, 1, merged)
	var password string
	require.NoError(t, db.QueryRow(`SELECT password FROM users WHERE id=$1`, targetID).Scan(&password))
	require.Empty(t, password)
}

func TestRepairOpenIDTrialCredential(t *testing.T) {
	dsn := os.Getenv("WORKSPACE_OPENID_MERGE_TRIAL_DSN")
	if dsn == "" {
		t.Skip("isolated trial database not configured")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	var databaseName string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&databaseName))
	require.True(t, strings.HasPrefix(databaseName, "openid_merge_trial_"))
	const targetID = "43z8mnm71ibminrw3pkgn3thir"
	var duplicated bool
	require.NoError(t, db.QueryRow(`SELECT u.password=lc.password AND u.password<>''
		FROM users u JOIN workspace_linked_credentials lc ON lc.userid=u.id
		WHERE u.id=$1`, targetID).Scan(&duplicated))
	require.True(t, duplicated)
	require.NoError(t, clearDuplicatedLinkedPassword(context.Background(), db, targetID))
	var passwordEmpty, credentialPresent bool
	require.NoError(t, db.QueryRow(`SELECT u.password='',lc.password<>''
		FROM users u JOIN workspace_linked_credentials lc ON lc.userid=u.id
		WHERE u.id=$1`, targetID).Scan(&passwordEmpty, &credentialPresent))
	require.True(t, passwordEmpty)
	require.True(t, credentialPresent)
}

func TestUnmergeOpenIDTrialDatabase(t *testing.T) {
	dsn := os.Getenv("WORKSPACE_OPENID_MERGE_TRIAL_DSN")
	if dsn == "" {
		t.Skip("isolated trial database not configured")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	var databaseName string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&databaseName))
	require.True(t, strings.HasPrefix(databaseName, "openid_merge_trial_"))
	const targetID = "43z8mnm71ibminrw3pkgn3thir"
	const sourceID = "9ga57yz9pbgwdkfzpqyfbhzn9y"
	var archived int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM workspace_openid_merge_archive WHERE targetid=$1 AND tablename='target_users'`, targetID).Scan(&archived))
	require.Equal(t, 1, archived)
	actualSource, err := unmergeAccountsDB(context.Background(), db, targetID)
	require.NoError(t, err)
	require.Equal(t, sourceID, actualSource)
	var service, password string
	require.NoError(t, db.QueryRow(`SELECT authservice,password FROM users WHERE id=$1`, targetID).Scan(&service, &password))
	require.Empty(t, service)
	require.NotEmpty(t, password)
	var sourceAuth string
	require.NoError(t, db.QueryRow(`SELECT authservice,authdata FROM users WHERE id=$1`, sourceID).Scan(&service, &sourceAuth))
	require.Equal(t, model.ServiceOpenid, service)
	require.NotEmpty(t, sourceAuth)
	var sourcePosts, targetPosts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts WHERE channelid='jw81hof46pr85qgtsa3bgpbwpo'`).Scan(&sourcePosts))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts WHERE channelid='d7gzb7zrkpbzpjnsy885a9e76o'`).Scan(&targetPosts))
	require.Equal(t, 5, sourcePosts)
	require.Equal(t, 8, targetPosts)
}

func TestUnmergeOpenIDCurrentStateCopy(t *testing.T) {
	dsn := os.Getenv("WORKSPACE_OPENID_UNMERGE_CURRENT_DSN")
	if dsn == "" {
		t.Skip("isolated current-state copy not configured")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	var databaseName string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&databaseName))
	require.True(t, strings.HasPrefix(databaseName, "openid_merge_trial_"))
	const targetID = "43z8mnm71ibminrw3pkgn3thir"
	const sourceID = "9ga57yz9pbgwdkfzpqyfbhzn9y"
	var expectedPosts, targetPosts, newLocalPosts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts p JOIN workspace_openid_account_merges m ON m.targetid=$1
		WHERE p.userid=$1 AND p.createat>m.createat AND p.channelid NOT IN ('jw81hof46pr85qgtsa3bgpbwpo','d7gzb7zrkpbzpjnsy885a9e76o')`, targetID).Scan(&newLocalPosts))
	require.Greater(t, newLocalPosts, 0)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM workspace_openid_merge_archive
		WHERE targetid=$1 AND tablename='direct_posts' AND rowdata->>'channelid'='jw81hof46pr85qgtsa3bgpbwpo'`, targetID).Scan(&expectedPosts))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts WHERE channelid='d7gzb7zrkpbzpjnsy885a9e76o'`).Scan(&targetPosts))
	require.Greater(t, expectedPosts, 0)
	actualSource, err := unmergeAccountsDBWithOptions(context.Background(), db, targetID, true)
	require.NoError(t, err)
	require.Equal(t, sourceID, actualSource)
	var restoredSourcePosts, restoredTargetPosts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts WHERE channelid='jw81hof46pr85qgtsa3bgpbwpo'`).Scan(&restoredSourcePosts))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts WHERE channelid='d7gzb7zrkpbzpjnsy885a9e76o'`).Scan(&restoredTargetPosts))
	require.Equal(t, expectedPosts, restoredSourcePosts)
	require.Equal(t, targetPosts-expectedPosts, restoredTargetPosts)
	var remainingLocalPosts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts WHERE userid=$1 AND channelid NOT IN ('jw81hof46pr85qgtsa3bgpbwpo','d7gzb7zrkpbzpjnsy885a9e76o')`, targetID).Scan(&remainingLocalPosts))
	require.GreaterOrEqual(t, remainingLocalPosts, newLocalPosts)
	var targetService, sourceService string
	require.NoError(t, db.QueryRow(`SELECT authservice FROM users WHERE id=$1`, targetID).Scan(&targetService))
	require.NoError(t, db.QueryRow(`SELECT authservice FROM users WHERE id=$1`, sourceID).Scan(&sourceService))
	require.Empty(t, targetService)
	require.Equal(t, model.ServiceOpenid, sourceService)
}

func TestRelinkOpenIDTrialUnmerge(t *testing.T) {
	dsn := os.Getenv("WORKSPACE_OPENID_RELINK_UNMERGE_DSN")
	if dsn == "" {
		t.Skip("isolated relink copy not configured")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	var databaseName string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&databaseName))
	require.True(t, strings.HasPrefix(databaseName, "openid_merge_trial_"))
	const targetID = "43z8mnm71ibminrw3pkgn3thir"
	const sourceID = "9ga57yz9pbgwdkfzpqyfbhzn9y"
	const sourceChannelID = "jw81hof46pr85qgtsa3bgpbwpo"
	const targetChannelID = "d7gzb7zrkpbzpjnsy885a9e76o"
	var archivedPreference string
	require.NoError(t, db.QueryRow(`SELECT rowdata->>'value' FROM workspace_openid_merge_archive
		WHERE sourceid=$1 AND targetid=$2 AND tablename='direct_preferences_source'
		AND rowdata->>'userid'=$1 AND rowdata->>'name'=$3 LIMIT 1`, sourceID, targetID, sourceChannelID).Scan(&archivedPreference))
	var localPreference string
	require.NoError(t, db.QueryRow(`SELECT value FROM preferences WHERE userid=$1 AND category='channel_open_time' AND name=$2`, targetID, targetChannelID).Scan(&localPreference))
	actualSource, err := unmergeAccountsDBWithOptions(context.Background(), db, targetID, true)
	require.NoError(t, err)
	require.Equal(t, sourceID, actualSource)
	var restoredSourcePreference, restoredLocalPreference string
	require.NoError(t, db.QueryRow(`SELECT value FROM preferences WHERE userid=$1 AND category='channel_open_time' AND name=$2`, sourceID, sourceChannelID).Scan(&restoredSourcePreference))
	require.NoError(t, db.QueryRow(`SELECT value FROM preferences WHERE userid=$1 AND category='channel_open_time' AND name=$2`, targetID, targetChannelID).Scan(&restoredLocalPreference))
	require.Equal(t, archivedPreference, restoredSourcePreference)
	require.Equal(t, localPreference, restoredLocalPreference)
}

func TestMain(m *testing.M) {
	mainHelper := testlib.NewMainHelper()
	defer mainHelper.Close()
	api4.SetMainHelper(mainHelper)
	mainHelper.Main(m)
}

func setupMergeTest(tb testing.TB) *api4.TestHelper {
	th := api4.SetupConfig(tb, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	})
	return th
}

func TestOpenIDAuthFailureDoesNotSaveLocalCredential(t *testing.T) {
	th := setupMergeTest(t).InitBasic(t)
	service := &Service{app: th.App}
	service.initCredentials()
	target, owner := th.BasicUser, th.BasicUser2
	target, appErr := th.App.GetUser(th.Context, target.Id)
	require.Nil(t, appErr)
	subject := "openid-occupied-" + model.NewId()
	_, err := th.Store.User().UpdateAuthData(owner.Id, model.ServiceOpenid, &subject, "", false)
	require.NoError(t, err)
	appErr = service.updateOpenIDAuthWithCredential(context.Background(), target, subject)
	require.NotNil(t, appErr)
	require.Equal(t, 409, appErr.StatusCode)
	var credentialCount int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM workspace_linked_credentials WHERE userid=$1`, target.Id).Scan(&credentialCount))
	require.Zero(t, credentialCount)
	var authService, password string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT authservice,password FROM users WHERE id=$1`, target.Id).Scan(&authService, &password))
	require.Empty(t, authService)
	require.Equal(t, target.Password, password)
}

func TestOpenIDAuthAndLocalCredentialCommitTogether(t *testing.T) {
	th := setupMergeTest(t).InitBasic(t)
	service := &Service{app: th.App}
	service.initCredentials()
	target, appErr := th.App.GetUser(th.Context, th.BasicUser.Id)
	require.Nil(t, appErr)
	subject := "openid-available-" + model.NewId()
	require.Nil(t, service.updateOpenIDAuthWithCredential(context.Background(), target, subject))
	var serviceName, password, credential string
	var authData sql.NullString
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT authservice,authdata,password FROM users WHERE id=$1`, target.Id).
		Scan(&serviceName, &authData, &password))
	require.Equal(t, model.ServiceOpenid, serviceName)
	require.Equal(t, subject, authData.String)
	require.Empty(t, password)
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT password FROM workspace_linked_credentials WHERE userid=$1`, target.Id).Scan(&credential))
	require.Equal(t, target.Password, credential)
}

func TestFastOpenIDLinkKeepsBothAccountsDataAndCanUnlink(t *testing.T) {
	th := setupMergeTest(t).InitBasic(t)
	service := &Service{app: th.App}
	service.initCredentials()
	service.initMergeMetadata()
	service.initFastLinks()
	ctx := context.Background()
	target, source := th.BasicUser, th.BasicUser2
	sourceClient := th.CreateClient()
	th.LoginBasic2WithClient(t, sourceClient)
	sourcePost := th.CreatePostWithClient(t, sourceClient, th.BasicChannel)
	targetClient := th.CreateClient()
	th.LoginBasicWithClient(t, targetClient)
	targetPost := th.CreatePostWithClient(t, targetClient, th.BasicChannel)
	var localPassword string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT password FROM users WHERE id=$1`, target.Id).Scan(&localPassword))
	subject := "openid-fast-" + model.NewId()
	_, err := th.Store.User().UpdateAuthData(source.Id, model.ServiceOpenid, &subject, "", false)
	require.NoError(t, err)
	th.Store.User().ClearCaches()
	require.NoError(t, linkOpenIDIdentityDB(ctx, th.Store.GetInternalMasterDB(), source.Id, target.Id))
	th.Store.User().ClearCaches()
	resolved, err := th.Store.User().GetByAuth(&subject, model.ServiceOpenid)
	require.NoError(t, err)
	require.Equal(t, target.Id, resolved.Id)
	for _, post := range []struct{ id, owner string }{{sourcePost.Id, source.Id}, {targetPost.Id, target.Id}} {
		stored, storeErr := th.Store.Post().GetSingle(th.Context, post.id, false)
		require.NoError(t, storeErr)
		require.Equal(t, post.owner, stored.UserId)
	}
	newPost := th.CreatePostWithClient(t, targetClient, th.BasicChannel)
	var savedPassword string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT password FROM workspace_linked_credentials WHERE userid=$1`, target.Id).Scan(&savedPassword))
	require.Equal(t, localPassword, savedPassword)
	changedSubject := "changed-" + model.NewId()
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE users SET authdata=$2 WHERE id=$1`, target.Id, changedSubject)
	require.NoError(t, err)
	_, err = unlinkOpenIDIdentityDB(ctx, th.Store.GetInternalMasterDB(), target.Id)
	var blocked *mergeBlockedError
	require.ErrorAs(t, err, &blocked)
	var activeLinkCount int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM workspace_openid_fast_links WHERE targetid=$1`, target.Id).Scan(&activeLinkCount))
	require.Equal(t, 1, activeLinkCount)
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE users SET authdata=$2 WHERE id=$1`, target.Id, subject)
	require.NoError(t, err)
	unlinkedSource, err := unlinkOpenIDIdentityDB(ctx, th.Store.GetInternalMasterDB(), target.Id)
	require.NoError(t, err)
	require.Equal(t, source.Id, unlinkedSource)
	th.Store.User().ClearCaches()
	resolved, err = th.Store.User().GetByAuth(&subject, model.ServiceOpenid)
	require.NoError(t, err)
	require.Equal(t, source.Id, resolved.Id)
	var restoredPassword string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT password FROM users WHERE id=$1`, target.Id).Scan(&restoredPassword))
	require.Equal(t, localPassword, restoredPassword)
	for _, post := range []struct{ id, owner string }{{sourcePost.Id, source.Id}, {targetPost.Id, target.Id}, {newPost.Id, target.Id}} {
		stored, storeErr := th.Store.Post().GetSingle(th.Context, post.id, false)
		require.NoError(t, storeErr)
		require.Equal(t, post.owner, stored.UserId)
	}
	// The same account can be selected again after an unlink.
	require.NoError(t, linkOpenIDIdentityDB(ctx, th.Store.GetInternalMasterDB(), source.Id, target.Id))
}

func TestUnmergeCurrentArchivedLinkOnIsolatedCopy(t *testing.T) {
	dsn := os.Getenv("WORKSPACE_OPENID_UNMERGE_CURRENT_DSN")
	if dsn == "" {
		t.Skip("isolated current-state copy not configured")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	var databaseName string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&databaseName))
	require.True(t, strings.HasPrefix(databaseName, "openid_merge_trial_"), "refusing to unmerge outside an isolated trial database")
	const targetID = "43z8mnm71ibminrw3pkgn3thir"
	const sourceID = "9ga57yz9pbgwdkfzpqyfbhzn9y"
	var archivedPosts, archivedCategories int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM workspace_openid_merge_archive
		WHERE sourceid=$1 AND targetid=$2 AND tablename='posts'`, sourceID, targetID).Scan(&archivedPosts))
	require.Greater(t, archivedPosts, 0)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM workspace_openid_merge_archive
		WHERE sourceid=$1 AND targetid=$2 AND tablename='sidebarcategories'`, sourceID, targetID).Scan(&archivedCategories))
	require.Greater(t, archivedCategories, 0)
	restoredSource, err := unmergeAccountsDBWithOptions(context.Background(), db, targetID, true)
	require.NoError(t, err)
	require.Equal(t, sourceID, restoredSource)
	var restoredPosts, restoredCategories int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts WHERE userid=$1`, sourceID).Scan(&restoredPosts))
	require.GreaterOrEqual(t, restoredPosts, archivedPosts)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sidebarcategories WHERE userid=$1`, sourceID).Scan(&restoredCategories))
	require.Equal(t, archivedCategories, restoredCategories)
	var sourceService, targetService string
	require.NoError(t, db.QueryRow(`SELECT authservice FROM users WHERE id=$1`, sourceID).Scan(&sourceService))
	require.NoError(t, db.QueryRow(`SELECT authservice FROM users WHERE id=$1`, targetID).Scan(&targetService))
	require.Equal(t, model.ServiceOpenid, sourceService)
	require.Empty(t, targetService)
}

func TestFastRelinkAfterLegacyUnmergeOnIsolatedCopy(t *testing.T) {
	dsn := os.Getenv("WORKSPACE_OPENID_RELINK_CURRENT_DSN")
	if dsn == "" {
		t.Skip("isolated unmerged copy not configured")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	var databaseName string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&databaseName))
	require.True(t, strings.HasPrefix(databaseName, "openid_merge_trial_"))
	const targetID = "43z8mnm71ibminrw3pkgn3thir"
	const sourceID = "9ga57yz9pbgwdkfzpqyfbhzn9y"
	var before int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts WHERE userid=$1`, sourceID).Scan(&before))
	require.NoError(t, linkOpenIDIdentityDB(context.Background(), db, sourceID, targetID))
	var after int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM posts WHERE userid=$1`, sourceID).Scan(&after))
	require.Equal(t, before, after)
	var sourceAuth, targetAuth sql.NullString
	require.NoError(t, db.QueryRow(`SELECT authdata FROM users WHERE id=$1`, sourceID).Scan(&sourceAuth))
	require.NoError(t, db.QueryRow(`SELECT authdata FROM users WHERE id=$1`, targetID).Scan(&targetAuth))
	require.False(t, sourceAuth.Valid)
	require.True(t, targetAuth.Valid)
}

func TestOpenIDProfileNamePersistsThroughAppUpdate(t *testing.T) {
	th := setupMergeTest(t).InitBasic(t)
	user := th.BasicUser2
	subject := "openid-name-" + model.NewId()
	_, err := th.Store.User().UpdateAuthData(user.Id, model.ServiceOpenid, &subject, "", false)
	require.NoError(t, err)
	th.Store.User().ClearCaches()
	user, appErr := th.App.GetUser(th.Context, user.Id)
	require.Nil(t, appErr)
	user.FirstName = "Edited"
	user.LastName = "Name"
	updated, appErr := th.App.UpdateUser(th.Context, user, true)
	require.Nil(t, appErr)
	require.Equal(t, "Edited", updated.FirstName)
	stored, err := th.Store.User().GetByAuth(&subject, model.ServiceOpenid)
	require.NoError(t, err)
	require.Equal(t, "Edited", stored.FirstName)
	require.Equal(t, "Name", stored.LastName)
}

func TestMergeOpenIDAccountMovesPostsAndPreservesLocalCredential(t *testing.T) {
	th := setupMergeTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	(&Service{app: th.App}).initCredentials()
	ctx := context.Background()
	target := th.BasicUser
	source := th.BasicUser2
	var originalHash string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT password FROM users WHERE id=$1`, target.Id).Scan(&originalHash))
	sourceClient := th.CreateClient()
	th.LoginBasic2WithClient(t, sourceClient)
	post := th.CreatePostWithClient(t, sourceClient, th.BasicChannel)
	sourceOnlyChannel := th.CreateChannelWithClient(t, sourceClient, model.ChannelTypePrivate)
	for _, item := range []struct{ userID, value string }{{target.Id, "local"}, {source.Id, "openid"}} {
		_, prefErr := th.Store.GetInternalMasterDB().Exec(`INSERT INTO preferences (userid,category,name,value) VALUES ($1,'merge_test','setting',$2)`, item.userID, item.value)
		require.NoError(t, prefErr)
	}
	fileID := model.NewId()
	_, storeErr := th.Store.GetInternalMasterDB().Exec(`INSERT INTO fileinfo (id,creatorid,postid,channelid) VALUES ($1,$2,$3,$4)`, fileID, source.Id, post.Id, th.BasicChannel.Id)
	require.NoError(t, storeErr)
	subject := "openid-merge-" + model.NewId()
	_, storeErr = th.Store.User().UpdateAuthData(source.Id, model.ServiceOpenid, &subject, "", false)
	require.NoError(t, storeErr)
	th.Store.User().ClearCaches()
	err := (&Service{app: th.App}).mergeAccounts(ctx, source.Id, target.Id)
	require.NoError(t, err)
	th.Store.User().ClearCaches()
	storedPost, storeErr := th.Store.Post().GetSingle(th.Context, post.Id, false)
	require.NoError(t, storeErr)
	require.Equal(t, target.Id, storedPost.UserId)
	var fileOwner string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT creatorid FROM fileinfo WHERE id=$1`, fileID).Scan(&fileOwner))
	require.Equal(t, target.Id, fileOwner)
	var sourceMemberships int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM teammembers WHERE userid=$1`, source.Id).Scan(&sourceMemberships))
	require.Zero(t, sourceMemberships)
	var targetMemberships int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM channelmembers WHERE userid=$1 AND channelid=$2`, target.Id, sourceOnlyChannel.Id).Scan(&targetMemberships))
	require.Equal(t, 1, targetMemberships)
	var preference string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT value FROM preferences WHERE userid=$1 AND category='merge_test' AND name='setting'`, target.Id).Scan(&preference))
	require.Equal(t, "local", preference)
	var archived int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM workspace_openid_merge_archive WHERE sourceid=$1 AND targetid=$2 AND tablename='preferences' AND rowdata->>'value'='openid'`, source.Id, target.Id).Scan(&archived))
	require.Equal(t, 1, archived)
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM channelmembers WHERE userid=$1`, source.Id).Scan(&sourceMemberships))
	require.Zero(t, sourceMemberships)
	resolved, storeErr := th.Store.User().GetByAuth(&subject, model.ServiceOpenid)
	require.NoError(t, storeErr)
	require.Equal(t, target.Id, resolved.Id)
	storedSource, storeErr := th.Store.User().Get(th.Context, source.Id)
	require.NoError(t, storeErr)
	require.NotZero(t, storedSource.DeleteAt)
	var storedHash string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT password FROM workspace_linked_credentials WHERE userid=$1`, target.Id).Scan(&storedHash))
	require.Equal(t, originalHash, storedHash)
	var userPassword string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT password FROM users WHERE id=$1`, target.Id).Scan(&userPassword))
	require.Empty(t, userPassword)
}

func TestMergeOpenIDAccountRejectsEmbeddedReferenceWithoutChangingAccounts(t *testing.T) {
	th := setupMergeTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	(&Service{app: th.App}).initCredentials()
	ctx := context.Background()
	target, source := th.BasicUser, th.BasicUser2
	sourceClient := th.CreateClient()
	th.LoginBasic2WithClient(t, sourceClient)
	post := th.CreatePostWithClient(t, sourceClient, th.BasicChannel)
	_, storeErr := th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET props=$2 WHERE id=$1`, post.Id, fmt.Sprintf(`{"linked_user":%q}`, source.Id))
	require.NoError(t, storeErr)
	subject := "openid-merge-blocked-" + model.NewId()
	_, storeErr = th.Store.User().UpdateAuthData(source.Id, model.ServiceOpenid, &subject, "", false)
	require.NoError(t, storeErr)
	th.Store.User().ClearCaches()
	err := (&Service{app: th.App}).mergeAccounts(ctx, source.Id, target.Id)
	require.Error(t, err)
	var blocked *mergeBlockedError
	require.ErrorAs(t, err, &blocked)
	storedPost, storeErr := th.Store.Post().GetSingle(th.Context, post.Id, false)
	require.NoError(t, storeErr)
	require.Equal(t, source.Id, storedPost.UserId)
	resolved, storeErr := th.Store.User().GetByAuth(&subject, model.ServiceOpenid)
	require.NoError(t, storeErr)
	require.Equal(t, source.Id, resolved.Id)
	var linkedCount int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM workspace_linked_credentials WHERE userid=$1`, target.Id).Scan(&linkedCount))
	require.Zero(t, linkedCount)
}

func TestMergeOpenIDAccountRejectsPluginBinaryReference(t *testing.T) {
	th := setupMergeTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	(&Service{app: th.App}).initCredentials()
	target, source := th.BasicUser, th.BasicUser2
	subject := "openid-plugin-" + model.NewId()
	_, err := th.Store.User().UpdateAuthData(source.Id, model.ServiceOpenid, &subject, "", false)
	require.NoError(t, err)
	_, err = th.Store.GetInternalMasterDB().Exec(`INSERT INTO pluginkeyvaluestore(pluginid,pkey,pvalue) VALUES ('merge-test',$1,convert_to($2,'UTF8'))`, model.NewId(), `{"user_id":"`+source.Id+`"}`)
	require.NoError(t, err)
	th.Store.User().ClearCaches()
	err = (&Service{app: th.App}).mergeAccounts(context.Background(), source.Id, target.Id)
	require.Error(t, err)
	var blocked *mergeBlockedError
	require.ErrorAs(t, err, &blocked)
	resolved, err := th.Store.User().GetByAuth(&subject, model.ServiceOpenid)
	require.NoError(t, err)
	require.Equal(t, source.Id, resolved.Id)
}

func TestMergeOpenIDAccountMovesDirectMessagesWithoutDuplicateChannel(t *testing.T) {
	th := setupMergeTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	(&Service{app: th.App}).initCredentials()
	target, source := th.BasicUser, th.BasicUser2
	peer := th.CreateUser(t)
	sourceClient := th.CreateClient()
	th.LoginBasic2WithClient(t, sourceClient)
	direct, _, err := sourceClient.CreateDirectChannel(context.Background(), source.Id, peer.Id)
	require.NoError(t, err)
	post := th.CreatePostWithClient(t, sourceClient, direct)
	subject := "openid-direct-" + model.NewId()
	_, err = th.Store.User().UpdateAuthData(source.Id, model.ServiceOpenid, &subject, "", false)
	require.NoError(t, err)
	th.Store.User().ClearCaches()
	require.NoError(t, (&Service{app: th.App}).mergeAccounts(context.Background(), source.Id, target.Id))
	var name string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT name FROM channels WHERE id=$1`, direct.Id).Scan(&name))
	require.Equal(t, model.GetDMNameFromIds(target.Id, peer.Id), name)
	var membershipCount int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM channelmembers WHERE channelid=$1 AND userid=$2`, direct.Id, target.Id).Scan(&membershipCount))
	require.Equal(t, 1, membershipCount)
	storedPost, err := th.Store.Post().GetSingle(th.Context, post.Id, false)
	require.NoError(t, err)
	require.Equal(t, target.Id, storedPost.UserId)
	require.Equal(t, direct.Id, storedPost.ChannelId)
}

func TestMergeOpenIDAccountCombinesOverlappingDirectChannels(t *testing.T) {
	th := setupMergeTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	(&Service{app: th.App}).initCredentials()
	target, source := th.BasicUser, th.BasicUser2
	peer := th.CreateUser(t)
	sourceClient := th.CreateClient()
	th.LoginBasic2WithClient(t, sourceClient)
	sourceDirect, _, err := sourceClient.CreateDirectChannel(context.Background(), source.Id, peer.Id)
	require.NoError(t, err)
	sourcePost := th.CreatePostWithClient(t, sourceClient, sourceDirect)
	targetDirect, _, err := th.Client.CreateDirectChannel(context.Background(), target.Id, peer.Id)
	require.NoError(t, err)
	targetPost := th.CreatePostWithClient(t, th.Client, targetDirect)
	for _, item := range []struct{ userID, category, name, value string }{
		{source.Id, "channel_open_time", sourceDirect.Id, "source"},
		{target.Id, "channel_open_time", targetDirect.Id, "local"},
		{peer.Id, "channel_open_time", sourceDirect.Id, "peer-source"},
		{peer.Id, "channel_open_time", targetDirect.Id, "peer-target"},
		{peer.Id, "channel_approximate_view_time", sourceDirect.Id, "peer-view"},
	} {
		_, err = th.Store.GetInternalMasterDB().Exec(`INSERT INTO preferences(userid,category,name,value) VALUES($1,$2,$3,$4)`, item.userID, item.category, item.name, item.value)
		require.NoError(t, err)
	}
	fileID := model.NewId()
	_, err = th.Store.GetInternalMasterDB().Exec(`INSERT INTO fileinfo(id,creatorid,postid,channelid) VALUES($1,$2,$3,$4)`, fileID, source.Id, sourcePost.Id, sourceDirect.Id)
	require.NoError(t, err)
	subject := "openid-direct-overlap-" + model.NewId()
	_, err = th.Store.User().UpdateAuthData(source.Id, model.ServiceOpenid, &subject, "", false)
	require.NoError(t, err)
	th.Store.User().ClearCaches()
	err = (&Service{app: th.App}).mergeAccounts(context.Background(), source.Id, target.Id)
	require.NoError(t, err)
	storedPost, err := th.Store.Post().GetSingle(th.Context, sourcePost.Id, false)
	require.NoError(t, err)
	require.Equal(t, target.Id, storedPost.UserId)
	require.Equal(t, targetDirect.Id, storedPost.ChannelId)
	storedTargetPost, err := th.Store.Post().GetSingle(th.Context, targetPost.Id, false)
	require.NoError(t, err)
	require.Equal(t, targetDirect.Id, storedTargetPost.ChannelId)
	var fileChannel string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT channelid FROM fileinfo WHERE id=$1`, fileID).Scan(&fileChannel))
	require.Equal(t, targetDirect.Id, fileChannel)
	var oldMembers int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM channelmembers WHERE channelid=$1`, sourceDirect.Id).Scan(&oldMembers))
	require.Zero(t, oldMembers)
	var mergedMembers int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM channelmembers WHERE channelid=$1`, targetDirect.Id).Scan(&mergedMembers))
	require.Equal(t, 2, mergedMembers)
	for _, item := range []struct{ userID, category, value string }{
		{target.Id, "channel_open_time", "local"},
		{peer.Id, "channel_open_time", "peer-target"},
		{peer.Id, "channel_approximate_view_time", "peer-view"},
	} {
		var value string
		require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT value FROM preferences WHERE userid=$1 AND category=$2 AND name=$3`, item.userID, item.category, targetDirect.Id).Scan(&value))
		require.Equal(t, item.value, value)
	}
	var sourceChannelPreferences int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM preferences WHERE name=$1`, sourceDirect.Id).Scan(&sourceChannelPreferences))
	require.Zero(t, sourceChannelPreferences)
	var archivedPreferences int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM workspace_openid_merge_archive WHERE sourceid=$1 AND targetid=$2 AND tablename='direct_preferences_source'`, source.Id, target.Id).Scan(&archivedPreferences))
	require.Equal(t, 3, archivedPreferences)
	resolved, err := th.Store.User().GetByAuth(&subject, model.ServiceOpenid)
	require.NoError(t, err)
	require.Equal(t, target.Id, resolved.Id)
}

func TestMergeOpenIDAccountMovesGroupMessagesWithoutDuplicateChannel(t *testing.T) {
	th := setupMergeTest(t).InitBasic(t)
	th.App.Srv().SetLicense(model.NewTestLicense())
	(&Service{app: th.App}).initCredentials()
	target, source := th.BasicUser, th.BasicUser2
	peerOne, peerTwo := th.CreateUser(t), th.CreateUser(t)
	sourceClient := th.CreateClient()
	th.LoginBasic2WithClient(t, sourceClient)
	group, _, err := sourceClient.CreateGroupChannel(context.Background(), []string{source.Id, peerOne.Id, peerTwo.Id})
	require.NoError(t, err)
	post := th.CreatePostWithClient(t, sourceClient, group)
	subject := "openid-group-" + model.NewId()
	_, err = th.Store.User().UpdateAuthData(source.Id, model.ServiceOpenid, &subject, "", false)
	require.NoError(t, err)
	th.Store.User().ClearCaches()
	require.NoError(t, (&Service{app: th.App}).mergeAccounts(context.Background(), source.Id, target.Id))
	var name string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT name FROM channels WHERE id=$1`, group.Id).Scan(&name))
	require.Equal(t, model.GetGroupNameFromUserIds([]string{target.Id, peerOne.Id, peerTwo.Id}), name)
	storedPost, err := th.Store.Post().GetSingle(th.Context, post.Id, false)
	require.NoError(t, err)
	require.Equal(t, target.Id, storedPost.UserId)
	require.Equal(t, group.Id, storedPost.ChannelId)
}
