package account_linking

import (
	"context"
	"database/sql"
	"fmt"
)

// Restore records whose owner was changed by the older, data-moving merge.
// Only rows recorded in that merge's archive are touched; newer local posts
// and files remain with the local account.
func restoreSourceOwnership(ctx context.Context, tx *sql.Tx, sourceID, targetID string, mergeAt int64) error {
	for _, item := range []struct{ table, column string }{
		{"posts", "userid"}, {"fileinfo", "creatorid"}, {"channels", "creatorid"},
	} {
		var expected int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM workspace_openid_merge_archive
			WHERE sourceid=$1 AND targetid=$2 AND tablename=$3`, sourceID, targetID, item.table).Scan(&expected); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE `+item.table+` current SET `+item.column+`=$1
			FROM workspace_openid_merge_archive archive
			WHERE archive.sourceid=$1 AND archive.targetid=$2 AND archive.tablename=$3
			AND current.id=archive.rowdata->>'id' AND current.`+item.column+` IN ($1,$2)`, sourceID, targetID, item.table)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != expected {
			return &mergeBlockedError{fmt.Sprintf("archived %s ownership has changed", item.table)}
		}
	}
	// A team membership transferred from the OpenID user to a previously
	// unjoined local user retains the source creation time. Remove it only if
	// its complete contents still match the archived source membership.
	if _, err := tx.ExecContext(ctx, `DELETE FROM teammembers current USING workspace_openid_merge_archive archive
		WHERE archive.sourceid=$1 AND archive.targetid=$2 AND archive.tablename='teammembers'
		AND current.userid=$2 AND current.teamid=archive.rowdata->>'teamid'
		AND to_jsonb(current)-'userid'=archive.rowdata-'userid'`, sourceID, targetID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO teammembers
		SELECT (jsonb_populate_record(NULL::teammembers,archive.rowdata)).*
		FROM workspace_openid_merge_archive archive WHERE archive.sourceid=$1 AND archive.targetid=$2
		AND archive.tablename='teammembers' ON CONFLICT DO NOTHING`, sourceID, targetID); err != nil {
		return err
	}
	// Direct channels renamed without being folded into another direct
	// channel must regain their old name and member. A new post or file in
	// such a channel makes attribution ambiguous, so leave the merge intact.
	var changed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM workspace_openid_merge_archive archive JOIN posts p
		ON p.channelid=archive.rowdata->>'id' WHERE archive.sourceid=$1 AND archive.targetid=$2
		AND archive.tablename='direct_channel' AND (p.createat>$3 OR p.updateat>$3)
		UNION ALL
		SELECT 1 FROM workspace_openid_merge_archive archive JOIN fileinfo f
		ON f.channelid=archive.rowdata->>'id' WHERE archive.sourceid=$1 AND archive.targetid=$2
		AND archive.tablename='direct_channel' AND (f.createat>$3 OR f.updateat>$3))`,
		sourceID, targetID, mergeAt).Scan(&changed); err != nil {
		return err
	}
	if changed {
		return &mergeBlockedError{"a renamed direct channel changed after linking"}
	}
	result, err := tx.ExecContext(ctx, `UPDATE channels current SET name=archive.rowdata->>'name',
		displayname=archive.rowdata->>'displayname',updateat=(archive.rowdata->>'updateat')::bigint
		FROM workspace_openid_merge_archive archive WHERE archive.sourceid=$1 AND archive.targetid=$2
		AND archive.tablename='direct_channel' AND current.id=archive.rowdata->>'id'
		AND current.deleteat=0`, sourceID, targetID)
	if err != nil {
		return err
	}
	var expectedRenames int64
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM workspace_openid_merge_archive
		WHERE sourceid=$1 AND targetid=$2 AND tablename='direct_channel'`, sourceID, targetID).Scan(&expectedRenames); err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != expectedRenames {
		return &mergeBlockedError{"a renamed direct channel is missing"}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM channelmembers current USING workspace_openid_merge_archive archive
		WHERE archive.sourceid=$1 AND archive.targetid=$2 AND archive.tablename='direct_channel'
		AND current.channelid=archive.rowdata->>'id' AND current.userid=$2
		AND EXISTS(SELECT 1 FROM workspace_openid_merge_archive old
			WHERE old.sourceid=$1 AND old.targetid=$2 AND old.tablename='channelmembers'
			AND old.rowdata->>'channelid'=current.channelid)`, sourceID, targetID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO channelmembers
		SELECT (jsonb_populate_record(NULL::channelmembers,archive.rowdata)).*
		FROM workspace_openid_merge_archive archive WHERE archive.sourceid=$1 AND archive.targetid=$2
		AND archive.tablename='channelmembers' ON CONFLICT DO NOTHING`, sourceID, targetID); err != nil {
		return err
	}
	// The merge rejected duplicate history keys before transferring ownership.
	// This lets us identify the transferred records by channel and join time.
	if _, err = tx.ExecContext(ctx, `DELETE FROM channelmemberhistory current USING workspace_openid_merge_archive archive
		WHERE archive.sourceid=$1 AND archive.targetid=$2 AND archive.tablename='channelmemberhistory'
		AND current.userid=$2 AND current.channelid=archive.rowdata->>'channelid'
		AND current.jointime=(archive.rowdata->>'jointime')::bigint`, sourceID, targetID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO channelmemberhistory
		SELECT (jsonb_populate_record(NULL::channelmemberhistory,archive.rowdata)).*
		FROM workspace_openid_merge_archive archive WHERE archive.sourceid=$1 AND archive.targetid=$2
		AND archive.tablename='channelmemberhistory' ON CONFLICT DO NOTHING`, sourceID, targetID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sidebarcategories
		SELECT (jsonb_populate_record(NULL::sidebarcategories,archive.rowdata)).*
		FROM workspace_openid_merge_archive archive WHERE archive.sourceid=$1 AND archive.targetid=$2
		AND archive.tablename='sidebarcategories' ON CONFLICT DO NOTHING`, sourceID, targetID); err != nil {
		return err
	}
	return nil
}

func restoreNamedPreferences(ctx context.Context, tx *sql.Tx, sourceID, targetID string) error {
	// A source-ID preference renamed during merging was created on the target
	// only when the target lacked that key. Preserve a pre-existing target key.
	if _, err := tx.ExecContext(ctx, `DELETE FROM preferences current USING workspace_openid_merge_archive archive
		WHERE archive.sourceid=$1 AND archive.targetid=$2 AND archive.tablename='preferences_name'
		AND current.userid=archive.rowdata->>'userid' AND current.category=archive.rowdata->>'category'
		AND current.name=$2 AND current.value=archive.rowdata->>'value'
		AND NOT EXISTS(SELECT 1 FROM workspace_openid_merge_archive original
			WHERE original.sourceid=$1 AND original.targetid=$2 AND original.tablename='target_preferences'
			AND original.rowdata->>'userid'=current.userid AND original.rowdata->>'category'=current.category
			AND original.rowdata->>'name'=$2)`, sourceID, targetID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO preferences
		SELECT (jsonb_populate_record(NULL::preferences,archive.rowdata)).*
		FROM workspace_openid_merge_archive archive WHERE archive.sourceid=$1 AND archive.targetid=$2
		AND archive.tablename='preferences_name' ON CONFLICT DO NOTHING`, sourceID, targetID)
	return err
}
