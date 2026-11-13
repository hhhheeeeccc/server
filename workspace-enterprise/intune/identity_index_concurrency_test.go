package intune

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIntuneConcurrentIndexInitializationDoesNotRetainWaitingSnapshot(t *testing.T) {
	f := setupIntune(t)
	db := f.s.db
	_, err := db.Exec(`DROP INDEX CONCURRENTLY idx_workspace_intune_users_auth_lower`)
	require.NoError(t, err)
	writer, err := db.Begin()
	require.NoError(t, err)
	defer writer.Rollback()
	_, err = writer.Exec(`UPDATE users SET updateat=updateat WHERE id=$1`, f.th.BasicUser.Id)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- f.s.ensureLookupIndex(ctx) }()
	require.Eventually(t, func() bool {
		var building bool
		return db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_progress_create_index
			WHERE relid='users'::regclass AND command='CREATE INDEX CONCURRENTLY')`).Scan(&building) == nil && building
	}, time.Second, 10*time.Millisecond)
	go func() { second <- f.s.ensureLookupIndex(ctx) }()
	// The first node is blocked by the intentional writer. A second node must
	// wait for its advisory lock without retaining an old statement snapshot.
	select {
	case err := <-second:
		t.Fatalf("second initializer returned before the first completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, writer.Rollback())
	require.NoError(t, <-first, "the lock waiter must not block the owner's final index validation")
	require.NoError(t, <-second)
	var ready bool
	require.NoError(t, db.QueryRow(`SELECT indisvalid AND indisready FROM pg_index
		WHERE indexrelid='idx_workspace_intune_users_auth_lower'::regclass`).Scan(&ready))
	require.True(t, ready)
}

func TestIntuneConflictingLookupIndexesAreLeftUnchanged(t *testing.T) {
	f := setupIntune(t)
	db := f.s.db
	// The package shares its SQL fixture across tests. Leave the original
	// expected index restored after proving conflicting definitions survive.
	t.Cleanup(func() {
		_, err := db.Exec(`DROP INDEX CONCURRENTLY IF EXISTS idx_workspace_intune_users_auth_lower`)
		require.NoError(t, err)
		require.NoError(t, f.s.ensureLookupIndex(context.Background()))
	})
	for _, kind := range []string{"valid", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			_, err := db.Exec(`DROP INDEX CONCURRENTLY idx_workspace_intune_users_auth_lower`)
			require.NoError(t, err)
			if kind == "valid" {
				_, err = db.Exec(`CREATE INDEX CONCURRENTLY idx_workspace_intune_users_auth_lower ON users(authservice)`)
				require.NoError(t, err)
			} else {
				// Native local users share an empty authservice, so uniqueness
				// fails and PostgreSQL retains this deliberately different index.
				_, err = db.Exec(`CREATE UNIQUE INDEX CONCURRENTLY idx_workspace_intune_users_auth_lower ON users(authservice)`)
				require.Error(t, err)
			}
			var beforeOID, afterOID int64
			var beforeDefinition, afterDefinition string
			const inspect = `SELECT c.oid,pg_get_indexdef(c.oid) FROM pg_class c WHERE c.oid='idx_workspace_intune_users_auth_lower'::regclass`
			require.NoError(t, db.QueryRow(inspect).Scan(&beforeOID, &beforeDefinition))
			err = f.s.ensureLookupIndex(context.Background())
			require.ErrorContains(t, err, "unexpected definition")
			require.NoError(t, db.QueryRow(inspect).Scan(&afterOID, &afterDefinition))
			require.Equal(t, beforeOID, afterOID)
			require.Equal(t, beforeDefinition, afterDefinition)
		})
	}
}
