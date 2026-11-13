package elasticsearch_indexing

import (
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/store"
)

func (s *indexingStore) Channel() store.ChannelStore {
	if s.reader == nil {
		return s.Store.Channel()
	}
	return &indexingChannelStore{ChannelStore: s.Store.Channel(), reader: s.reader}
}

type indexingChannelStore struct {
	store.ChannelStore
	reader *indexingReader
}

func (s *indexingChannelStore) AnalyticsTypeCount(team string, kind model.ChannelType) (int64, error) {
	query := `SELECT COUNT(*) FROM Channels`
	var where []string
	var args []any
	if team != "" {
		args = append(args, team)
		where = append(where, fmt.Sprintf("TeamId=$%d", len(args)))
	}
	if kind != "" {
		args = append(args, kind)
		where = append(where, fmt.Sprintf("Type=$%d", len(args)))
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	var count int64
	err := s.reader.get(&count, query, args...)
	return count, err
}

func (s *indexingChannelStore) GetChannelsBatchForIndexing(at int64, id string, limit int) ([]*model.Channel, error) {
	rows := []*model.Channel{}
	err := s.reader.selectRows(&rows, `SELECT Id,CreateAt,UpdateAt,DeleteAt,TeamId,Type,DisplayName,Name,Header,Purpose,
		LastPostAt,TotalMsgCount,ExtraUpdateAt,CreatorId,SchemeId,GroupConstrained,AutoTranslation,Shared,
		TotalMsgCountRoot,LastRootPostAt,BannerInfo,DefaultCategoryName,Discoverable,DisableJoinLeaveMessages
		FROM Channels WHERE (CreateAt,Id)>($1,$2) AND Type IN ('O','P','D','G') ORDER BY CreateAt,Id LIMIT $3`, at, id, limit)
	return rows, err
}

func (s *indexingChannelStore) GetAllChannelMemberIdsByChannelId(id string) ([]string, error) {
	rows := []string{}
	err := s.reader.selectRows(&rows, `SELECT UserId FROM ChannelMembers WHERE ChannelId=$1`, id)
	return rows, err
}

func (s *indexingChannelStore) GetTeamMembersForChannel(_ request.CTX, id string) ([]string, error) {
	rows := []string{}
	err := s.reader.selectRows(&rows, `SELECT tm.UserId FROM Channels c,Teams t,TeamMembers tm
		WHERE c.TeamId=t.Id AND t.Id=tm.TeamId AND c.Id=$1`, id)
	return rows, err
}

func (s *indexingStore) FileInfo() store.FileInfoStore {
	if s.reader == nil {
		return s.Store.FileInfo()
	}
	return &indexingFileStore{FileInfoStore: s.Store.FileInfo(), reader: s.reader}
}

type indexingFileStore struct {
	store.FileInfoStore
	reader *indexingReader
}

func (s *indexingFileStore) CountAll() (int64, error) {
	var count int64
	err := s.reader.get(&count, `SELECT num FROM file_stats`)
	return count, err
}

func (s *indexingFileStore) GetFilesBatchForIndexing(at int64, id string, deleted bool, limit int) ([]*model.FileForIndexing, error) {
	rows := []*model.FileForIndexing{}
	err := s.reader.selectRows(&rows, `SELECT Id,CreatorId,PostId,COALESCE(ChannelId,'') AS ChannelId,CreateAt,UpdateAt,
		DeleteAt,Path,ThumbnailPath,PreviewPath,Name,Extension,Size,MimeType,Width,Height,HasPreviewImage,MiniPreview,
		COALESCE(Content,'') AS Content,COALESCE(RemoteId,'') AS RemoteId,Archived
		FROM FileInfo WHERE (CreateAt,Id)>($1,$2) AND ($3 OR DeleteAt=0) ORDER BY CreateAt,Id LIMIT $4`, at, id, deleted, limit)
	return rows, err
}

func (s *indexingStore) User() store.UserStore {
	if s.reader == nil {
		return s.Store.User()
	}
	return &indexingUserStore{UserStore: s.Store.User(), reader: s.reader}
}

type indexingUserStore struct {
	store.UserStore
	reader *indexingReader
}

func (s *indexingUserStore) Count(o model.UserCountOptions) (int64, error) {
	// This wrapper is private to the indexing job, whose total includes local,
	// non-deleted users and bots. Never fall back to an uninterruptible read.
	if !o.IncludeBotAccounts || o.IncludeDeleted || o.IncludeRemoteUsers || o.ExcludeRegularUsers || o.TeamId != "" || o.ChannelId != "" ||
		o.ViewRestrictions != nil || len(o.Roles)+len(o.ChannelRoles)+len(o.TeamRoles) != 0 {
		return 0, errors.New("unsupported user count options in indexing reader")
	}
	var count int64
	err := s.reader.get(&count, `SELECT COUNT(*) FROM Users WHERE DeleteAt=0 AND (RemoteId='' OR RemoteId IS NULL)`)
	return count, err
}

func (s *indexingUserStore) GetUsersBatchForIndexing(at int64, id string, limit int) ([]*model.UserForIndexing, error) {
	users := []*model.UserForIndexing{}
	if err := s.reader.selectRows(&users, `SELECT Id,Username,Nickname,FirstName,LastName,Roles,CreateAt,DeleteAt
		FROM Users WHERE (CreateAt,Id)>($1,$2) ORDER BY CreateAt,Id LIMIT $3`, at, id, limit); err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return users, nil
	}
	ids := make([]string, 0, len(users))
	byID := make(map[string]*model.UserForIndexing, len(users))
	for _, user := range users {
		ids = append(ids, user.Id)
		byID[user.Id] = user
		user.TeamsIds, user.ChannelsIds = []string{}, []string{}
	}
	var channelMembers []struct{ UserId, ChannelId string }
	if err := s.reader.selectRows(&channelMembers, `SELECT cm.UserId,cm.ChannelId FROM ChannelMembers cm
		JOIN Channels c ON cm.ChannelId=c.Id WHERE cm.UserId=ANY($1) AND c.Type IN ('O','D','G')`, pq.Array(ids)); err != nil {
		return nil, err
	}
	for _, member := range channelMembers {
		if user := byID[member.UserId]; user != nil {
			user.ChannelsIds = append(user.ChannelsIds, member.ChannelId)
		}
	}
	var teamMembers []struct{ UserId, TeamId string }
	if err := s.reader.selectRows(&teamMembers, `SELECT UserId,TeamId FROM TeamMembers WHERE UserId=ANY($1) AND DeleteAt=0`, pq.Array(ids)); err != nil {
		return nil, err
	}
	for _, member := range teamMembers {
		if user := byID[member.UserId]; user != nil {
			user.TeamsIds = append(user.TeamsIds, member.TeamId)
		}
	}
	// Retain the SQL tuple order, including IDs tied on CreateAt. Sorting a map
	// only by timestamp would let the next cursor skip other users in that tie.
	return users, nil
}
