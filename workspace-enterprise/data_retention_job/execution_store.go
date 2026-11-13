// Policy selection adapted from Mattermost's SQL retention implementation.
// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See the sibling Mattermost LICENSE.txt for license information.

package data_retention_job

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	sq "github.com/mattermost/squirrel"
)

var errRetentionSessionLost = errors.New("retention execution cannot reconnect after losing its database session")

// database/sql normally replaces a bad connection and retries the statement.
// That would lose a session advisory lock. Each execution gets one attempt to
// connect; a subsequent execution must acquire a fresh lock on its own backend.
type retentionConnector struct {
	connector driver.Connector
	mu        sync.Mutex
	attempted bool
}

func (c *retentionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.attempted {
		return nil, errRetentionSessionLost
	}
	c.attempted = true
	return c.connector.Connect(ctx)
}

func (c *retentionConnector) Driver() driver.Driver { return c.connector.Driver() }

type retentionExecution struct {
	db      *sql.DB
	ctx     context.Context
	timeout time.Duration
	dialer  *retentionDialer
}

func newRetentionExecution(ctx context.Context, dsn string, timeout time.Duration) (*retentionExecution, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	dsn, err := retentionSessionDSN(dsn, timeout)
	if err != nil {
		return nil, err
	}
	connector, err := pq.NewConnector(dsn)
	if err != nil {
		// A driver parse error can contain the password from the DSN.
		return nil, errors.New("invalid retention database configuration")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	dialer := &retentionDialer{ctx: lifetime, cancel: cancel, timeout: min(timeout, 5*time.Second)}
	connector.Dialer(dialer)
	db := sql.OpenDB(&retentionConnector{connector: connector})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return &retentionExecution{db: db, ctx: ctx, timeout: timeout, dialer: dialer}, nil
}

func retentionSessionDSN(dsn string, timeout time.Duration) (string, error) {
	settings := map[string]string{
		"statement_timeout":                strconv.FormatInt(max(1, timeout.Milliseconds()), 10),
		"client_connection_check_interval": "100",
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", errors.New("invalid retention database configuration")
		}
		query := u.Query()
		for key, value := range settings {
			query.Set(key, value)
		}
		u.RawQuery = query.Encode()
		return u.String(), nil
	}
	for key, value := range settings {
		dsn += " " + key + "=" + value
	}
	return dsn, nil
}

func (s *retentionExecution) Close() {
	s.dialer.close()
	_ = s.db.Close()
}

func (s *retentionExecution) queryContext() (context.Context, context.CancelFunc) {
	return s.queryContextFor(s.ctx)
}

// A query may reuse this backend only after completing before its deadline.
// Once a query expires, close every session-owned socket, including a silent
// startup or pq cancellation request. Never reconnect without a new lock.
func (s *retentionExecution) queryContextFor(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, s.timeout)
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { s.dialer.close(); close(finished) })
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			if !stop() {
				<-finished
			}
			cancel()
		})
	}
}

func (s *retentionExecution) exec(query string, args ...any) (sql.Result, error) {
	ctx, cancel := s.queryContext()
	defer cancel()
	return s.db.ExecContext(ctx, query, args...)
}

func (s *retentionExecution) checkSession() error {
	ctx, cancel := s.queryContext()
	defer cancel()
	return s.db.PingContext(ctx)
}

func (s *retentionExecution) getQueryBuilder() sq.StatementBuilderType {
	return sq.StatementBuilder.PlaceholderFormat(sq.Dollar)
}

// The policy selection below follows the core SQL retention implementation:
// channel overrides team, either granular policy overrides the global policy,
// and deleted post IDs are committed in the same transaction as deletion.
// Keep these semantics aligned with the referenced core implementation when
// upgrading it. This private store does not run migrations or constructor
// maintenance and never changes the application's SQL pool.
type retentionPolicyBatchDeletionInfo struct {
	BaseBuilder         sq.SelectBuilder
	Table               string
	TimeColumn          string
	PrimaryKeys         []string
	ChannelIDTable      string
	NowMillis           int64
	GlobalPolicyEndTime int64
	Limit               int64
	StoreDeletedIds     bool
}

func executeRetentionDeletion(builder sq.SelectBuilder, r retentionPolicyBatchDeletionInfo, s *retentionExecution) (int64, error) {
	query, args, err := builder.ToSql()
	if err != nil {
		return 0, err
	}
	keys := "(" + strings.Join(r.PrimaryKeys, ",") + ")"
	query = fmt.Sprintf("DELETE FROM %s WHERE %s IN (%s)", r.Table, keys, query)
	if !r.StoreDeletedIds {
		result, err := s.exec(query, args...)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	}

	ctx, cancel := s.queryContext()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query+" RETURNING "+r.Table+"."+r.PrimaryKeys[0], args...)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	iterationErr := rows.Err()
	closeErr := rows.Close()
	if iterationErr != nil {
		return 0, iterationErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if len(ids) > 0 {
		journal := model.RetentionIdsForDeletion{TableName: r.Table, Ids: ids}
		journal.PreSave()
		if _, err = tx.ExecContext(ctx, `INSERT INTO RetentionIdsForDeletion (Id, TableName, Ids) VALUES ($1,$2,$3)`, journal.Id, journal.TableName, pq.Array(journal.Ids)); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(ids)), nil
}

func (s *retentionExecution) reactionIDs(limit int) ([]*model.RetentionIdsForDeletion, error) {
	ctx, cancel := s.queryContext()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT Id, TableName, Ids FROM RetentionIdsForDeletion WHERE TableName='Posts' LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []*model.RetentionIdsForDeletion
	for rows.Next() {
		row := &model.RetentionIdsForDeletion{}
		if err = rows.Scan(&row.Id, &row.TableName, pq.Array(&row.Ids)); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (s *retentionExecution) deleteReactions(row *model.RetentionIdsForDeletion) error {
	ctx, cancel := s.queryContext()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM Reactions WHERE PostId=ANY($1)`, pq.Array(row.Ids)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM RetentionIdsForDeletion WHERE Id=$1`, row.Id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *retentionExecution) deleteOrphanedPreferences(limit int) error {
	_, err := s.exec(`DELETE FROM Preferences WHERE ctid IN (
        SELECT Preferences.ctid FROM Preferences LEFT JOIN Posts ON Preferences.Name=Posts.Id
        WHERE Posts.Id IS NULL AND Category=$1 LIMIT $2)`, model.PreferenceCategoryFlaggedPost, limit)
	return err
}

func (s *retentionExecution) deleteOrphanedPolicies(limit int) error {
	for _, pair := range [][2]string{{"Channels", "ChannelId"}, {"Teams", "TeamId"}} {
		table, key := "RetentionPolicies"+pair[0], pair[1]
		_, err := s.exec(fmt.Sprintf(`DELETE FROM %s WHERE %s IN (
            SELECT %s FROM %s LEFT JOIN %s ON %s.%s=%s.Id WHERE %s.Id IS NULL LIMIT $1)`,
			table, key, key, table, pair[0], table, key, pair[0], pair[0]), limit)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *retentionExecution) expiredFileIDs(cutoff int64, limit int) ([]string, error) {
	ctx, cancel := s.queryContext()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT Id FROM FileInfo WHERE CreateAt<$1 AND CreatorId<>$2
        ORDER BY CreateAt ASC, Id ASC LIMIT $3`, cutoff, model.BookmarkFileOwner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *retentionExecution) postBatch(retentionPolicyBatchConfigs model.RetentionPolicyBatchConfigs, cursor model.RetentionPolicyCursor) (int64, model.RetentionPolicyCursor, error) {
	builder := s.getQueryBuilder().
		Select("Posts.Id").
		From("Posts")

	if retentionPolicyBatchConfigs.PreservePinnedPosts {
		builder = builder.Where(sq.Or{
			sq.Eq{"Posts.IsPinned": false},
			sq.And{
				sq.Eq{"Posts.IsPinned": true},
				sq.Gt{"Posts.DeleteAt": 0},
			},
		})
	}

	return deletePolicyBatch(retentionPolicyBatchDeletionInfo{
		BaseBuilder:         builder,
		Table:               "Posts",
		TimeColumn:          "CreateAt",
		PrimaryKeys:         []string{"Id"},
		ChannelIDTable:      "Posts",
		NowMillis:           retentionPolicyBatchConfigs.Now,
		GlobalPolicyEndTime: retentionPolicyBatchConfigs.GlobalPolicyEndTime,
		Limit:               retentionPolicyBatchConfigs.Limit,
		StoreDeletedIds:     true,
	}, s, cursor)
}

func (s *retentionExecution) threadBatch(retentionPolicyBatchConfigs model.RetentionPolicyBatchConfigs, cursor model.RetentionPolicyCursor) (int64, model.RetentionPolicyCursor, error) {
	builder := s.getQueryBuilder().
		Select("Threads.PostId").
		From("Threads")
	return deletePolicyBatch(retentionPolicyBatchDeletionInfo{
		BaseBuilder:         builder,
		Table:               "Threads",
		TimeColumn:          "LastReplyAt",
		PrimaryKeys:         []string{"PostId"},
		ChannelIDTable:      "Threads",
		NowMillis:           retentionPolicyBatchConfigs.Now,
		GlobalPolicyEndTime: retentionPolicyBatchConfigs.GlobalPolicyEndTime,
		Limit:               retentionPolicyBatchConfigs.Limit,
		StoreDeletedIds:     false,
	}, s, cursor)
}

func (s *retentionExecution) membershipBatch(retentionPolicyBatchConfigs model.RetentionPolicyBatchConfigs, cursor model.RetentionPolicyCursor) (int64, model.RetentionPolicyCursor, error) {
	builder := s.getQueryBuilder().
		Select("ThreadMemberships.PostId, ThreadMemberships.UserId").
		From("ThreadMemberships").
		InnerJoin("Threads ON ThreadMemberships.PostId = Threads.PostId")
	return deletePolicyBatch(retentionPolicyBatchDeletionInfo{
		BaseBuilder:         builder,
		Table:               "ThreadMemberships",
		TimeColumn:          "LastUpdated",
		PrimaryKeys:         []string{"PostId", "UserId"},
		ChannelIDTable:      "Threads",
		NowMillis:           retentionPolicyBatchConfigs.Now,
		GlobalPolicyEndTime: retentionPolicyBatchConfigs.GlobalPolicyEndTime,
		Limit:               retentionPolicyBatchConfigs.Limit,
		StoreDeletedIds:     false,
	}, s, cursor)
}

// The thread-policy pass runs first. Once it removes a thread, its memberships
// no longer join Threads and cannot be selected by membershipBatch. Delete
// only those orphans, at most one configured row batch at a time.
func (s *retentionExecution) orphanedMembershipBatch(limit int64) (int64, error) {
	result, err := s.exec(`DELETE FROM ThreadMemberships WHERE ctid IN (
        SELECT m.ctid FROM ThreadMemberships m
        WHERE NOT EXISTS(SELECT 1 FROM Threads t WHERE t.PostId=m.PostId)
        LIMIT $1)`, limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *retentionExecution) historyBatch(retentionPolicyBatchConfigs model.RetentionPolicyBatchConfigs, cursor model.RetentionPolicyCursor) (int64, model.RetentionPolicyCursor, error) {
	builder := s.getQueryBuilder().
		Select("ChannelMemberHistory.ChannelId, ChannelMemberHistory.UserId, ChannelMemberHistory.JoinTime").
		From("ChannelMemberHistory")
	return deletePolicyBatch(retentionPolicyBatchDeletionInfo{
		BaseBuilder:         builder,
		Table:               "ChannelMemberHistory",
		TimeColumn:          "LeaveTime",
		PrimaryKeys:         []string{"ChannelId", "UserId", "JoinTime"},
		ChannelIDTable:      "ChannelMemberHistory",
		NowMillis:           retentionPolicyBatchConfigs.Now,
		GlobalPolicyEndTime: retentionPolicyBatchConfigs.GlobalPolicyEndTime,
		Limit:               retentionPolicyBatchConfigs.Limit,
		StoreDeletedIds:     false,
	}, s, cursor)
}

func deletePolicyBatch(
	r retentionPolicyBatchDeletionInfo,
	s *retentionExecution,
	cursor model.RetentionPolicyCursor,
) (int64, model.RetentionPolicyCursor, error) {
	baseBuilder := r.BaseBuilder.InnerJoin("Channels ON " + r.ChannelIDTable + ".ChannelId = Channels.Id")

	scopedTimeColumn := r.Table + "." + r.TimeColumn
	nowStr := strconv.FormatInt(r.NowMillis, 10)
	// A record falls under the scope of a granular retention policy if:
	// 1. The policy's post duration is >= 0
	// 2. The record's lifespan has not exceeded the policy's post duration
	const millisecondsInADay = 24 * 60 * 60 * 1000
	fallsUnderGranularPolicy := sq.And{
		sq.GtOrEq{"RetentionPolicies.PostDuration": 0},
		sq.Expr(nowStr + " - " + scopedTimeColumn + " > RetentionPolicies.PostDuration * " + strconv.FormatInt(millisecondsInADay, 10)),
	}

	// If the caller wants to disable the global policy from running
	if r.GlobalPolicyEndTime <= 0 {
		cursor.GlobalPoliciesDone = true
	}
	// If the caller wants to disable the granular policies from running
	if r.NowMillis <= 0 {
		cursor.ChannelPoliciesDone = true
		cursor.TeamPoliciesDone = true
	}

	var totalRowsAffected int64

	// First, delete all of the records which fall under the scope of a channel-specific policy
	if !cursor.ChannelPoliciesDone {
		channelPoliciesBuilder := baseBuilder.
			InnerJoin("RetentionPoliciesChannels ON " + r.ChannelIDTable + ".ChannelId = RetentionPoliciesChannels.ChannelId").
			InnerJoin("RetentionPolicies ON RetentionPoliciesChannels.PolicyId = RetentionPolicies.Id").
			Where(fallsUnderGranularPolicy).
			Limit(uint64(r.Limit))
		rowsAffected, err := executeRetentionDeletion(channelPoliciesBuilder, r, s)
		if err != nil {
			return 0, cursor, err
		}
		if rowsAffected < r.Limit {
			cursor.ChannelPoliciesDone = true
		}
		totalRowsAffected += rowsAffected
		r.Limit -= rowsAffected
	}

	// Next, delete all of the records which fall under the scope of a team-specific policy
	if cursor.ChannelPoliciesDone && !cursor.TeamPoliciesDone {
		// Channel-specific policies override team-specific policies.
		teamPoliciesBuilder := baseBuilder.
			LeftJoin("RetentionPoliciesChannels ON " + r.ChannelIDTable + ".ChannelId = RetentionPoliciesChannels.ChannelId").
			InnerJoin("RetentionPoliciesTeams ON Channels.TeamId = RetentionPoliciesTeams.TeamId").
			InnerJoin("RetentionPolicies ON RetentionPoliciesTeams.PolicyId = RetentionPolicies.Id").
			Where(sq.And{
				sq.Eq{"RetentionPoliciesChannels.PolicyId": nil},
				sq.Expr("RetentionPoliciesTeams.PolicyId = RetentionPolicies.Id"),
			}).
			Where(fallsUnderGranularPolicy).
			Limit(uint64(r.Limit))
		rowsAffected, err := executeRetentionDeletion(teamPoliciesBuilder, r, s)
		if err != nil {
			return 0, cursor, err
		}
		if rowsAffected < r.Limit {
			cursor.TeamPoliciesDone = true
		}
		totalRowsAffected += rowsAffected
		r.Limit -= rowsAffected
	}

	// Finally, delete all of the records which fall under the scope of the global policy
	if cursor.ChannelPoliciesDone && cursor.TeamPoliciesDone && !cursor.GlobalPoliciesDone {
		// Granular policies override the global policy.
		globalPolicyBuilder := baseBuilder.
			LeftJoin("RetentionPoliciesChannels ON " + r.ChannelIDTable + ".ChannelId = RetentionPoliciesChannels.ChannelId").
			LeftJoin("RetentionPoliciesTeams ON Channels.TeamId = RetentionPoliciesTeams.TeamId").
			LeftJoin("RetentionPolicies ON RetentionPoliciesChannels.PolicyId = RetentionPolicies.Id").
			Where(sq.And{
				sq.Eq{"RetentionPoliciesChannels.PolicyId": nil},
				sq.Eq{"RetentionPoliciesTeams.PolicyId": nil},
			}).
			Where(sq.Lt{scopedTimeColumn: r.GlobalPolicyEndTime}).
			Limit(uint64(r.Limit))
		rowsAffected, err := executeRetentionDeletion(globalPolicyBuilder, r, s)
		if err != nil {
			return 0, cursor, err
		}
		if rowsAffected < r.Limit {
			cursor.GlobalPoliciesDone = true
		}
		totalRowsAffected += rowsAffected
	}

	return totalRowsAffected, cursor, nil
}
