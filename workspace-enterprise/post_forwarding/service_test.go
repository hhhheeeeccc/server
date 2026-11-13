package post_forwarding_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	_ "github.com/mattermost/enterprise"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func setup(t *testing.T) *api4.TestHelper {
	t.Helper()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.EmailSettings.SendPushNotifications = new(false)
		cfg.FileSettings.ExtractContent = new(false)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	return th
}

func upload(t *testing.T, th *api4.TestHelper, data []byte, name string) (*model.Post, *model.FileInfo) {
	t.Helper()
	response, _, err := th.Client.UploadFile(context.Background(), data, th.BasicChannel.Id, name)
	require.NoError(t, err)
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "رسالة بمرفق", FileIds: model.StringArray{response.FileInfos[0].Id}})
	require.NoError(t, err)
	info, err := th.Store.FileInfo().GetFromMaster(response.FileInfos[0].Id)
	require.NoError(t, err)
	return post, info
}

func forward(t *testing.T, th *api4.TestHelper, postID, channelID string, ids []string, status int) *model.FileUploadResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{"channel_id": channelID, "file_ids": ids})
	require.NoError(t, err)
	r, err := th.Client.DoAPIPost(context.Background(), "/workspace/posts/"+postID+"/files/forward", string(body))
	if status < 400 {
		require.NoError(t, err)
	} else {
		require.Error(t, err)
	}
	require.NotNil(t, r)
	defer r.Body.Close()
	require.Equal(t, status, r.StatusCode)
	if status >= 400 {
		return nil
	}
	encoded, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	require.Less(t, len(encoded), 32<<10, "the response must contain metadata, not attachment bytes")
	var result model.FileUploadResponse
	require.NoError(t, json.Unmarshal(encoded, &result))
	return &result
}

func TestForwardFilesCopiesIndependentBytesWithoutClientUpload(t *testing.T) {
	th := setup(t)
	data := bytes.Repeat([]byte("forwarded attachment bytes\x00"), 100000)
	post, original := upload(t, th, data, "large.bin")
	destination := th.CreatePrivateChannel(t)
	result := forward(t, th, post.Id, destination.Id, []string{original.Id}, http.StatusCreated)
	require.Len(t, result.FileInfos, 1)
	copy, err := th.Store.FileInfo().GetFromMaster(result.FileInfos[0].Id)
	require.NoError(t, err)
	require.NotEqual(t, original.Id, copy.Id)
	require.NotEqual(t, original.Path, copy.Path)
	require.Equal(t, original.Size, copy.Size)
	require.Equal(t, original.Name, copy.Name)
	require.Equal(t, destination.Id, copy.ChannelId)
	require.Equal(t, th.BasicUser.Id, copy.CreatorId)
	require.Empty(t, copy.PostId)
	created, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: destination.Id, Message: "إعادة توجيه", FileIds: model.StringArray{copy.Id}})
	require.NoError(t, err)
	require.Equal(t, model.StringArray{copy.Id}, created.FileIds)
	download, _, err := th.Client.GetFile(context.Background(), copy.Id)
	require.NoError(t, err)
	require.Equal(t, data, download)
	// Deleting the original storage object must not break the independent copy.
	require.Nil(t, th.App.RemoveFile(original.Path))
	download, _, err = th.Client.GetFile(context.Background(), copy.Id)
	require.NoError(t, err)
	require.Equal(t, data, download)
}

func TestForwardFilesChecksLicenseSourceDestinationAndAttachment(t *testing.T) {
	th := setup(t)
	post, file := upload(t, th, []byte("source file"), "proof.txt")
	_, otherFile := upload(t, th, []byte("another file"), "other.txt")
	destination := th.CreatePrivateChannel(t)
	forward(t, th, post.Id, destination.Id, []string{otherFile.Id}, http.StatusBadRequest)
	forward(t, th, post.Id, destination.Id, []string{file.Id, file.Id}, http.StatusBadRequest)
	forward(t, th, post.Id, destination.Id, nil, http.StatusBadRequest)
	_, err := th.SystemAdminClient.RemoveUserFromChannel(context.Background(), destination.Id, th.BasicUser.Id)
	require.NoError(t, err)
	forward(t, th, post.Id, destination.Id, []string{file.Id}, http.StatusForbidden)
	th.AddUserToChannel(t, th.BasicUser, destination)
	th.Server.SetLicense(nil)
	require.Nil(t, th.App.License())
	forward(t, th, post.Id, destination.Id, []string{file.Id}, http.StatusForbidden)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings.EnableFileAttachments = new(false) })
	forward(t, th, post.Id, destination.Id, []string{file.Id}, http.StatusForbidden)
}

func TestForwardFilesRejectsUnreadableSource(t *testing.T) {
	th := setup(t)
	private := th.CreatePrivateChannel(t)
	response, _, err := th.Client.UploadFile(context.Background(), []byte("private"), private.Id, "private.txt")
	require.NoError(t, err)
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: private.Id, FileIds: model.StringArray{response.FileInfos[0].Id}})
	require.NoError(t, err)
	th.LoginBasic2(t)
	forward(t, th, post.Id, th.BasicChannel.Id, post.FileIds, http.StatusForbidden)
}

func TestForwardFilesCopiesAnotherUsersAttachmentToDirectChannel(t *testing.T) {
	th := setup(t)
	data := []byte("shared attachment")
	post, original := upload(t, th, data, "shared.txt")
	th.LoginBasic2(t)
	destination, _, err := th.Client.CreateDirectChannel(context.Background(), th.BasicUser2.Id, th.SystemAdminUser.Id)
	require.NoError(t, err)
	result := forward(t, th, post.Id, destination.Id, post.FileIds, http.StatusCreated)
	require.Len(t, result.FileInfos, 1)
	copy, err := th.Store.FileInfo().GetFromMaster(result.FileInfos[0].Id)
	require.NoError(t, err)
	require.Equal(t, th.BasicUser2.Id, copy.CreatorId)
	require.NotEqual(t, original.CreatorId, copy.CreatorId)
	_, _, err = th.Client.CreatePost(context.Background(), &model.Post{ChannelId: destination.Id, FileIds: model.StringArray{copy.Id}})
	require.NoError(t, err)
	download, _, err := th.Client.GetFile(context.Background(), copy.Id)
	require.NoError(t, err)
	require.Equal(t, data, download)
}

func TestForwardFilesEnforcesCurrentNativeSizeLimit(t *testing.T) {
	th := setup(t)
	post, file := upload(t, th, bytes.Repeat([]byte("x"), 2048), "size.bin")
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings.MaxFileSize = new(int64(1024)) })
	forward(t, th, post.Id, th.BasicChannel.Id, []string{file.Id}, http.StatusRequestEntityTooLarge)
}

func TestForwardFilesRejectsProtectedAndDeletedPost(t *testing.T) {
	for _, kind := range []string{"burn-on-read", "deleted"} {
		t.Run(kind, func(t *testing.T) {
			th := setup(t)
			post, file := upload(t, th, []byte("private"), "private.txt")
			if kind == "deleted" {
				_, err := th.Client.DeletePost(context.Background(), post.Id)
				require.NoError(t, err)
			} else {
				_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET type=$1 WHERE id=$2`, model.PostTypeBurnOnRead, post.Id)
				require.NoError(t, err)
			}
			body, _ := json.Marshal(map[string]any{"channel_id": th.BasicChannel.Id, "file_ids": []string{file.Id}})
			r, err := th.Client.DoAPIPost(context.Background(), "/workspace/posts/"+post.Id+"/files/forward", string(body))
			require.Error(t, err)
			require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, r.StatusCode)
			require.NoError(t, r.Body.Close())
		})
	}
}

func TestForwardFilesCleansEarlierCopiesAfterStorageFailure(t *testing.T) {
	th := setup(t)
	post, first := upload(t, th, []byte("first"), "first.txt")
	response, _, err := th.Client.UploadFile(context.Background(), []byte("missing"), th.BasicChannel.Id, "missing.txt")
	require.NoError(t, err)
	missing, err := th.Store.FileInfo().GetFromMaster(response.FileInfos[0].Id)
	require.NoError(t, err)
	post.FileIds = append(post.FileIds, missing.Id)
	post, _, err = th.Client.UpdatePost(context.Background(), post.Id, post)
	require.NoError(t, err)
	require.Nil(t, th.App.RemoveFile(missing.Path))
	destination := th.CreatePrivateChannel(t)
	body, _ := json.Marshal(map[string]any{"channel_id": destination.Id, "file_ids": post.FileIds})
	r, err := th.Client.DoAPIPost(context.Background(), "/workspace/posts/"+post.Id+"/files/forward", string(body))
	require.Error(t, err)
	require.NoError(t, r.Body.Close())
	var count int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT COUNT(*) FROM fileinfo WHERE channelid=$1 AND postid=''`, destination.Id).Scan(&count))
	require.Zero(t, count)
	bytes, _, err := th.Client.GetFile(context.Background(), first.Id)
	require.NoError(t, err)
	require.Equal(t, "first", string(bytes))
}

func TestForwardFilesRejectsMalformedBody(t *testing.T) {
	th := setup(t)
	for _, body := range []string{"null", "{} {}", strings.Repeat("x", 5000)} {
		r, err := th.Client.DoAPIPost(context.Background(), "/workspace/posts/"+th.BasicPost.Id+"/files/forward", body)
		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, r.StatusCode)
		require.NoError(t, r.Body.Close())
	}
}

func TestForwardFilesRejectsExpiredSourceBeforeDeletionWorker(t *testing.T) {
	th := setup(t)
	post, file := upload(t, th, []byte("expired file"), "expired.txt")
	db := th.Store.GetInternalMasterDB()
	// An already expired row that the background worker cannot claim yet makes
	// this deterministic, without pausing any production worker or goroutine.
	_, err := db.Exec(`INSERT INTO workspace_message_expiry.messages(post_id,channel_id,sent_at,expires_at,next_attempt_at)
 VALUES($1,$2,0,1,9223372036854775807)`, post.Id, post.ChannelId)
	require.NoError(t, err)
	var deletedAt int64
	require.NoError(t, db.QueryRow(`SELECT deleteat FROM posts WHERE id=$1`, post.Id).Scan(&deletedAt))
	require.Zero(t, deletedAt, "the source must still exist before deferred deletion")
	forward(t, th, post.Id, th.BasicChannel.Id, []string{file.Id}, http.StatusNotFound)
	var copies int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM fileinfo WHERE channelid=$1 AND postid=''`, th.BasicChannel.Id).Scan(&copies))
	require.Zero(t, copies, "expired bytes must not survive as new unattached uploads")
}

func TestForwardFilesAllowsSourceBeforeDeadline(t *testing.T) {
	th := setup(t)
	post, file := upload(t, th, []byte("available until deadline"), "available.txt")
	_, err := th.Store.GetInternalMasterDB().Exec(`INSERT INTO workspace_message_expiry.messages(post_id,channel_id,sent_at,expires_at,next_attempt_at)
 VALUES($1,$2,0,9223372036854775807,9223372036854775807)`, post.Id, post.ChannelId)
	require.NoError(t, err)
	destination := th.CreatePrivateChannel(t)
	result := forward(t, th, post.Id, destination.Id, []string{file.Id}, http.StatusCreated)
	require.Len(t, result.FileInfos, 1)
}

func TestForwardFilesCleansCopiesWhenSourceExpiresDuringUpload(t *testing.T) {
	th := setup(t)
	post, file := upload(t, th, []byte("expires while copying"), "expires.txt")
	destination := th.CreatePrivateChannel(t)
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec(`INSERT INTO workspace_message_expiry.messages(post_id,channel_id,sent_at,expires_at,next_attempt_at)
 VALUES($1,$2,0,9223372036854775807,9223372036854775807)`, post.Id, post.ChannelId)
	require.NoError(t, err)
	// Expire at the exact upload insert, without sleeps or racing the worker.
	_, err = db.Exec(`CREATE FUNCTION workspace_message_expiry.expire_forwarding_test_source() RETURNS trigger LANGUAGE plpgsql AS $fn$
 BEGIN
 IF NEW.channelid=TG_ARGV[0] THEN
  UPDATE workspace_message_expiry.messages SET expires_at=1 WHERE post_id=TG_ARGV[1];
 END IF;
 RETURN NEW;
 END $fn$`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec(`DROP TRIGGER IF EXISTS expire_forwarding_test ON fileinfo; DROP FUNCTION workspace_message_expiry.expire_forwarding_test_source()`)
		require.NoError(t, err)
	})
	// Both arguments are server-generated 26-character IDs, never user input.
	require.True(t, model.IsValidId(destination.Id) && model.IsValidId(post.Id))
	_, err = db.Exec(fmt.Sprintf(`CREATE TRIGGER expire_forwarding_test AFTER INSERT ON fileinfo FOR EACH ROW EXECUTE FUNCTION workspace_message_expiry.expire_forwarding_test_source('%s','%s')`, destination.Id, post.Id))
	require.NoError(t, err)
	forward(t, th, post.Id, destination.Id, []string{file.Id}, http.StatusNotFound)
	var copies int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM fileinfo WHERE channelid=$1 AND postid=''`, destination.Id).Scan(&copies))
	require.Zero(t, copies, "a deadline crossed during upload must not expose copies")
}
