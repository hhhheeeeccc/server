package message_export

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/lib/pq"
	"github.com/mattermost/enterprise/message_export/shared"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

// Compare against the native store on the same real database, including fields
// not consumed by today's formatters. Expected SQL is deliberately not mocked.
func TestMessageExportReaderMatchesNativeMetadataAndPagination(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
	reader := newMessageExportReader(context.Background(), th.App.Config().SqlSettings)
	defer reader.Close()
	native := shared.NewMessageExportStore(th.Store)
	bounded := &exportReadStore{MessageExportStore: native, reader: reader}
	db := th.Store.GetInternalMasterDB()
	private, err := th.Store.Channel().Save(th.Context, &model.Channel{TeamId: th.BasicTeam.Id,
		Name: model.NewId(), DisplayName: "قناة خاصة", Type: model.ChannelTypePrivate}, 100)
	require.NoError(t, err)
	direct, appErr := th.App.GetOrCreateDirectChannel(th.Context, th.BasicUser.Id, th.BasicUser2.Id)
	require.Nil(t, appErr)
	group, err := th.Store.Channel().Save(th.Context, &model.Channel{Name: model.NewId(), Type: model.ChannelTypeGroup}, 0)
	require.NoError(t, err)
	historyOnly, err := th.Store.Channel().Save(th.Context, &model.Channel{TeamId: th.BasicTeam.Id,
		Name: model.NewId(), DisplayName: "عضوية دون رسالة", Type: model.ChannelTypePrivate}, 100)
	require.NoError(t, err)
	nonMessage, err := th.Store.Channel().Save(th.Context, &model.Channel{TeamId: th.BasicTeam.Id,
		Name: model.NewId(), DisplayName: "غير محادثة", Type: model.ChannelTypeOpen}, 100)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE Channels SET Type='S' WHERE Id=$1`, nonMessage.Id)
	require.NoError(t, err)
	_, err = th.Store.Bot().Save(&model.Bot{UserId: th.BasicUser2.Id, Username: th.BasicUser2.Username, OwnerId: th.BasicUser.Id})
	require.NoError(t, err)
	base := model.GetMillis() + 10000
	channels := []*model.Channel{th.BasicChannel, private, direct, group}
	var posts []*model.Post
	for i := range 10 {
		user := th.BasicUser.Id
		if i == 3 {
			user = th.BasicUser2.Id
		}
		post, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: channels[i%4].Id, UserId: user,
			Message: fmt.Sprintf("رسالة %d", i), Props: model.StringInterface{"بيانات": "قيمة عربية"}})
		require.NoError(t, err)
		at := base + 10
		if i == 8 {
			at = base + 20
		} else if i == 9 {
			at = base + 21
		}
		_, err = db.Exec(`UPDATE Posts SET UpdateAt=$1,CreateAt=$1,EditAt=$1 WHERE Id=$2`, at, post.Id)
		require.NoError(t, err)
		posts = append(posts, post)
	}
	_, err = db.Exec(`UPDATE Posts SET DeleteAt=$1,OriginalId=$2 WHERE Id=$2`, base+11, posts[1].Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE Posts SET Type='system_join_channel' WHERE Id=$1`, posts[2].Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE Posts SET Type='custom_export',Hashtags='#اختبار',FileIds=$1 WHERE Id=$2`, `["`+model.NewId()+`"]`, posts[4].Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE Posts SET Filenames='["legacy.txt"]',Hashtags='#legacy' WHERE Id=$1`, posts[5].Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE Posts SET ChannelId=$1,UserId=$2 WHERE Id=$3`, model.NewId(), model.NewId(), posts[7].Id)
	require.NoError(t, err)

	t.Run("all-native-count-filters", func(t *testing.T) {
		ids := []string{posts[0].Id, posts[1].Id, posts[4].Id}
		sort.Strings(ids)
		// The 64 combinations exercise team join, human/bot and post type,
		// legacy/current attachments, hashtag, deletion and system filtering.
		for mask := range 64 {
			options := &model.PostCountOptions{UsersPostsOnly: mask&1 != 0, MustHaveFile: mask&2 != 0,
				MustHaveHashtag: mask&4 != 0, ExcludeDeleted: mask&8 != 0, ExcludeSystemPosts: mask&16 != 0,
				SinceUpdateAt: base + 10, SincePostID: ids[1], UntilUpdateAt: base + 20}
			if mask&32 != 0 {
				options.TeamId = th.BasicTeam.Id
			}
			want, err := native.Post().AnalyticsPostCount(options)
			require.NoError(t, err)
			got, err := shared.ReadPostCount(th.Context, bounded, options)
			require.NoError(t, err)
			require.Equal(t, want, got, "filter combination %d", mask)
		}
		want, err := native.Post().AnalyticsPostCount(&model.PostCountOptions{SinceUpdateAt: base})
		require.NoError(t, err)
		got, err := shared.ReadPostCount(th.Context, bounded, &model.PostCountOptions{SinceUpdateAt: base})
		require.NoError(t, err)
		require.EqualValues(t, 10, got)
		require.Equal(t, want, got)
	})
	t.Run("all-fields-and-equal-time-page-cursors", func(t *testing.T) {
		for _, upper := range []int64{0, base + 20} {
			cursor := model.MessageExportCursor{LastPostUpdateAt: base, UntilUpdateAt: upper}
			var all []*model.MessageExport
			for page := 0; ; page++ {
				require.Less(t, page, 10, "equal-timestamp cursor failed to advance")
				want, wantCursor, err := native.Compliance().MessageExport(th.Context, cursor, 2)
				require.NoError(t, err)
				got, next, err := bounded.Compliance().MessageExport(th.Context, cursor, 2)
				require.NoError(t, err)
				require.Equal(t, want, got)
				require.Equal(t, wantCursor, next)
				all = append(all, got...)
				cursor = next
				if len(got) == 0 {
					break
				}
			}
			count := 9
			if upper != 0 {
				count = 8
			}
			require.Len(t, all, count)
			var missing, bot, directName, groupName bool
			for _, row := range all {
				if *row.PostId == posts[7].Id {
					missing = true
					require.Nil(t, row.ChannelId)
					require.Nil(t, row.TeamId)
					require.Nil(t, row.UserId)
				}
				bot = bot || row.IsBot
				if row.ChannelType != nil && *row.ChannelType == model.ChannelTypeDirect {
					directName = true
					require.Equal(t, "Direct Message", *row.ChannelDisplayName)
				}
				if row.ChannelType != nil && *row.ChannelType == model.ChannelTypeGroup {
					groupName = true
					require.Equal(t, "Group Message", *row.ChannelDisplayName)
				}
			}
			require.True(t, missing && bot && directName && groupName)
		}
	})
	t.Run("channel-dto-and-missing-channels", func(t *testing.T) {
		for _, ids := range [][]string{{th.BasicChannel.Id, private.Id, direct.Id, group.Id, nonMessage.Id, model.NewId()}, {model.NewId()}, {nonMessage.Id}, {}} {
			want, wantErr := shared.ReadChannels(th.Context, native, ids)
			got, err := shared.ReadChannels(th.Context, bounded, ids)
			if wantErr != nil {
				require.Error(t, err)
				require.Equal(t, wantErr.Error(), err.Error())
				continue
			}
			require.NoError(t, err)
			require.Len(t, got, 4)
			require.ElementsMatch(t, want, got)
		}
	})
	t.Run("full-file-metadata-and-deleted-records", func(t *testing.T) {
		for i := range 3 {
			info, err := th.Store.FileInfo().Save(th.Context, &model.FileInfo{CreatorId: th.BasicUser.Id,
				PostId: posts[0].Id, ChannelId: th.BasicChannel.Id, Path: fmt.Sprintf("export-test/%d", i),
				ThumbnailPath: "thumbnail", PreviewPath: "preview", Name: "مرفق.png", Extension: "png",
				Size: 1234, MimeType: "image/png", Width: 41, Height: 29, HasPreviewImage: true,
				MiniPreview: model.NewPointer([]byte("mini")), Content: "نص المرفق", RemoteId: model.NewPointer(model.NewId()), Archived: i == 2})
			require.NoError(t, err)
			var deletedAt int64
			if i == 1 {
				deletedAt = base + 50
			}
			_, err = db.Exec(`UPDATE FileInfo SET CreateAt=$1,UpdateAt=$1,DeleteAt=$2 WHERE Id=$3`, base+int64(i), deletedAt, info.Id)
			require.NoError(t, err)
			if i == 0 {
				_, err = db.Exec(`UPDATE FileInfo SET ChannelId=NULL,Content=NULL,RemoteId=NULL WHERE Id=$1`, info.Id)
				require.NoError(t, err)
			}
		}
		for _, deleted := range []bool{false, true} {
			want, err := native.FileInfo().GetForPost(posts[0].Id, true, deleted, false)
			require.NoError(t, err)
			got, err := bounded.FileInfo().GetForPost(posts[0].Id, false, deleted, true)
			require.NoError(t, err)
			require.NotEmpty(t, got)
			require.Equal(t, want, got)
		}
		want, err := getFileDeletions(th.Context, native, base-1, "", base+100, 20)
		require.NoError(t, err)
		require.Len(t, want, 1)
		got, err := getFileDeletions(th.Context, bounded, base-1, "", base+100, 20)
		require.NoError(t, err)
		require.Equal(t, want, got)
	})
	t.Run("history-and-prehistory-fallback", func(t *testing.T) {
		ids := []string{th.BasicChannel.Id, private.Id, direct.Id, group.Id, historyOnly.Id}
		_, err := db.Exec(`DELETE FROM ChannelMemberHistory WHERE ChannelId=ANY($1)`, pq.Array(ids))
		require.NoError(t, err)
		for i, row := range []struct {
			channel string
			user    string
			join    int64
			leave   *int64
		}{
			{th.BasicChannel.Id, th.BasicUser.Id, base - 10, model.NewPointer(base)},
			{th.BasicChannel.Id, th.BasicUser.Id, base + 20, nil},
			{private.Id, th.BasicUser2.Id, base, nil},
			{direct.Id, th.BasicUser.Id, base - 20, model.NewPointer(base - 1)},
			{group.Id, th.BasicUser.Id, base + 21, nil},
			{historyOnly.Id, th.BasicUser.Id, base + 5, nil},
		} {
			_, err := db.Exec(`INSERT INTO ChannelMemberHistory(ChannelId,UserId,JoinTime,LeaveTime) VALUES($1,$2,$3,$4)`, row.channel, row.user, row.join, row.leave)
			require.NoError(t, err, "history row %d", i)
		}
		_, err = db.Exec(`UPDATE Users SET DeleteAt=$1 WHERE Id=$2`, base-1, th.BasicUser2.Id)
		require.NoError(t, err)
		for _, start := range []int64{1, base} {
			want, err := shared.ReadMembershipHistory(th.Context, native, start, base+20, ids)
			require.NoError(t, err)
			got, err := shared.ReadMembershipHistory(th.Context, bounded, start, base+20, ids)
			require.NoError(t, err)
			require.NotEmpty(t, got)
			require.ElementsMatch(t, want, got)
			if start == base {
				require.Len(t, got, 4, "inclusive join/leave boundaries and NULL leave times must be preserved")
				var bot bool
				for _, row := range got {
					bot = bot || row.IsBot && row.UserDeleteAt == base-1
				}
				require.True(t, bot)
			} else {
				for _, row := range got {
					require.Equal(t, start, row.JoinTime)
					require.Equal(t, base+20, *row.LeaveTime)
				}
			}
		}
		want, err := shared.ReadActiveChannels(th.Context, native, base, base+20)
		require.NoError(t, err)
		got, err := shared.ReadActiveChannels(th.Context, bounded, base, base+20)
		require.NoError(t, err)
		require.ElementsMatch(t, want, got)
		require.Contains(t, got, private.Id)
		require.Contains(t, got, th.BasicChannel.Id)
		require.Contains(t, got, historyOnly.Id, "activity must include membership changes without any posts")
	})
}
