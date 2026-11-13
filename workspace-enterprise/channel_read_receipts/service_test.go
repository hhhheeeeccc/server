package channel_read_receipts

import (
	"errors"
	"sort"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestWalkChannelMembersReadsEveryPage(t *testing.T) {
	members := make(model.ChannelMembers, 2*maxMembers+1)
	for index := range members {
		members[index] = model.ChannelMember{UserId: model.NewId(), LastViewedAt: int64(index + 1)}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].UserId < members[j].UserId })
	cursors := make([]string, 0, 3)
	receipts := make(map[string]int64)
	pageErr := walkChannelMembers(func(after string) (model.ChannelMembers, error) {
		cursors = append(cursors, after)
		start := sort.Search(len(members), func(index int) bool { return members[index].UserId > after })
		end := min(start+maxMembers, len(members))
		return members[start:end], nil
	}, func(member model.ChannelMember) {
		receipts[member.UserId] = member.LastViewedAt
	})
	require.NoError(t, pageErr)
	require.Equal(t, []string{"", members[maxMembers-1].UserId, members[2*maxMembers-1].UserId}, cursors)
	require.Len(t, receipts, len(members))
	require.Equal(t, members[len(members)-1].LastViewedAt, receipts[members[len(members)-1].UserId])
}

func TestWalkChannelMembersStopsOnLaterPageFailure(t *testing.T) {
	expected := errors.New("page unavailable")
	pages := 0
	visited := 0
	pageErr := walkChannelMembers(func(after string) (model.ChannelMembers, error) {
		pages++
		if after != "" {
			return nil, expected
		}
		members := make(model.ChannelMembers, maxMembers)
		for index := range members {
			members[index] = model.ChannelMember{UserId: model.NewId()}
		}
		return members, nil
	}, func(model.ChannelMember) { visited++ })
	require.ErrorIs(t, pageErr, expected)
	require.Equal(t, 2, pages)
	require.Equal(t, maxMembers, visited)
}
