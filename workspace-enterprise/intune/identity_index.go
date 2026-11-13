package intune

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

const intuneIndexLockKey int64 = 0x57736b496e74756e

func (s *Service) ensureLookupIndex(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Register cleanup before attempting acquisition: cancellation can race with
	// a successful grant. Never return a session holding our lock to the pool.
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, intuneIndexLockKey).Scan(&unlocked); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	// A blocking advisory-lock statement can retain a snapshot which the
	// owning node's concurrent index build then waits for. End each attempt
	// before waiting so that the owner can finish its validation phase.
	retry := time.NewTicker(100 * time.Millisecond)
	defer retry.Stop()
	for {
		var acquired bool
		if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, intuneIndexLockKey).Scan(&acquired); err != nil {
			return err
		}
		if acquired {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-retry.C:
		}
	}

	var tableID int64
	var schema, table string
	if err = conn.QueryRowContext(ctx, `SELECT c.oid,n.nspname,c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.oid=to_regclass('users') AND c.relkind='r'`).Scan(&tableID, &schema, &table); err != nil {
		return err
	}
	var valid, matches bool
	err = conn.QueryRowContext(ctx, `SELECT COALESCE(i.indisvalid AND i.indisready,false),
		COALESCE(i.indrelid=$1::oid AND NOT i.indisunique AND i.indnatts=2 AND i.indnkeyatts=2
		AND am.amname='btree' AND pg_get_indexdef(c.oid,1,true)='authservice'
		AND pg_get_indexdef(c.oid,2,true) IN ('lower(authdata::text)','lower(authdata)','lower((authdata)::text)')
		AND pg_get_expr(i.indpred,i.indrelid)='(authdata IS NOT NULL)',false)
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		LEFT JOIN pg_index i ON i.indexrelid=c.oid LEFT JOIN pg_am am ON am.oid=c.relam
		WHERE n.nspname=$2 AND c.relname='idx_workspace_intune_users_auth_lower'`, tableID, schema).Scan(&valid, &matches)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	qualifiedIndex := pq.QuoteIdentifier(schema) + ".idx_workspace_intune_users_auth_lower"
	if err == nil {
		if !matches {
			return fmt.Errorf("Intune identity index %s has an unexpected definition; left unchanged", qualifiedIndex)
		}
		if valid {
			return nil
		}
		if _, err = conn.ExecContext(ctx, `DROP INDEX CONCURRENTLY `+qualifiedIndex); err != nil {
			return err
		}
	}
	// A canceled concurrent build can leave an invalid index behind. The
	// session lock serializes repairs across Enterprise nodes.
	_, err = conn.ExecContext(ctx, `CREATE INDEX CONCURRENTLY idx_workspace_intune_users_auth_lower
		ON `+pq.QuoteIdentifier(schema)+`.`+pq.QuoteIdentifier(table)+` (authservice, lower(authdata)) WHERE authdata IS NOT NULL`)
	return err
}
