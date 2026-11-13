package shared

import (
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

// ExportChannel contains only the channel fields consumed by export metadata.
// It is not a replacement for the full ChannelStore contract.
type ExportChannel struct {
	Id, TeamId, Name, DisplayName string
	Type                          model.ChannelType
}

// Worker stores can provide cancellable metadata without changing the native
// store interfaces or breaking older CLI implementations of MessageExportStore.
type ContextualMetadataStore interface {
	ExportPostCount(request.CTX, *model.PostCountOptions) (int64, error)
	ExportActiveChannels(request.CTX, int64, int64) ([]string, error)
	ExportChannels(request.CTX, []string) ([]ExportChannel, error)
	ExportMembershipHistory(request.CTX, int64, int64, []string) ([]*model.ChannelMemberHistoryResult, error)
}

func ReadPostCount(rctx request.CTX, db MessageExportStore, options *model.PostCountOptions) (int64, error) {
	if reader, ok := db.(ContextualMetadataStore); ok {
		return reader.ExportPostCount(rctx, options)
	}
	return db.Post().AnalyticsPostCount(options)
}

func ReadActiveChannels(rctx request.CTX, db MessageExportStore, start, end int64) ([]string, error) {
	if reader, ok := db.(ContextualMetadataStore); ok {
		return reader.ExportActiveChannels(rctx, start, end)
	}
	return db.ChannelMemberHistory().GetChannelsWithActivityDuring(start, end)
}

func ReadChannels(rctx request.CTX, db MessageExportStore, ids []string) ([]ExportChannel, error) {
	if reader, ok := db.(ContextualMetadataStore); ok {
		return reader.ExportChannels(rctx, ids)
	}
	channels, err := db.Channel().GetMany(ids, true)
	if err != nil {
		return nil, err
	}
	result := make([]ExportChannel, 0, len(channels))
	for _, channel := range channels {
		result = append(result, ExportChannel{Id: channel.Id, TeamId: channel.TeamId,
			Name: channel.Name, DisplayName: channel.DisplayName, Type: channel.Type})
	}
	return result, nil
}

func ReadMembershipHistory(rctx request.CTX, db MessageExportStore, start, end int64, ids []string) ([]*model.ChannelMemberHistoryResult, error) {
	if reader, ok := db.(ContextualMetadataStore); ok {
		return reader.ExportMembershipHistory(rctx, start, end, ids)
	}
	return db.ChannelMemberHistory().GetUsersInChannelDuring(start, end, ids)
}
