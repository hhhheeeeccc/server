package data_retention_job

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/store/sqlstore"
	"github.com/stretchr/testify/require"
)

// Compare persisted results and batch cursors with the original store on two
// independently seeded schemas. This protects the policy algorithm duplicated
// here because core does not expose a side-effect-free scoped store factory.
func TestRetentionExecutionMatchesCorePolicySQL(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
	admin := th.Store.GetInternalMasterDB()
	settings := th.App.Config().Clone().SqlSettings
	dsn := *settings.DataSource
	bootstrapDSN, err := pq.ParseURL(dsn)
	require.NoError(t, err)
	settings.DataSource = new(bootstrapDSN + " default_transaction_read_only=on")
	settings.DataSourceReplicas = nil
	settings.DataSourceSearchReplicas = nil
	settings.ReplicaLagSettings = nil
	settings.MaxOpenConns = new(1)
	settings.MaxIdleConns = new(1)
	native, err := sqlstore.New(settings, th.Context.Logger(), nil, sqlstore.SkipMigrations())
	require.NoError(t, err)
	defer native.Close()
	bootstrapDB := native.GetInternalMasterDB()
	defer bootstrapDB.Close()

	for _, tc := range []struct {
		name           string
		preservePinned bool
		granular       bool
		global         bool
	}{
		{"all_policies", false, true, true},
		{"preserve_pinned", true, true, true},
		{"granular_only", true, true, false},
		{"global_only", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := model.GetMillis()
			cfg := model.RetentionPolicyBatchConfigs{Limit: 2, PreservePinnedPosts: tc.preservePinned}
			if tc.granular {
				cfg.Now = now
			}
			if tc.global {
				cfg.GlobalPolicyEndTime = now - int64(time.Hour/time.Millisecond)
			}
			var executions []*retentionExecution
			for range 2 {
				schema := "retention_" + model.NewId()
				quoted := pq.QuoteIdentifier(schema)
				_, err := admin.Exec("CREATE SCHEMA " + quoted)
				require.NoError(t, err)
				t.Cleanup(func() { _, err := admin.Exec("DROP SCHEMA " + quoted + " CASCADE"); require.NoError(t, err) })
				execution, err := newRetentionExecution(th.Context.Context(), dsn, 10*time.Second)
				require.NoError(t, err)
				t.Cleanup(execution.Close)
				_, err = execution.exec("SET search_path TO " + quoted)
				require.NoError(t, err)
				seedRetentionSQLComparison(t, execution, now)
				executions = append(executions, execution)
			}
			native.SetMasterX(executions[0].db)
			deletes := []struct {
				name   string
				native policyBatchFunc
				local  policyBatchFunc
			}{
				{"posts", native.Post().PermanentDeleteBatchForRetentionPolicies, executions[1].postBatch},
				{"threads", native.Thread().PermanentDeleteBatchForRetentionPolicies, executions[1].threadBatch},
				{"memberships", native.Thread().PermanentDeleteBatchThreadMembershipsForRetentionPolicies, executions[1].membershipBatch},
				{"history", native.ChannelMemberHistory().PermanentDeleteBatchForRetentionPolicies, executions[1].historyBatch},
			}
			for _, deletion := range deletes {
				cursor := model.RetentionPolicyCursor{}
				for step := range 30 {
					wantCount, wantCursor, err := deletion.native(cfg, cursor)
					require.NoError(t, err)
					gotCount, gotCursor, err := deletion.local(cfg, cursor)
					require.NoError(t, err)
					require.Equal(t, wantCount, gotCount, "%s batch %d count", deletion.name, step)
					require.Equal(t, wantCursor, gotCursor, "%s batch %d cursor", deletion.name, step)
					if retentionCursorDone(gotCursor) {
						break
					}
					require.NotEqual(t, 29, step, "policy batching must terminate")
					cursor = gotCursor
				}
			}

			journal, err := native.RetentionPolicy().GetIdsForDeletionByTableName("Posts", 100)
			require.NoError(t, err)
			for _, row := range journal {
				_, err := native.Reaction().DeleteOrphanedRowsByIds(row)
				require.NoError(t, err)
			}
			journal, err = executions[1].reactionIDs(100)
			require.NoError(t, err)
			for _, row := range journal {
				require.NoError(t, executions[1].deleteReactions(row))
			}
			_, err = native.Preference().DeleteOrphanedRows(100)
			require.NoError(t, err)
			require.NoError(t, executions[1].deleteOrphanedPreferences(100))
			_, err = native.RetentionPolicy().DeleteOrphanedRows(100)
			require.NoError(t, err)
			require.NoError(t, executions[1].deleteOrphanedPolicies(100))
			for _, table := range []string{"Posts", "Threads", "ThreadMemberships", "ChannelMemberHistory", "Reactions", "Preferences", "RetentionPoliciesChannels", "RetentionPoliciesTeams", "RetentionIdsForDeletion"} {
				want := retentionComparisonRows(t, executions[0], table)
				got := retentionComparisonRows(t, executions[1], table)
				require.JSONEq(t, string(want), string(got), table)
			}
			var recent, forever, pinned int
			require.NoError(t, executions[1].db.QueryRow(`SELECT count(*) FROM Posts WHERE Id='global_recent'`).Scan(&recent))
			require.Equal(t, 1, recent)
			require.NoError(t, executions[1].db.QueryRow(`SELECT count(*) FROM Posts WHERE Id='forever_old'`).Scan(&forever))
			require.Equal(t, 1, forever, "an indefinite channel policy overrides the global policy")
			require.NoError(t, executions[1].db.QueryRow(`SELECT count(*) FROM Posts WHERE Id='pinned_old'`).Scan(&pinned))
			if tc.global {
				require.Equal(t, map[bool]int{true: 1, false: 0}[tc.preservePinned], pinned)
			}
		})
	}
}

func seedRetentionSQLComparison(t *testing.T, s *retentionExecution, now int64) {
	t.Helper()
	for _, ddl := range []string{
		`CREATE TABLE Teams (Id text PRIMARY KEY)`,
		`CREATE TABLE Channels (Id text PRIMARY KEY, TeamId text)`,
		`CREATE TABLE RetentionPolicies (Id text PRIMARY KEY, PostDuration bigint)`,
		`CREATE TABLE RetentionPoliciesChannels (PolicyId text, ChannelId text)`,
		`CREATE TABLE RetentionPoliciesTeams (PolicyId text, TeamId text)`,
		`CREATE TABLE Posts (Id text PRIMARY KEY, ChannelId text, CreateAt bigint, IsPinned boolean, DeleteAt bigint)`,
		`CREATE TABLE Threads (PostId text PRIMARY KEY, ChannelId text, LastReplyAt bigint)`,
		`CREATE TABLE ThreadMemberships (PostId text, UserId text, LastUpdated bigint)`,
		`CREATE TABLE ChannelMemberHistory (ChannelId text, UserId text, JoinTime bigint, LeaveTime bigint)`,
		`CREATE TABLE RetentionIdsForDeletion (Id text, TableName text, Ids text[])`,
		`CREATE TABLE Preferences (UserId text, Category text, Name text, Value text)`,
		`CREATE TABLE Reactions (UserId text, PostId text, EmojiName text)`,
		`INSERT INTO Teams VALUES ('team'),('unscoped')`,
		`INSERT INTO Channels VALUES ('global','unscoped'),('team','team'),('override','team'),('forever','team')`,
		`INSERT INTO RetentionPolicies VALUES ('team_policy',1),('protect',10),('forever',-1)`,
		`INSERT INTO RetentionPoliciesTeams VALUES ('team_policy','team'),('team_policy','missing')`,
		`INSERT INTO RetentionPoliciesChannels VALUES ('protect','override'),('forever','forever'),('protect','missing')`,
		`INSERT INTO Preferences VALUES ('u','flagged_post','global_old','true'),('u','flagged_post','global_recent','true'),('u','advanced_settings','feature_enabled_keep','false')`,
		`INSERT INTO Reactions VALUES ('u','global_old','smile'),('u','global_recent','smile')`,
	} {
		_, err := s.exec(ddl)
		require.NoError(t, err)
	}
	for _, row := range []struct {
		id, channel string
		age         time.Duration
		pinned      bool
		deleted     int64
	}{
		{"global_old", "global", 5 * 24 * time.Hour, false, 0},
		{"global_recent", "global", 10 * time.Minute, false, 0},
		{"team_old", "team", 5 * 24 * time.Hour, false, 0},
		{"override_old", "override", 5 * 24 * time.Hour, false, 0},
		{"forever_old", "forever", 5 * 24 * time.Hour, false, 0},
		{"pinned_old", "global", 5 * 24 * time.Hour, true, 0},
		{"deleted_pinned_old", "global", 5 * 24 * time.Hour, true, now - 1000},
	} {
		at := now - row.age.Milliseconds()
		_, err := s.exec(`INSERT INTO Posts VALUES ($1,$2,$3,$4,$5)`, row.id, row.channel, at, row.pinned, row.deleted)
		require.NoError(t, err)
		_, err = s.exec(`INSERT INTO Threads VALUES ($1,$2,$3)`, row.id, row.channel, at)
		require.NoError(t, err)
		for _, user := range []string{"u", "v"} {
			_, err = s.exec(`INSERT INTO ThreadMemberships VALUES ($1,$2,$3)`, row.id, user, at)
			require.NoError(t, err)
		}
		_, err = s.exec(`INSERT INTO ChannelMemberHistory VALUES ($1,$2,$3,$4)`, row.channel, row.id, at-1, at)
		require.NoError(t, err)
	}
}

func retentionComparisonRows(t *testing.T, s *retentionExecution, table string) json.RawMessage {
	t.Helper()
	var rows json.RawMessage
	query := fmt.Sprintf(`SELECT coalesce(jsonb_agg(v ORDER BY v::text),'[]'::jsonb) FROM (SELECT to_jsonb(t) AS v FROM %s t) snapshot`, pq.QuoteIdentifier(strings.ToLower(table)))
	require.NoError(t, s.db.QueryRow(query).Scan(&rows))
	return rows
}
