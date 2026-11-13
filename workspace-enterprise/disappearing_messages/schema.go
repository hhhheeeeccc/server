package disappearing_messages

import (
	"context"
	"fmt"

	"github.com/lib/pq"
)

func (r repository) install(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(834921906); CREATE SCHEMA IF NOT EXISTS workspace_message_expiry;
	 CREATE TABLE IF NOT EXISTS workspace_message_expiry.policies (
	 channel_id varchar(26) PRIMARY KEY,duration_seconds bigint NOT NULL CHECK(duration_seconds IN(0,86400,604800,7776000)),updated_by varchar(26) NOT NULL);
	 CREATE TABLE IF NOT EXISTS workspace_message_expiry.messages (
	 post_id varchar(26) PRIMARY KEY,channel_id varchar(26) NOT NULL,sent_at bigint NOT NULL,expires_at bigint NOT NULL,
	 next_attempt_at bigint NOT NULL,completed_at bigint NOT NULL DEFAULT 0,claim_token varchar(26) NOT NULL DEFAULT '');
	 CREATE INDEX IF NOT EXISTS workspace_message_expiry_due ON workspace_message_expiry.messages(next_attempt_at,post_id) WHERE completed_at=0;`); err != nil {
		return err
	}
	// Resolve the actual host schema once. The persisted function cannot depend
	// on a caller-controlled search_path or interpolated request input.
	var schema string
	if err = tx.QueryRowContext(ctx, `SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.oid='posts'::regclass`).Scan(&schema); err != nil {
		return err
	}
	posts := pq.QuoteIdentifier(schema) + `.posts`
	statement := fmt.Sprintf(`CREATE OR REPLACE FUNCTION workspace_message_expiry.enroll() RETURNS trigger
	 LANGUAGE plpgsql SET search_path=pg_catalog AS $fn$
	 DECLARE duration bigint; sent bigint; parent_expiry bigint; deadline bigint;
	 BEGIN
	 IF TG_OP='UPDATE' THEN
	   IF NEW.deleteat=0 AND EXISTS(SELECT 1 FROM workspace_message_expiry.messages WHERE post_id=OLD.id AND expires_at<=floor(extract(epoch FROM clock_timestamp())*1000)::bigint) THEN
	     RAISE EXCEPTION 'This message has expired';
	   END IF;
	   RETURN NEW;
	 END IF;
	 IF NEW.type LIKE 'system_%%' THEN RETURN NEW; END IF;
	 SELECT duration_seconds INTO duration FROM workspace_message_expiry.policies WHERE channel_id=NEW.channelid FOR SHARE;
	 IF NEW.rootid<>'' THEN
	   SELECT expires_at INTO parent_expiry FROM workspace_message_expiry.messages WHERE post_id=NEW.rootid;
	 END IF;
	 IF COALESCE(duration,0)=0 AND parent_expiry IS NULL THEN RETURN NEW; END IF;
	 IF NEW.type<>'' THEN RAISE EXCEPTION 'Disappearing messages must use the normal message type'; END IF;
	 sent:=floor(extract(epoch FROM clock_timestamp())*1000)::bigint;
	 deadline:=CASE WHEN COALESCE(duration,0)>0 THEN sent+duration*1000 ELSE parent_expiry END;
	 IF parent_expiry IS NOT NULL THEN deadline:=least(deadline,parent_expiry); END IF;
	 IF deadline<=sent THEN RAISE EXCEPTION 'This message thread has expired'; END IF;
	 INSERT INTO workspace_message_expiry.messages(post_id,channel_id,sent_at,expires_at,next_attempt_at)
	 VALUES(NEW.id,NEW.channelid,sent,deadline,deadline);
	 RETURN NEW;
	 END $fn$;
	 DROP TRIGGER IF EXISTS workspace_message_expiry_enroll ON %s;
	 CREATE TRIGGER workspace_message_expiry_enroll BEFORE INSERT OR UPDATE ON %s
	 FOR EACH ROW EXECUTE FUNCTION workspace_message_expiry.enroll();`, posts, posts)
	if _, err = tx.ExecContext(ctx, statement); err != nil {
		return err
	}
	return tx.Commit()
}
