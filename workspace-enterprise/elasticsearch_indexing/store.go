package elasticsearch_indexing

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/store"
)

// This wrapper is private to the Enterprise index job. Its counts, batches and
// membership reads share a cancelable session, without touching application SQL.
// Posts whose channels no longer exist are omitted from searchable batches.
type indexingStore struct {
	store.Store
	db     *sql.DB
	reader *indexingReader
}

func (s *indexingStore) Post() store.PostStore {
	return &indexingPostStore{PostStore: s.Store.Post(), db: s.db, reader: s.reader}
}

type indexingPostStore struct {
	store.PostStore
	db     *sql.DB
	reader *indexingReader
}

func (s *indexingPostStore) GetPostsBatchForIndexing(startTime int64, startPostID string, limit int) ([]*model.PostForIndexing, error) {
	if s.db == nil && s.reader == nil {
		return s.PostStore.GetPostsBatchForIndexing(startTime, startPostID, limit)
	}
	posts := make([]*model.PostForIndexing, 0)
	query := `SELECT Posts.Id, Posts.CreateAt, Posts.UpdateAt, Posts.EditAt,
		Posts.DeleteAt, Posts.IsPinned, Posts.UserId, Posts.ChannelId,
		Posts.RootId, Posts.OriginalId, Posts.Message, Posts.Type,
		Posts.Props, Posts.Hashtags, Posts.Filenames, Posts.FileIds,
		Posts.HasReactions, Posts.RemoteId, Channels.TeamId,
		Channels.Type AS channeltype
		FROM Posts JOIN Channels ON Posts.ChannelId = Channels.Id
		WHERE (Posts.CreateAt, Posts.Id) > ($1, $2)
		ORDER BY Posts.CreateAt ASC, Posts.Id ASC LIMIT $3`
	var err error
	if s.reader != nil {
		err = s.reader.selectRows(&posts, query, startTime, startPostID, limit)
	} else {
		err = sqlx.NewDb(s.db, "postgres").Select(&posts, query, startTime, startPostID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("get searchable posts for indexing: %w", err)
	}
	return posts, nil
}

func (s *indexingPostStore) AnalyticsPostCount(options *model.PostCountOptions) (int64, error) {
	if s.reader == nil {
		return s.PostStore.AnalyticsPostCount(options)
	}
	if options != nil && *options != (model.PostCountOptions{}) {
		return 0, errors.New("unsupported post count options in indexing reader")
	}
	var count int64
	err := s.reader.get(&count, `SELECT COUNT(*) FROM Posts`)
	return count, err
}
