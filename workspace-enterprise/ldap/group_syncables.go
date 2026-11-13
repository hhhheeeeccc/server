package ldap

import (
	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

// Listing needs existence, not every team/channel object for every linked
// group. Keep the native joins, including the parent team for a channel:
// orphan links do not count, while archived targets still do. Read the same
// replica as the native group store and close each result before the next batch.
func (s *Service) populateGroupSyncables(rctx request.CTX, groups []*model.Group) error {
	byID := make(map[string]*model.Group, len(groups))
	ids := make([]string, 0, len(groups))
	for _, group := range groups {
		if group.Id != "" {
			byID[group.Id] = group
			ids = append(ids, group.Id)
		}
	}
	for len(ids) > 0 {
		end := min(500, len(ids))
		rows, err := s.app.Srv().Store().GetInternalReplicaDB().QueryContext(rctx.Context(), `
			SELECT requested.groupid FROM unnest($1::varchar[]) AS requested(groupid)
			WHERE EXISTS (SELECT 1 FROM GroupTeams gt JOIN Teams t ON t.Id=gt.TeamId
				WHERE gt.GroupId=requested.groupid AND gt.DeleteAt=0)
			OR EXISTS (SELECT 1 FROM GroupChannels gc JOIN Channels c ON c.Id=gc.ChannelId
				JOIN Teams t ON t.Id=c.TeamId WHERE gc.GroupId=requested.groupid AND gc.DeleteAt=0)`, pq.Array(ids[:end]))
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			byID[id].HasSyncables = true
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		ids = ids[end:]
	}
	return nil
}
