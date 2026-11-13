package shared

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestExportMembershipKeepsDistinctUsersWithoutEmail(t *testing.T) {
	for _, email := range []string{"", "reused@example.test"} {
		t.Run("email="+email, func(t *testing.T) {
			history := []*model.ChannelMemberHistoryResult{
				{ChannelId: "channel", UserId: "first", Username: "first", UserEmail: email, JoinTime: 10},
				{ChannelId: "channel", UserId: "second", Username: "second", UserEmail: email, JoinTime: 20, IsBot: true},
			}
			data, err := GetGenericExportData(ExportParams{
				BatchStartTime: 1, BatchEndTime: 100,
				ChannelMetadata:        map[string]*MetadataChannel{"channel": {ChannelId: "channel"}},
				ChannelMemberHistories: map[string][]*model.ChannelMemberHistoryResult{"channel": history},
			})
			require.NoError(t, err)
			require.Len(t, data.Exports, 1)
			export := data.Exports[0]
			require.Len(t, export.JoinEvents, 2)
			require.ElementsMatch(t, []LeaveExport{
				{UserId: "first", Username: "first", UserEmail: email, UserType: User, LeaveTime: 100, ClosedOut: true},
				{UserId: "second", Username: "second", UserEmail: email, UserType: Bot, LeaveTime: 100, ClosedOut: true},
			}, export.LeaveEvents)
			require.Equal(t, 2, data.Results.Joins)
			require.Equal(t, 2, data.Results.Leaves)
		})
	}
}

func TestExportMembershipLeaveDoesNotRemoveAnotherUserWithoutEmail(t *testing.T) {
	history := []*model.ChannelMemberHistoryResult{
		{UserId: "left", Username: "left", JoinTime: 10, LeaveTime: model.NewPointer(int64(30))},
		{UserId: "stayed", Username: "stayed", JoinTime: 20},
	}
	joins, leaves := getJoinsAndLeaves(1, 100, history, nil)
	require.Len(t, joins, 2)
	require.Equal(t, int64(30), joins[0].LeaveTime)
	require.Equal(t, int64(100), joins[1].LeaveTime)
	require.ElementsMatch(t, []LeaveExport{
		{UserId: "left", Username: "left", UserType: User, LeaveTime: 30},
		{UserId: "stayed", Username: "stayed", UserType: User, LeaveTime: 100, ClosedOut: true},
	}, leaves)
}

func TestExportMembershipClosesPostAuthorsWithoutEmail(t *testing.T) {
	joins, leaves := getJoinsAndLeaves(1, 100, nil, map[string]ChannelMember{
		"first":  {UserId: "first", Username: "first"},
		"second": {UserId: "second", Username: "second"},
	})
	require.Len(t, joins, 2)
	require.ElementsMatch(t, []LeaveExport{
		{UserId: "first", Username: "first", UserType: User, LeaveTime: 100, ClosedOut: true},
		{UserId: "second", Username: "second", UserType: User, LeaveTime: 100, ClosedOut: true},
	}, leaves)
}

func TestExportMembershipRejoinKeepsUserActiveAfterEarlierLeave(t *testing.T) {
	history := []*model.ChannelMemberHistoryResult{
		{UserId: "rejoined", Username: "rejoined", JoinTime: 10, LeaveTime: model.NewPointer(int64(20))},
		{UserId: "rejoined", Username: "rejoined", JoinTime: 30},
		{UserId: "stayed", Username: "stayed", JoinTime: 25},
	}
	_, leaves := getJoinsAndLeaves(1, 100, history, nil)
	require.ElementsMatch(t, []LeaveExport{
		{UserId: "rejoined", Username: "rejoined", UserType: User, LeaveTime: 20},
		{UserId: "rejoined", Username: "rejoined", UserType: User, LeaveTime: 100, ClosedOut: true},
		{UserId: "stayed", Username: "stayed", UserType: User, LeaveTime: 100, ClosedOut: true},
	}, leaves)
}
