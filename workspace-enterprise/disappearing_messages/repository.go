package disappearing_messages

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

const queryTimeout = 5 * time.Second

var errForbidden = errors.New("not an eligible conversation member")

type repository struct{ db *sql.DB }

func (r repository) policy(ctx context.Context, channelID string) (Policy, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var p Policy
	err := r.db.QueryRowContext(ctx, `SELECT duration_seconds FROM workspace_message_expiry.policies WHERE channel_id=$1`, channelID).Scan(&p.DurationSeconds)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	return p, err
}

func (r repository) setPolicy(ctx context.Context, channelID, userID string, p Policy) error {
	if err := validateDuration(p.DurationSeconds); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	// Eligibility and membership are checked on the primary in the same
	// statement as the policy write; an administrator has no implicit access.
	result, err := r.db.ExecContext(ctx, `INSERT INTO workspace_message_expiry.policies(channel_id,duration_seconds,updated_by)
	 SELECT c.id,$3,$2 FROM channels c
	 JOIN channelmembers cm ON cm.channelid=c.id AND cm.userid=$2
	 JOIN users u ON u.id=cm.userid AND u.deleteat=0
	 WHERE c.id=$1 AND c.type='D' AND c.deleteat=0 AND NOT COALESCE(c.shared,false)
	 AND (SELECT count(*) FROM channelmembers members JOIN users member_user ON member_user.id=members.userid
	   WHERE members.channelid=c.id AND member_user.deleteat=0
	   AND NOT EXISTS(SELECT 1 FROM bots b WHERE b.userid=members.userid))=2
	 AND (SELECT count(*) FROM channelmembers members WHERE members.channelid=c.id)=2
	 ON CONFLICT(channel_id) DO UPDATE SET duration_seconds=EXCLUDED.duration_seconds,updated_by=EXCLUDED.updated_by`, channelID, userID, p.DurationSeconds)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return errForbidden
	}
	return err
}

type dueMessage struct {
	PostID string
	Token  string
}

func (r repository) claim(ctx context.Context) (*dueMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	m := &dueMessage{Token: model.NewId()}
	err := r.db.QueryRowContext(ctx, `WITH clock AS (SELECT floor(extract(epoch FROM clock_timestamp())*1000)::bigint AS now)
	 UPDATE workspace_message_expiry.messages SET claim_token=$1,next_attempt_at=(SELECT now+30000 FROM clock)
	 WHERE post_id=(SELECT post_id FROM workspace_message_expiry.messages
	 WHERE completed_at=0 AND next_attempt_at<=(SELECT now FROM clock) AND expires_at<=(SELECT now FROM clock)
	 ORDER BY next_attempt_at,post_id LIMIT 1 FOR UPDATE SKIP LOCKED)
	 RETURNING post_id`, m.Token).Scan(&m.PostID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

func (r repository) complete(ctx context.Context, m *dueMessage) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	_, err := r.db.ExecContext(ctx, `UPDATE workspace_message_expiry.messages SET completed_at=floor(extract(epoch FROM clock_timestamp())*1000)::bigint WHERE post_id=$1 AND claim_token=$2`, m.PostID, m.Token)
	return err
}
