package database_migration

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRepairThreadMembershipIndexesPreservesRowsAndIsIdempotent(t *testing.T) {
	for _, existing := range []string{"", `CREATE INDEX custom_user ON threadmemberships(userid);
		CREATE INDEX custom_updated ON threadmemberships(lastupdated);
		CREATE INDEX custom_viewed ON threadmemberships(lastviewed)`} {
		db := licenseTestDatabase(t)
		_, err := db.Exec(`CREATE TABLE threadmemberships(postid text,userid text,following boolean,lastupdated bigint,lastviewed bigint,PRIMARY KEY(postid,userid));
			INSERT INTO threadmemberships SELECT i::text,'user-'||(i%10),true,i,i-1 FROM generate_series(1,100)i;` + existing)
		require.NoError(t, err)
		var before, after string
		const snapshot = `SELECT jsonb_agg(to_jsonb(t) ORDER BY postid,userid)::text FROM threadmemberships t`
		require.NoError(t, db.QueryRow(snapshot).Scan(&before))
		changed, err := RepairThreadMembershipIndexes(context.Background(), db)
		require.NoError(t, err)
		require.Equal(t, existing == "", changed)
		changed, err = RepairThreadMembershipIndexes(context.Background(), db)
		require.NoError(t, err)
		require.False(t, changed)
		var indexes int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_index WHERE indrelid='threadmemberships'::regclass AND indisvalid`).Scan(&indexes))
		require.Equal(t, 4, indexes)
		require.NoError(t, db.QueryRow(snapshot).Scan(&after))
		require.Equal(t, before, after)
	}
}

func TestRepairThreadMembershipIndexesRejectsConflictsAtomically(t *testing.T) {
	db := licenseTestDatabase(t)
	_, err := db.Exec(`CREATE TABLE threadmemberships(postid text,userid text,lastupdated bigint,lastviewed bigint,PRIMARY KEY(postid,userid));
		CREATE INDEX idx_thread_memberships_user_id ON threadmemberships(postid)`)
	require.NoError(t, err)
	changed, err := RepairThreadMembershipIndexes(context.Background(), db)
	require.Error(t, err)
	require.False(t, changed)
	var indexes int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_index WHERE indrelid='threadmemberships'::regclass`).Scan(&indexes))
	require.Equal(t, 2, indexes, "a conflicting object must not cause partial repairs")
}
