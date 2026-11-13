package account_linking

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

// A fast link transfers only the OpenID sign-in identifier. The former OpenID
// user stays in the database with all of its content, but cannot sign in until
// the link is removed. Neither account's posts or memberships are rewritten.
func (s *Service) initFastLinks() {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		s.app.Log().Error("Unable to initialize OpenID links: database is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS workspace_openid_fast_links (
		targetid varchar(26) PRIMARY KEY REFERENCES users(id),
		sourceid varchar(26) UNIQUE NOT NULL REFERENCES users(id),
		createdat bigint NOT NULL,
		subject text)`)
	if err == nil {
		_, err = db.ExecContext(ctx, `ALTER TABLE workspace_openid_fast_links ADD COLUMN IF NOT EXISTS subject text`)
	}
	if err != nil {
		s.app.Log().Error("Unable to initialize OpenID links", mlog.Err(err))
	}
}

func (s *Service) linkOpenIDIdentity(ctx context.Context, sourceID, targetID string) error {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return errors.New("master database unavailable")
	}
	return linkOpenIDIdentityDB(ctx, db, sourceID, targetID)
}

func linkOpenIDIdentityDB(ctx context.Context, db *sql.DB, sourceID, targetID string) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT id,authservice,authdata,password,username,deleteat
		FROM users WHERE id IN ($1,$2) ORDER BY id FOR UPDATE`, sourceID, targetID)
	if err != nil {
		return err
	}
	type identity struct {
		service, password, username string
		auth                        sql.NullString
		deleted                     int64
	}
	accounts := make(map[string]identity, 2)
	for rows.Next() {
		var id string
		var user identity
		if err = rows.Scan(&id, &user.service, &user.auth, &user.password, &user.username, &user.deleted); err != nil {
			_ = rows.Close()
			return err
		}
		accounts[id] = user
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	source, sourceOK := accounts[sourceID]
	target, targetOK := accounts[targetID]
	if sourceID == targetID || !sourceOK || !targetOK || source.deleted != 0 || target.deleted != 0 ||
		source.service != model.ServiceOpenid || !source.auth.Valid || source.auth.String == "" ||
		target.service != "" || target.auth.Valid || target.password == "" {
		return &mergeBlockedError{"select an active OpenID account and an active local account with a password"}
	}
	var blocked bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM bots WHERE userid IN ($1,$2)) OR
		EXISTS(SELECT 1 FROM workspace_openid_account_merges WHERE sourceid IN ($1,$2) OR targetid IN ($1,$2)) OR
		EXISTS(SELECT 1 FROM workspace_linked_credentials WHERE userid IN ($1,$2))`, sourceID, targetID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return &mergeBlockedError{"one of these accounts is already linked or is a bot"}
	}
	now := model.GetMillis()
	if err = upsertCredential(ctx, tx, targetID, target.password, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE userid=$1`, sourceID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET authservice='',authdata=NULL,password='',deleteat=$2,updateat=$2
		WHERE id=$1`, sourceID, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET authservice=$2,authdata=$3,password='',updateat=$4
		WHERE id=$1`, targetID, model.ServiceOpenid, source.auth.String, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_openid_account_merges
		(targetid,sourceid,sourceusername,createat) VALUES ($1,$2,$3,$4)`, targetID, sourceID, source.username, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_openid_fast_links (targetid,sourceid,createdat,subject)
		VALUES ($1,$2,$3,$4)`, targetID, sourceID, now, source.auth.String); err != nil {
		return err
	}
	return tx.Commit()
}

func unlinkOpenIDIdentityDB(ctx context.Context, db *sql.DB, targetID string) (string, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var sourceID string
	var linkedSubject sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT sourceid,subject FROM workspace_openid_fast_links WHERE targetid=$1 FOR UPDATE`, targetID).Scan(&sourceID, &linkedSubject); err != nil {
		return "", err
	}
	var targetService, targetPassword, sourceService, sourcePassword string
	var targetAuth, sourceAuth sql.NullString
	var sourceDeleted int64
	if err = tx.QueryRowContext(ctx, `SELECT authservice,authdata,password FROM users WHERE id=$1 FOR UPDATE`, targetID).
		Scan(&targetService, &targetAuth, &targetPassword); err != nil {
		return "", err
	}
	if err = tx.QueryRowContext(ctx, `SELECT authservice,authdata,password,deleteat FROM users WHERE id=$1 FOR UPDATE`, sourceID).
		Scan(&sourceService, &sourceAuth, &sourcePassword, &sourceDeleted); err != nil {
		return "", err
	}
	var savedPassword string
	if err = tx.QueryRowContext(ctx, `SELECT password FROM workspace_linked_credentials WHERE userid=$1 FOR UPDATE`, targetID).Scan(&savedPassword); err != nil {
		return "", err
	}
	if targetService != model.ServiceOpenid || !targetAuth.Valid || targetAuth.String == "" ||
		(linkedSubject.Valid && targetAuth.String != linkedSubject.String) ||
		savedPassword == "" || targetPassword != "" || sourceService != "" || sourceAuth.Valid ||
		sourcePassword != "" || sourceDeleted == 0 {
		return "", &mergeBlockedError{"linked login details changed; accounts were left untouched"}
	}
	now := model.GetMillis()
	if _, err = tx.ExecContext(ctx, `UPDATE users SET authservice='',authdata=NULL,password=$2,updateat=$3 WHERE id=$1`, targetID, savedPassword, now); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET authservice=$2,authdata=$3,password='',deleteat=0,updateat=$4 WHERE id=$1`, sourceID, model.ServiceOpenid, targetAuth.String, now); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE userid IN ($1,$2)`, sourceID, targetID); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM workspace_linked_credentials WHERE userid=$1`, targetID); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM workspace_openid_fast_links WHERE targetid=$1`, targetID); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM workspace_openid_account_merges WHERE targetid=$1 AND sourceid=$2`, targetID, sourceID); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return sourceID, nil
}
