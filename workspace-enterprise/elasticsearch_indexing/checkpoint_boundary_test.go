package elasticsearch_indexing

import (
	"io"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/mattermost/mattermost/server/v8/channels/store/storetest/mocks"
	"github.com/mattermost/mattermost/server/v8/enterprise/elasticsearch/common"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Exercise the actual worker and common serialization, without a database or
// search server. More than one batch shares the inclusive upper timestamp.
func TestIndexBatchDrainsInclusiveEndTimestamp(t *testing.T) {
	for _, kind := range []string{"posts", "channels", "users", "files"} {
		t.Run(kind, func(t *testing.T) {
			cfg := &model.Config{}
			cfg.SetDefaults()
			cfg.ElasticsearchSettings.EnableIndexing = new(true)
			cfg.ElasticsearchSettings.BatchSize = new(2)
			st := &mocks.Store{}
			ids := []string{"a", "b", "c", "d", "e"}
			times := []int64{900, 900, 900, 900, 901}
			// Each mock verifies the exact timestamp-and-ID cursor after the first full batch.
			switch kind {
			case "posts":
				s := &mocks.PostStore{}
				st.On("Post").Return(s)
				rows := make([]*model.PostForIndexing, 5)
				for i := range rows {
					rows[i] = &model.PostForIndexing{Post: model.Post{Id: ids[i], CreateAt: times[i], Type: model.PostTypeBurnOnRead}}
				}
				s.On("GetPostsBatchForIndexing", int64(100), "", 2).Return(rows[:2], nil).Once()
				s.On("GetPostsBatchForIndexing", int64(900), "b", 2).Return(rows[2:4], nil).Once()
				s.On("GetPostsBatchForIndexing", int64(900), "d", 2).Return(rows[4:], nil).Once()
			case "channels":
				s := &mocks.ChannelStore{}
				st.On("Channel").Return(s)
				s.On("GetTeamMembersForChannel", mock.Anything, mock.Anything).Return([]string{}, nil)
				rows := make([]*model.Channel, 5)
				for i := range rows {
					rows[i] = &model.Channel{Id: ids[i], CreateAt: times[i]}
				}
				s.On("GetChannelsBatchForIndexing", int64(100), "", 2).Return(rows[:2], nil).Once()
				s.On("GetChannelsBatchForIndexing", int64(900), "b", 2).Return(rows[2:4], nil).Once()
				s.On("GetChannelsBatchForIndexing", int64(900), "d", 2).Return(rows[4:], nil).Once()
			case "users":
				s := &mocks.UserStore{}
				st.On("User").Return(s)
				rows := make([]*model.UserForIndexing, 5)
				for i := range rows {
					rows[i] = &model.UserForIndexing{Id: ids[i], CreateAt: times[i]}
				}
				s.On("GetUsersBatchForIndexing", int64(100), "", 2).Return(rows[:2], nil).Once()
				s.On("GetUsersBatchForIndexing", int64(900), "b", 2).Return(rows[2:4], nil).Once()
				s.On("GetUsersBatchForIndexing", int64(900), "d", 2).Return(rows[4:], nil).Once()
			case "files":
				s := &mocks.FileInfoStore{}
				st.On("FileInfo").Return(s)
				rows := make([]*model.FileForIndexing, 5)
				for i := range rows {
					rows[i] = &model.FileForIndexing{FileInfo: model.FileInfo{Id: ids[i], CreateAt: times[i]}}
				}
				s.On("GetFilesBatchForIndexing", int64(100), "", true, 2).Return(rows[:2], nil).Once()
				s.On("GetFilesBatchForIndexing", int64(900), "b", true, 2).Return(rows[2:4], nil).Once()
				s.On("GetFilesBatchForIndexing", int64(900), "d", true, 2).Return(rows[4:], nil).Once()
			}
			js := &jobs.JobServer{ConfigService: &indexingConfig{config: cfg}, Store: st}
			license := func() *model.License { return &model.License{Features: &model.Features{Elasticsearch: new(true)}} }
			add := func(_, _, id string, _ io.ReadSeeker) error {
				require.NotEqual(t, "e", id, "out-of-range entity indexed")
				return nil
			}
			w := &checkpointWorker{server: js, liveConfig: func() *model.Config { return cfg }, target: indexingTarget(cfg), add: add}
			w.batcher = common.NewIndexerWorker("boundary", "elasticsearch", js, nil, nil, license, nil, add, nil)
			job := &model.Job{Data: model.StringMap{"index_posts": "false", "index_channels": "false", "index_users": "false", "index_files": "false"}}
			job.Data["index_"+kind] = "true"
			p := common.IndexingProgress{StartAtTime: 100, LastEntityTime: 100, EndAtTime: 900, Now: time.Now()}
			for i := 0; i < 3; i++ {
				next, status, err := w.indexBatch(p, job, nil, nil)
				require.NoError(t, err)
				require.Empty(t, status)
				p = next
				if i < 2 {
					require.False(t, p.IsDone(job), "boundary batch completed prematurely")
					require.EqualValues(t, 900, p.LastEntityTime)
				}
			}
			require.True(t, p.IsDone(job))
			require.EqualValues(t, 4, p.DonePostsCount+p.DoneChannelsCount+p.DoneUsersCount+p.DoneFilesCount)
			require.EqualValues(t, 100, p.LastEntityTime)
			st.AssertExpectations(t)
		})
	}
}

func TestIndexingRowsThroughIncludesBoundaryAndExcludesFuture(t *testing.T) {
	for _, tc := range []struct {
		rows []int64
		want []int64
		end  bool
	}{
		{nil, nil, false}, {[]int64{899, 900, 900}, []int64{899, 900, 900}, false},
		{[]int64{900, 901}, []int64{900}, true}, {[]int64{901}, []int64{}, true},
	} {
		rows, end := indexingRowsThrough(tc.rows, 900, func(at int64) int64 { return at })
		require.Equal(t, tc.want, rows)
		require.Equal(t, tc.end, end)
	}
}
