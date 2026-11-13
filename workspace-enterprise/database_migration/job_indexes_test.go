package database_migration

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRepairJobIndexesPreservesDataAndIsIdempotent(t *testing.T) {
	for _, existing := range []string{"", `CREATE INDEX custom_status_type ON jobs(status,type); CREATE INDEX custom_type ON jobs(type)`} {
		db := licenseTestDatabase(t)
		_, err := db.Exec(`CREATE TABLE jobs(id text PRIMARY KEY,type text,status text,createat bigint);
			INSERT INTO jobs SELECT i::text,'test',CASE WHEN i=1 THEN 'pending' ELSE 'success' END,i FROM generate_series(1,1000)i`)
		require.NoError(t, err)
		if existing != "" {
			_, err = db.Exec(existing)
			require.NoError(t, err)
		}
		var before, after string
		require.NoError(t, db.QueryRow(`SELECT jsonb_agg(to_jsonb(j) ORDER BY id)::text FROM jobs j`).Scan(&before))
		changed, err := RepairJobIndexes(context.Background(), db)
		require.NoError(t, err)
		require.Equal(t, existing == "", changed)
		changed, err = RepairJobIndexes(context.Background(), db)
		require.NoError(t, err)
		require.False(t, changed)
		var indexes int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_index WHERE indrelid='jobs'::regclass AND indisvalid`).Scan(&indexes))
		require.Equal(t, 3, indexes)
		require.NoError(t, db.QueryRow(`SELECT jsonb_agg(to_jsonb(j) ORDER BY id)::text FROM jobs j`).Scan(&after))
		require.Equal(t, before, after)
	}
}

func TestRepairJobIndexesRejectsConflictingDefinition(t *testing.T) {
	db := licenseTestDatabase(t)
	_, err := db.Exec(`CREATE TABLE jobs(id text PRIMARY KEY,type text,status text,createat bigint);
		CREATE INDEX idx_jobs_status_type ON jobs(createat)`)
	require.NoError(t, err)
	changed, err := RepairJobIndexes(context.Background(), db)
	require.ErrorContains(t, err, "conflicts")
	require.False(t, changed)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_index WHERE indrelid='jobs'::regclass`).Scan(&count))
	require.Equal(t, 2, count, "conflict must not cause partial repairs")
}
