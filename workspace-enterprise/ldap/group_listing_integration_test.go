package ldap

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	ldapclient "github.com/mattermost/ldap"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestLDAPGroupListingMatchesNativeSyncables(t *testing.T) {
	th, directory := setupDirectoryAPI(t)
	service := th.App.Ldap().(*Service)
	archivedTeam := th.CreateTeam(t)
	archivedChannel := th.CreateChannelWithClient(t, th.SystemAdminClient, model.ChannelTypeOpen)
	orphanChannel := th.CreateChannelWithClient(t, th.SystemAdminClient, model.ChannelTypeOpen)
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec(`UPDATE Teams SET DeleteAt=1 WHERE Id=$1`, archivedTeam.Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE Channels SET DeleteAt=1 WHERE Id=$1`, archivedChannel.Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE Channels SET TeamId=$1 WHERE Id=$2`, model.NewId(), orphanChannel.Id)
	require.NoError(t, err)

	entries := []*ldapclient.Entry{}
	oracle := []*model.Group{}
	for i, scenario := range []string{"team", "channel", "both", "deleted_team_link", "deleted_channel_link", "missing_team", "missing_channel", "missing_parent_team", "archived_team", "archived_channel", "unlinked", "deleted_group"} {
		remote := fmt.Sprintf("fixture-%02d", i)
		name := fmt.Sprintf("Group %02d", i)
		if i < 2 {
			name = "Equal name" // Remote ID breaks case-insensitive display-name ties.
		}
		entries = append(entries, ldapclient.NewEntry("cn="+remote+",dc=example,dc=test", map[string][]string{
			"objectClass": {"groupOfNames"}, "cn": {name}, "entryUUID": {remote},
		}))
		group := &model.Group{DisplayName: name, RemoteId: &remote, Source: model.GroupSourceLdap}
		if scenario != "unlinked" {
			stored, err := th.Store.Group().Create(group)
			require.NoError(t, err)
			group = stored
		}
		add := func(kind model.GroupSyncableType, target string, deleted bool) {
			link := model.NewGroupTeam(group.Id, target, false)
			if kind == model.GroupSyncableTypeChannel {
				link = model.NewGroupChannel(group.Id, target, false)
			}
			_, err := th.Store.Group().CreateGroupSyncable(link)
			require.NoError(t, err)
			if deleted {
				_, err = th.Store.Group().DeleteGroupSyncable(group.Id, target, kind)
				require.NoError(t, err)
			}
		}
		switch scenario {
		case "team", "deleted_team_link":
			add(model.GroupSyncableTypeTeam, th.BasicTeam.Id, scenario == "deleted_team_link")
		case "channel", "deleted_channel_link":
			add(model.GroupSyncableTypeChannel, th.BasicChannel.Id, scenario == "deleted_channel_link")
		case "both":
			add(model.GroupSyncableTypeTeam, th.BasicTeam.Id, false)
			add(model.GroupSyncableTypeChannel, th.BasicChannel.Id, false)
		case "missing_team":
			_, err = db.Exec(`INSERT INTO GroupTeams (GroupId,TeamId,AutoAdd,SchemeAdmin,CreateAt,UpdateAt,DeleteAt) VALUES ($1,$2,false,false,1,1,0)`, group.Id, model.NewId())
			require.NoError(t, err)
		case "missing_channel":
			_, err = db.Exec(`INSERT INTO GroupChannels (GroupId,ChannelId,AutoAdd,SchemeAdmin,CreateAt,UpdateAt,DeleteAt) VALUES ($1,$2,false,false,1,1,0)`, group.Id, model.NewId())
			require.NoError(t, err)
		case "missing_parent_team":
			add(model.GroupSyncableTypeChannel, orphanChannel.Id, false)
		case "archived_team":
			add(model.GroupSyncableTypeTeam, archivedTeam.Id, false)
		case "archived_channel":
			add(model.GroupSyncableTypeChannel, archivedChannel.Id, false)
		case "deleted_group":
			_, err = db.Exec(`UPDATE UserGroups SET DeleteAt=1 WHERE Id=$1`, group.Id)
			require.NoError(t, err)
			group.Id = ""
		}
		if group.Id != "" {
			for _, kind := range []model.GroupSyncableType{model.GroupSyncableTypeTeam, model.GroupSyncableTypeChannel} {
				links, err := th.Store.Group().GetAllGroupSyncablesByGroupId(group.Id, kind)
				require.NoError(t, err)
				group.HasSyncables = group.HasSyncables || len(links) > 0
			}
		}
		oracle = append(oracle, group)
	}
	directory.entriesMu.Lock()
	directory.entries = entries
	directory.entriesMu.Unlock()
	sort.Slice(oracle, func(i, j int) bool {
		a, b := strings.ToLower(oracle[i].DisplayName), strings.ToLower(oracle[j].DisplayName)
		if a == b {
			return oracle[i].GetRemoteId() < oracle[j].GetRemoteId()
		}
		return a < b
	})
	for _, opts := range []model.LdapGroupSearchOpts{
		{}, {IsLinked: new(true)}, {IsLinked: new(false)}, {IsConfigured: new(true)},
		{IsConfigured: new(false)}, {IsLinked: new(false), IsConfigured: new(true)}, {Q: "EQUAL NAME"},
	} {
		expected := []*model.Group{}
		for _, group := range oracle {
			if !strings.Contains(strings.ToLower(group.DisplayName), strings.ToLower(opts.Q)) ||
				(opts.IsLinked != nil && *opts.IsLinked != (group.Id != "")) ||
				(opts.IsConfigured != nil && *opts.IsConfigured != group.HasSyncables) {
				continue
			}
			expected = append(expected, group)
		}
		for _, page := range []int{0, 1, 2, 6, int(^uint(0) >> 1)} {
			got, count, appErr := service.GetAllGroupsPage(th.Context, page, 2, opts)
			require.Nil(t, appErr)
			require.Equal(t, len(expected), count)
			want := []*model.Group{}
			if page <= len(expected)/2 {
				want = expected[page*2 : min(page*2+2, len(expected))]
			}
			require.Len(t, got, len(want))
			for i := range want {
				require.Equal(t, want[i].GetRemoteId(), got[i].GetRemoteId())
				require.Equal(t, want[i].Id, got[i].Id)
				require.Equal(t, want[i].HasSyncables, got[i].HasSyncables, want[i].GetRemoteId())
			}
		}
	}
}

func TestLDAPGroupListingQueryScaling(t *testing.T) {
	if os.Getenv("WORKSPACE_REVIEW_QUERY_STATS") != "1" {
		t.Skip("isolated query-count fixture requires pg_stat_statements preload")
	}
	for _, size := range []int{20, 100, 501} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			th, directory := setupDirectoryAPI(t)
			th.App.UpdateConfig(func(cfg *model.Config) { cfg.LdapSettings.MaxPageSize = new(1000) })
			entries := make([]*ldapclient.Entry, 0, size)
			for i := range size {
				remote := fmt.Sprintf("scale-%04d", i)
				name := fmt.Sprintf("Scale %04d", i)
				group, err := th.Store.Group().Create(&model.Group{DisplayName: name, Source: model.GroupSourceLdap, RemoteId: &remote})
				require.NoError(t, err)
				if i%2 == 0 {
					_, err = th.Store.Group().CreateGroupSyncable(model.NewGroupTeam(group.Id, th.BasicTeam.Id, false))
					require.NoError(t, err)
				}
				entries = append(entries, ldapclient.NewEntry("cn="+remote+",dc=example,dc=test", map[string][]string{
					"objectClass": {"groupOfNames"}, "cn": {name}, "entryUUID": {remote},
				}))
			}
			directory.entriesMu.Lock()
			directory.entries = entries
			directory.entriesMu.Unlock()
			db := th.Store.GetInternalMasterDB()
			_, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS pg_stat_statements`)
			require.NoError(t, err)
			calls := func() int64 {
				var result int64
				err := db.QueryRow(`SELECT COALESCE(sum(calls),0) FROM pg_stat_statements
					WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
					AND (lower(query) LIKE '%groupteams%' OR lower(query) LIKE '%groupchannels%')
					AND lower(query) NOT LIKE '%pg_stat_statements%'`).Scan(&result)
				require.NoError(t, err)
				return result
			}
			service := th.App.Ldap().(*Service)
			list := func() {
				groups, count, appErr := service.GetAllGroupsPage(th.Context, 0, 10, model.LdapGroupSearchOpts{})
				require.Nil(t, appErr)
				require.Equal(t, size, count)
				require.Len(t, groups, 10)
				for i, group := range groups {
					require.Equal(t, fmt.Sprintf("scale-%04d", i), group.GetRemoteId())
					require.Equal(t, i%2 == 0, group.HasSyncables)
				}
			}
			list() // Warm native and database caches outside the measured interval.
			beforeCalls := calls()
			times := []float64{}
			var allocated uint64
			for range 3 {
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				start := time.Now()
				list()
				times = append(times, float64(time.Since(start).Microseconds())/1000)
				runtime.ReadMemStats(&after)
				allocated += after.TotalAlloc - before.TotalAlloc
			}
			queries := calls() - beforeCalls
			slices.Sort(times)
			t.Logf("LDAP_GROUP_SCALING groups=%d page=10 queries_per_call=%d median_ms=%.3f allocated_bytes_mean=%d", size, queries/3, times[1], allocated/3)
			require.LessOrEqual(t, queries, int64(3*((size+499)/500)), "page listing must not load every group's complete team/channel objects separately")
		})
	}
}
