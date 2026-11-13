package channel_last_post

import (
	"context"
	"database/sql"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
)

// Read priorities/acknowledgements in two bounded batches instead of two
// round trips for every channel preview. Both use the same primary database
// as the preview rows, so edits are not followed by stale replica metadata.
func previewPriorityMetadata(ctx context.Context, db *sql.DB, postIDs []string) (map[string]*model.PostMetadata, error) {
	result := make(map[string]*model.PostMetadata, len(postIDs))
	if len(postIDs) == 0 {
		return result, nil
	}
	for _, id := range postIDs {
		result[id] = &model.PostMetadata{}
	}
	rows, err := db.QueryContext(ctx, `SELECT postid,channelid,priority,requestedack,persistentnotifications
		FROM postspriority WHERE postid=ANY($1::varchar[])`, pq.Array(postIDs))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p model.PostPriority
		if err := rows.Scan(&p.PostId, &p.ChannelId, &p.Priority, &p.RequestedAck, &p.PersistentNotifications); err != nil {
			rows.Close()
			return nil, err
		}
		if metadata := result[p.PostId]; metadata != nil {
			metadata.Priority = &p
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = db.QueryContext(ctx, `SELECT postid,userid,channelid,acknowledgedat,remoteid
		FROM postacknowledgements WHERE postid=ANY($1::varchar[]) AND acknowledgedat<>0`, pq.Array(postIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ack model.PostAcknowledgement
		if err := rows.Scan(&ack.PostId, &ack.UserId, &ack.ChannelId, &ack.AcknowledgedAt, &ack.RemoteId); err != nil {
			return nil, err
		}
		if metadata := result[ack.PostId]; metadata != nil {
			metadata.Acknowledgements = append(metadata.Acknowledgements, &ack)
		}
	}
	return result, rows.Err()
}
