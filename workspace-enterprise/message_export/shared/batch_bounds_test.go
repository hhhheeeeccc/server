package shared

import (
	"context"
	"errors"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

type boundedMetadataFixture struct {
	MessageExportStore
	ContextualMetadataStore
	activeCalls, channelCalls, historyCalls int
	channelBatches, historyBatches          [][]string
	cancel                                  context.CancelFunc
	cancelPhase                             string
}

func (s *boundedMetadataFixture) ExportActiveChannels(request.CTX, int64, int64) ([]string, error) {
	s.activeCalls++
	return []string{"one", "two", "three", "four"}, nil
}

func (s *boundedMetadataFixture) ExportChannels(_ request.CTX, ids []string) ([]ExportChannel, error) {
	s.channelCalls++
	// Stop the old zero-step loop deterministically instead of hanging a test.
	if s.channelCalls > 5 {
		return nil, errors.New("fixture stopped a non-progressing channel loop")
	}
	s.channelBatches = append(s.channelBatches, append([]string(nil), ids...))
	if s.cancelPhase == "channels" && s.channelCalls == 1 {
		s.cancel()
	}
	var channels []ExportChannel
	for _, id := range ids {
		channels = append(channels, ExportChannel{Id: id, Type: model.ChannelTypeOpen})
	}
	return channels, nil
}

func (s *boundedMetadataFixture) ExportMembershipHistory(_ request.CTX, _, _ int64, ids []string) ([]*model.ChannelMemberHistoryResult, error) {
	s.historyCalls++
	if s.historyCalls > 5 {
		return nil, errors.New("fixture stopped a non-progressing history loop")
	}
	s.historyBatches = append(s.historyBatches, append([]string(nil), ids...))
	if s.cancelPhase == "history" && s.historyCalls == 1 {
		s.cancel()
	}
	var history []*model.ChannelMemberHistoryResult
	for _, id := range ids {
		history = append(history, &model.ChannelMemberHistoryResult{ChannelId: id, UserId: "member", JoinTime: 10})
	}
	return history, nil
}

func TestCalculateChannelExportsRejectsNonPositiveBatches(t *testing.T) {
	for _, tc := range []struct {
		name              string
		channels, history int
	}{
		{"zero_channels", 0, 1}, {"negative_channels", -1, 1},
		{"zero_history", 1, 0}, {"negative_history", 1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &boundedMetadataFixture{}
			require.NotPanics(t, func() {
				_, _, err := CalculateChannelExports(request.TestContext(t), ChannelExportsParams{
					Store: fixture, ChannelBatchSize: tc.channels, ChannelHistoryBatchSize: tc.history,
					ReportProgressMessage: func(string) {},
				})
				require.ErrorContains(t, err, "batch size")
			})
			require.Zero(t, fixture.activeCalls, "invalid jobs must not start reading channel data")
			require.Zero(t, fixture.channelCalls)
			require.Zero(t, fixture.historyCalls)
		})
	}
}

func TestCalculateChannelExportsStopsAfterCancellation(t *testing.T) {
	for _, phase := range []string{"channels", "history"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fixture := &boundedMetadataFixture{cancel: cancel, cancelPhase: phase}
			_, _, err := CalculateChannelExports(request.TestContext(t).WithContext(ctx), ChannelExportsParams{
				Store: fixture, ChannelBatchSize: 1, ChannelHistoryBatchSize: 1,
				ReportProgressMessage: func(string) {},
			})
			require.ErrorIs(t, err, context.Canceled)
			if phase == "channels" {
				require.Equal(t, 1, fixture.channelCalls)
				require.Zero(t, fixture.historyCalls)
			} else {
				require.Equal(t, 1, fixture.historyCalls)
			}
		})
	}
}

func TestCalculateChannelExportsPreservesEveryBatchMember(t *testing.T) {
	fixture := &boundedMetadataFixture{}
	metadata, histories, err := CalculateChannelExports(request.TestContext(t), ChannelExportsParams{
		Store: fixture, ChannelBatchSize: 3, ChannelHistoryBatchSize: 2,
		ReportProgressMessage: func(string) {},
	})
	require.NoError(t, err)
	require.Len(t, metadata, 4)
	require.Len(t, histories, 4)
	require.Equal(t, [][]string{{"one", "two", "three"}, {"four"}}, fixture.channelBatches)
	require.Equal(t, [][]string{{"one", "two"}, {"three", "four"}}, fixture.historyBatches)
	for id, members := range histories {
		require.Equal(t, id, metadata[id].ChannelId)
		require.Len(t, members, 1)
		require.Equal(t, "member", members[0].UserId)
	}
}
