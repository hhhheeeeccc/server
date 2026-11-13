package elasticsearch_indexing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestIndexingReaderCutsSilentQueryAndRecoversWithoutClosingAppPool(t *testing.T) {
	th, _, _ := setupIndexer(t)
	dsn := New(th.Server).MakeWorker().(*checkpointWorker).databaseDSN
	for _, cancelEarly := range []bool{false, true} {
		name := "deadline-and-fallback"
		if cancelEarly {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			proxy, proxied := newIndexingSQLProxy(t, dsn)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader := newIndexingReader(ctx, []string{proxied, dsn}, 5*time.Second)
			defer func() { proxy.Close(); reader.Close() }()
			var n int
			require.NoError(t, reader.get(&n, `SELECT 1`))
			if !cancelEarly {
				reader.timeout = 100 * time.Millisecond
			}
			proxy.blocked.Store(true)
			completed := make(chan error, 1)
			go func() {
				_, err := (&indexingPostStore{reader: reader}).GetPostsBatchForIndexing(0, "", 20)
				completed <- err
			}()
			require.Eventually(t, func() bool { return proxy.dropped.Load() > 0 }, time.Second, time.Millisecond)
			if cancelEarly {
				cancel()
			}
			select {
			case err := <-completed:
				require.Error(t, err)
			case <-time.After(700 * time.Millisecond):
				t.Fatal("a real batch read ignored its cancellation/deadline on silent SQL traffic")
			}
			require.Nil(t, reader.db)
			require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT 1`).Scan(&n), "application pool must stay usable")
			if !cancelEarly {
				rows, err := (&indexingPostStore{reader: reader}).GetPostsBatchForIndexing(0, "", 100)
				require.NoError(t, err, "the next read must use the configured healthy fallback")
				var ids []string
				for _, row := range rows {
					ids = append(ids, row.Id)
				}
				require.Contains(t, ids, th.BasicPost.Id)
			} else {
				require.ErrorIs(t, reader.get(&n, `SELECT 1`), context.Canceled)
			}
		})
	}
	t.Run("server-deadline", func(t *testing.T) {
		reader := newIndexingReader(context.Background(), []string{dsn}, 100*time.Millisecond)
		require.NoError(t, reader.open())
		connection := reader.db
		completed, finished := make(chan error, 1), make(chan struct{})
		defer func() { reader.Close(); <-finished }()
		// Deliberately bypass the client query context to prove that PostgreSQL
		// enforces the timeout configured at startup, even without cancel traffic.
		go func() {
			defer close(finished)
			var n int
			completed <- connection.QueryRowContext(context.Background(), `SELECT 1 FROM pg_sleep(1)`).Scan(&n)
		}()
		select {
		case err := <-completed:
			var pgErr *pq.Error
			require.True(t, errors.As(err, &pgErr))
			require.Equal(t, pq.ErrorCode("57014"), pgErr.Code)
		case <-time.After(700 * time.Millisecond):
			t.Fatal("the private reader did not enforce its server statement timeout")
		}
	})
}

func TestIndexingReaderPreservesEntitySemanticsAndUserTiePagination(t *testing.T) {
	th, _, _ := setupIndexer(t)
	w := New(th.Server).MakeWorker().(*checkpointWorker)
	at := model.GetMillis() - 2000
	var ids []string
	for i := 0; i < 7; i++ {
		user, err := th.Store.User().Save(th.Context, &model.User{Username: "reader" + model.NewId(), Email: model.NewId() + "@example.test", CreateAt: at,
			Nickname: "اسم قارئ", FirstName: "اسم", LastName: "عائلة", Roles: model.SystemUserRoleId})
		require.NoError(t, err)
		ids = append(ids, user.Id)
	}
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec(`UPDATE Users SET DeleteAt=1 WHERE Id=$1`, ids[0])
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE Users SET RemoteId=$1 WHERE Id=$2`, model.NewId(), ids[1])
	require.NoError(t, err)
	private, err := th.Store.Channel().Save(th.Context, &model.Channel{TeamId: th.BasicTeam.Id, Name: model.NewId(), DisplayName: "قناة خاصة", Type: model.ChannelTypePrivate}, 100)
	require.NoError(t, err)
	_, err = th.Store.Channel().SaveMember(th.Context, &model.ChannelMember{ChannelId: private.Id, UserId: th.BasicUser.Id, SchemeUser: true, NotifyProps: model.GetDefaultChannelNotifyProps()})
	require.NoError(t, err)
	nonMessage, err := th.Store.Channel().Save(th.Context, &model.Channel{TeamId: th.BasicTeam.Id, Name: model.NewId(), DisplayName: "قناة لوحة", Type: model.ChannelTypeOpen}, 100)
	require.NoError(t, err)
	// The regular channel Save API intentionally rejects board backing types.
	// Use the real enum value in this isolated fixture to exercise its exclusion.
	_, err = db.Exec(`UPDATE Channels SET Type=$1 WHERE Id=$2`, model.ChannelTypeOpenBoard, nonMessage.Id)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO TeamMembers(TeamId,UserId,Roles,DeleteAt,SchemeUser,SchemeAdmin,SchemeGuest)
		VALUES($1,$2,'team_user',1,true,false,false)`, th.BasicTeam.Id, ids[2])
	require.NoError(t, err)
	file, err := th.Store.FileInfo().Save(th.Context, &model.FileInfo{CreatorId: th.BasicUser.Id, PostId: th.BasicPost.Id, ChannelId: th.BasicChannel.Id,
		Name: "semantics.txt", Path: "fixture/semantics.txt", Extension: "txt", MimeType: "text/plain", Content: "دلالات ملف", Size: 20})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE FileInfo SET DeleteAt=1 WHERE Id=$1`, file.Id)
	require.NoError(t, err)
	reader := newIndexingReader(context.Background(), w.readSources, 5*time.Second)
	defer reader.Close()
	bounded := &indexingStore{Store: th.Store, reader: reader}
	var readOnly bool
	require.NoError(t, reader.get(&readOnly, `SELECT current_setting('transaction_read_only')='on'`))
	require.True(t, readOnly)
	var milliseconds int
	require.NoError(t, reader.get(&milliseconds, `SELECT (EXTRACT(EPOCH FROM current_setting('client_connection_check_interval')::interval)*1000)::bigint`))
	require.Equal(t, 100, milliseconds)
	err = reader.query(func(ctx context.Context, conn *sqlx.DB) error {
		_, err := conn.ExecContext(ctx, `UPDATE Posts SET Message='must not be written' WHERE Id=''`)
		return err
	})
	var pgErr *pq.Error
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, pq.ErrorCode("25006"), pgErr.Code, "the private reader must be enforced read only by PostgreSQL")
	for _, count := range []struct {
		name string
		old  func() (int64, error)
		new  func() (int64, error)
	}{
		{"posts", func() (int64, error) { return th.Store.Post().AnalyticsPostCount(&model.PostCountOptions{}) }, func() (int64, error) { return bounded.Post().AnalyticsPostCount(&model.PostCountOptions{}) }},
		{"channels", func() (int64, error) { return th.Store.Channel().AnalyticsTypeCount("", "") }, func() (int64, error) { return bounded.Channel().AnalyticsTypeCount("", "") }},
		{"users", func() (int64, error) { return th.Store.User().Count(model.UserCountOptions{IncludeBotAccounts: true}) }, func() (int64, error) { return bounded.User().Count(model.UserCountOptions{IncludeBotAccounts: true}) }},
		{"files", th.Store.FileInfo().CountAll, bounded.FileInfo().CountAll},
	} {
		t.Run(count.name, func(t *testing.T) {
			original, err := count.old()
			require.NoError(t, err)
			actual, err := count.new()
			require.NoError(t, err)
			require.Equal(t, original, actual)
		})
	}
	originalChannels, err := th.Store.Channel().GetChannelsBatchForIndexing(0, "", 1000)
	require.NoError(t, err)
	channels, err := bounded.Channel().GetChannelsBatchForIndexing(0, "", 1000)
	require.NoError(t, err)
	require.Equal(t, originalChannels, channels)
	for _, channel := range channels {
		require.NotEqual(t, nonMessage.Id, channel.Id)
	}
	originalFiles, err := th.Store.FileInfo().GetFilesBatchForIndexing(0, "", true, 1000)
	require.NoError(t, err)
	files, err := bounded.FileInfo().GetFilesBatchForIndexing(0, "", true, 1000)
	require.NoError(t, err)
	require.Equal(t, originalFiles, files)
	for _, channel := range []*model.Channel{private, th.BasicChannel} {
		original, err := th.Store.Channel().GetAllChannelMemberIdsByChannelId(channel.Id)
		require.NoError(t, err)
		members, err := bounded.Channel().GetAllChannelMemberIdsByChannelId(channel.Id)
		require.NoError(t, err)
		require.ElementsMatch(t, original, members)
		teams, err := th.Store.Channel().GetTeamMembersForChannel(th.Context, channel.Id)
		require.NoError(t, err)
		actualTeams, err := bounded.Channel().GetTeamMembersForChannel(th.Context, channel.Id)
		require.NoError(t, err)
		require.ElementsMatch(t, teams, actualTeams)
	}
	originalUsers, err := th.Store.User().GetUsersBatchForIndexing(0, "", 1000)
	require.NoError(t, err)
	users, err := bounded.User().GetUsersBatchForIndexing(0, "", 1000)
	require.NoError(t, err)
	byID := map[string]*model.UserForIndexing{}
	for _, user := range originalUsers {
		byID[user.Id] = user
	}
	require.Len(t, users, len(originalUsers))
	for _, user := range users {
		original := byID[user.Id]
		require.NotNil(t, original)
		require.ElementsMatch(t, original.TeamsIds, user.TeamsIds)
		require.ElementsMatch(t, original.ChannelsIds, user.ChannelsIds)
		if user.Id == th.BasicUser.Id {
			require.NotContains(t, user.ChannelsIds, private.Id)
		}
		if user.Id == ids[2] {
			require.NotContains(t, user.TeamsIds, th.BasicTeam.Id, "deleted team memberships must not be indexed on users")
		}
		original.TeamsIds, original.ChannelsIds = user.TeamsIds, user.ChannelsIds
		require.Equal(t, original, user)
	}
	var seen []string
	lastAt, lastID := at-1, ""
	for page := 0; page < 20; page++ {
		batch, err := bounded.User().GetUsersBatchForIndexing(lastAt, lastID, 2)
		require.NoError(t, err)
		if len(batch) == 0 {
			break
		}
		for _, user := range batch {
			if user.CreateAt == at {
				seen = append(seen, user.Id)
			}
		}
		last := batch[len(batch)-1]
		require.True(t, last.CreateAt > lastAt || (last.CreateAt == lastAt && last.Id > lastID))
		lastAt, lastID = last.CreateAt, last.Id
		if lastAt > at {
			break
		}
	}
	require.ElementsMatch(t, ids, seen, "all users sharing a timestamp must survive page boundaries exactly once")
}
