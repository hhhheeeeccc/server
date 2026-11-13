package compliance_service

import (
	"context"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

func TestComplianceReaderMatchesNativeFieldsAndFilters(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	job := savedReadReport(t, th)
	private, err := th.Store.Channel().Save(th.Context, &model.Channel{TeamId: th.BasicTeam.Id,
		Name: model.NewId(), DisplayName: "قناة خاصة", Type: model.ChannelTypePrivate}, 100)
	require.NoError(t, err)
	direct, appErr := th.App.GetOrCreateDirectChannel(th.Context, th.BasicUser.Id, th.BasicUser2.Id)
	require.Nil(t, appErr)
	group, err := th.Store.Channel().Save(th.Context, &model.Channel{Name: model.NewId(), Type: model.ChannelTypeGroup}, 0)
	require.NoError(t, err)
	_, err = th.Store.Bot().Save(&model.Bot{UserId: th.BasicUser2.Id, Username: th.BasicUser2.Username, OwnerId: th.BasicUser.Id})
	require.NoError(t, err)
	for i, row := range []struct {
		channel *model.Channel
		user    string
		message string
	}{
		{th.BasicChannel, th.BasicUser.Id, "قيمة عربية"},
		{private, th.BasicUser.Id, "Report 100% ready"},
		{direct, th.BasicUser.Id, "Report 100X ready"},
		{group, th.BasicUser.Id, `under_score and path\part`},
		{direct, th.BasicUser.Id, "underXscore"},
		{direct, th.BasicUser2.Id, "PATHPART"},
		{private, th.BasicUser.Id, "قيمة محذوفة"},
	} {
		post, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: row.channel.Id, UserId: row.user,
			CreateAt: job.StartAt, Message: row.message, Props: model.StringInterface{"معلومة": "عربية"},
			FileIds: model.StringArray{model.NewId()}, Hashtags: "#اختبار"})
		require.NoError(t, err)
		if i == 6 {
			_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE Posts SET DeleteAt=$1, UpdateAt=$1, OriginalId=$2, Type='system_test' WHERE Id=$2`, job.StartAt+100, post.Id)
			require.NoError(t, err)
		}
	}
	for _, at := range []int64{job.StartAt - 1, job.EndAt} {
		_, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: th.BasicChannel.Id,
			UserId: th.BasicUser.Id, CreateAt: at, Message: "خارج النطاق"})
		require.NoError(t, err)
	}
	reader := newComplianceReader(th.App.Config().SqlSettings)
	defer reader.Close()
	for _, spec := range []struct {
		name, keywords, emails string
		count                  int
	}{
		{"all", "", "", 7},
		{"keywords-literal-wildcards", "قيمة,100%,under_", "", 4},
		{"native-backslash-sanitization", `path\part`, "", 1},
		{"email-normalization", "", strings.ToUpper(th.BasicUser2.Email), 1},
		{"combined-or-filters", "قيمة,100%", th.BasicUser.Email + "," + th.BasicUser2.Email, 3},
		{"no-match", "لايتطابق", "", 0},
	} {
		t.Run(spec.name, func(t *testing.T) {
			job.Keywords, job.Emails = spec.keywords, spec.emails
			original, nativeCursor, err := th.Store.Compliance().ComplianceExport(job, model.ComplianceExportCursor{}, 100)
			require.NoError(t, err)
			posts, cursor, err := service.fetchComplianceBatch(context.Background(), reader, job, model.ComplianceExportCursor{}, 100)
			require.NoError(t, err)
			require.Len(t, posts, spec.count)
			require.Equal(t, original, posts, "every exported field must match the native privileged reader")
			require.Equal(t, nativeCursor, cursor)
		})
	}
	// Also compare tie-breaking cursors page by page in each stream separately;
	// the independent mixed-stream archive test covers the native omission fix.
	job.Keywords, job.Emails = "", ""
	for _, directOnly := range []bool{false, true} {
		cursor := model.ComplianceExportCursor{ChannelsQueryCompleted: directOnly, DirectMessagesQueryCompleted: !directOnly}
		for range 10 {
			original, expected, err := th.Store.Compliance().ComplianceExport(job, cursor, 1)
			require.NoError(t, err)
			posts, next, err := reader.batch(context.Background(), job, cursor, 1)
			require.NoError(t, err)
			require.Equal(t, original, posts)
			require.Equal(t, expected, next)
			cursor = next
			if cursor.ChannelsQueryCompleted && cursor.DirectMessagesQueryCompleted {
				break
			}
		}
		require.True(t, cursor.ChannelsQueryCompleted && cursor.DirectMessagesQueryCompleted)
	}
}

func TestComplianceAdhocOutlivesCanceledRequestContext(t *testing.T) {
	th, service := dailyScheduleFixture(t)
	job := savedReadReport(t, th)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // SaveComplianceReport returns before its asynchronous export runs.
	rctx := request.EmptyContext(th.App.Log()).WithContext(ctx)
	require.Nil(t, service.RunComplianceJob(rctx, job))
	require.Len(t, readReportCSV(t, th, job), 1)
	stored, err := th.Store.Compliance().Get(job.Id)
	require.NoError(t, err)
	require.Equal(t, model.ComplianceStatusFinished, stored.Status)
}
