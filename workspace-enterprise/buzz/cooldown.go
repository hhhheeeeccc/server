package buzz

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

// A shared reservation serializes attempts across nodes without holding a pool
// connection while the native post handler runs. Expired entries are reused;
// there is at most one row per user/channel pair that has sent a Buzz.
func (s *Service) reserveCooldown(ctx context.Context, channelID, userID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s.schemaMu.Lock()
	if !s.schemaReady {
		tx, err := s.app.Srv().Store().GetInternalMasterDB().BeginTx(ctx, nil)
		if err != nil {
			s.schemaMu.Unlock()
			return "", err
		}
		defer tx.Rollback()
		// Serialize first-use DDL across servers sharing this database.
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(6492261421)`); err == nil {
			_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS workspace_buzz_cooldowns (
				channelid varchar(26) NOT NULL, userid varchar(26) NOT NULL,
				token varchar(26) NOT NULL, expiresat bigint NOT NULL,
				PRIMARY KEY (channelid, userid))`)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			s.schemaMu.Unlock()
			return "", err
		}
		s.schemaReady = true
	}
	s.schemaMu.Unlock()

	// Consult existing posts as well, so upgrades, deleted reservation rows or
	// an earlier in-flight attempt cannot erase the last successful Buzz.
	// The existing channel/createat index bounds this scan to three minutes.
	token := model.NewId()
	var reserved string
	err := s.app.Srv().Store().GetInternalMasterDB().QueryRowContext(ctx, `
		WITH clock AS (SELECT (EXTRACT(EPOCH FROM statement_timestamp()) * 1000)::bigint AS now)
		INSERT INTO workspace_buzz_cooldowns(channelid, userid, token, expiresat)
		SELECT $1::varchar(26), $2::varchar(26), $3::varchar(26), clock.now + 600000 FROM clock
		WHERE NOT EXISTS (SELECT 1 FROM posts WHERE channelid=$1 AND userid=$2
			AND message=$4 AND deleteat=0 AND createat > clock.now - $5::bigint)
		ON CONFLICT (channelid, userid) DO UPDATE
		SET token=EXCLUDED.token, expiresat=EXCLUDED.expiresat
		WHERE workspace_buzz_cooldowns.expiresat <= (SELECT now FROM clock)
		RETURNING token`, channelID, userID, token, buzzMessage, buzzCooldownMS).Scan(&reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return reserved, err
}

func (s *Service) finishCooldown(ctx context.Context, channelID, userID, token string, success bool) error {
	// A client disconnect must not leave a failed send reserving the cooldown.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	db := s.app.Srv().Store().GetInternalMasterDB()
	if success {
		_, err := db.ExecContext(ctx, `UPDATE workspace_buzz_cooldowns
			SET expiresat=(EXTRACT(EPOCH FROM statement_timestamp())*1000)::bigint + $4::bigint
			WHERE channelid=$1 AND userid=$2 AND token=$3`, channelID, userID, token, buzzCooldownMS)
		return err
	}
	_, err := db.ExecContext(ctx, `DELETE FROM workspace_buzz_cooldowns
		WHERE channelid=$1 AND userid=$2 AND token=$3`, channelID, userID, token)
	return err
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}
