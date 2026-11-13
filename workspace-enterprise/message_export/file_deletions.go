package message_export

import (
	"context"
	"database/sql"
	"github.com/mattermost/enterprise/message_export/shared"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

// getFileDeletions uses the primary database because a replica can lag behind
// the post cursor. (DeleteAt, Id) makes equal-millisecond deletes resumable.
func getFileDeletions(rctx request.CTX, store shared.MessageExportStore, afterTime int64, afterID string, until int64, limit int) ([]shared.IndependentFileDeletion, error) {
	deletions := make([]shared.IndependentFileDeletion, 0, limit)
	read := func(ctx context.Context, db *sql.DB) error {
		rows, err := db.QueryContext(ctx, `
		SELECT fi.Id, fi.DeleteAt, fi.Path, fi.Name,
			p.Id, p.CreateAt, p.UpdateAt, p.DeleteAt, p.EditAt,
			p.Message, p.Type, p.Props, p.OriginalId, p.RootId,
			COALESCE(t.Id, ''), COALESCE(t.Name, ''), COALESCE(t.DisplayName, ''),
			c.Id, c.Name, c.Type,
			CASE WHEN c.Type = 'D' THEN 'Direct Message'
				WHEN c.Type = 'G' THEN 'Group Message'
				ELSE c.DisplayName END,
			COALESCE(u.Id, ''), COALESCE(u.Email, ''), COALESCE(u.Username, ''),
			(b.UserId IS NOT NULL)
		FROM FileInfo fi
		JOIN Posts p ON p.Id = fi.PostId
		JOIN Channels c ON c.Id = p.ChannelId
		LEFT JOIN Teams t ON t.Id = c.TeamId
		LEFT JOIN Users u ON u.Id = p.UserId
		LEFT JOIN Bots b ON b.UserId = p.UserId
		WHERE fi.DeleteAt > 0 AND fi.DeleteAt <= $1
			AND (fi.DeleteAt > $2 OR (fi.DeleteAt = $2 AND fi.Id > $3))
			AND p.Type NOT LIKE 'system_%'
		ORDER BY fi.DeleteAt, fi.Id
		LIMIT $4`, until, afterTime, afterID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var file model.FileInfo
			var post model.MessageExport
			var channelType string
			err = rows.Scan(
				&file.Id, &file.DeleteAt, &file.Path, &file.Name,
				&post.PostId, &post.PostCreateAt, &post.PostUpdateAt, &post.PostDeleteAt, &post.PostEditAt,
				&post.PostMessage, &post.PostType, &post.PostProps, &post.PostOriginalId, &post.PostRootId,
				&post.TeamId, &post.TeamName, &post.TeamDisplayName,
				&post.ChannelId, &post.ChannelName, &channelType, &post.ChannelDisplayName,
				&post.UserId, &post.UserEmail, &post.Username, &post.IsBot,
			)
			if err != nil {
				return err
			}
			post.ChannelType = model.NewPointer(model.ChannelType(channelType))
			deletions = append(deletions, shared.IndependentFileDeletion{Post: &post, File: &file})
		}
		return rows.Err()
	}
	var err error
	if reader, ok := store.(interface {
		ReadQuery(request.CTX, func(context.Context, *sql.DB) error) error
	}); ok {
		err = reader.ReadQuery(rctx, read)
	} else {
		err = read(rctx.Context(), store.GetInternalMasterDB())
	}
	return deletions, err
}
