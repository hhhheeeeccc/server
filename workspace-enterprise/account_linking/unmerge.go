package account_linking

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

// The archive restores supported pre-merge state. New content outside a
// merged direct channel can remain with the local account when requested.
// Other shapes fail closed.
func (s *Service) registerOpenIDUnmerge() {
	handler := s.unmergeOpenIDAccount
	s.app.Srv().Router.Handle(model.APIURLSuffix+"/users/{user_id:[A-Za-z0-9]+}/openid_unmerge", &web.Handler{
		Srv: s.app.Srv(), HandleFunc: handler, HandlerName: web.GetHandlerName(handler),
		RequireSession: true, RequireMfa: true,
	}).Methods(http.MethodPost)
}

func (s *Service) unmergeOpenIDAccount(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !c.IsSystemAdmin() {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}
	if c.App.License() == nil {
		c.Err = model.NewAppError("UnmergeOpenIDAccount", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	c.RequireUserId()
	if c.Err != nil {
		return
	}
	auditRec := c.MakeAuditRecord(model.AuditEventUpdateUserAuth, model.AuditStatusFail)
	auditRec.AddMeta("target_user_id", c.Params.UserId)
	defer c.LogAuditRec(auditRec)
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	var request unmergeRequest
	if err := decodeOpenIDActionRequest(w, r, &request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("UnmergeOpenIDAccount", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("preserve_new_on_local", err)
		return
	}
	db := s.app.Srv().Store().GetInternalMasterDB()
	var fastLink bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_openid_fast_links WHERE targetid=$1)`, c.Params.UserId).Scan(&fastLink); err != nil {
		c.Logger.Error("Unable to inspect OpenID link", mlog.Err(err))
		c.Err = model.NewAppError("UnmergeOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError)
		return
	}
	if !fastLink && c.App.Config().ElasticsearchSettings.EnableIndexing != nil && *c.App.Config().ElasticsearchSettings.EnableIndexing {
		c.Err = model.NewAppError("UnmergeOpenIDAccount", "app.user.update_auth_data.email_exists.app_error", nil,
			"OpenID unmerge requires search reindex support when Elasticsearch indexing is enabled", http.StatusConflict)
		return
	}
	var sourceID string
	var err error
	if fastLink {
		sourceID, err = unlinkOpenIDIdentityDB(ctx, db, c.Params.UserId)
	} else {
		sourceID, err = unmergeAccountsDBWithOptions(ctx, db, c.Params.UserId, request.PreserveNewOnLocal)
	}
	if err != nil {
		var blocked *mergeBlockedError
		if errors.As(err, &blocked) {
			c.Err = model.NewAppError("UnmergeOpenIDAccount", "app.user.update_auth_data.email_exists.app_error", nil, blocked.reason, http.StatusConflict)
			return
		}
		c.Logger.Error("OpenID account unmerge failed", mlog.Err(err))
		c.Err = model.NewAppError("UnmergeOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError)
		return
	}
	if c.App.Config().ClusterSettings.Enable != nil && *c.App.Config().ClusterSettings.Enable {
		if cacheErr := c.App.Srv().Platform().InvalidateAllCaches(); cacheErr != nil {
			c.Logger.Error("Unable to invalidate caches after OpenID unmerge", mlog.Err(cacheErr))
		}
	} else {
		c.App.Srv().Store().User().ClearCaches()
		c.App.Srv().Store().Channel().ClearCaches()
		c.App.Srv().Store().Team().ClearCaches()
		c.App.Srv().Store().Post().ClearCaches()
	}
	c.App.InvalidateCacheForUser(sourceID)
	c.App.InvalidateCacheForUser(c.Params.UserId)
	c.App.Srv().Platform().ClearUserSessionCache(sourceID)
	c.App.Srv().Platform().ClearUserSessionCache(c.Params.UserId)
	auditRec.AddMeta("source_user_id", sourceID)
	auditRec.Success()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"unlinked":true}`))
}

type unmergePair struct{ sourceChannel, targetChannel string }

type unmergeRequest struct {
	PreserveNewOnLocal bool `json:"preserve_new_on_local"`
}

func unmergeAccountsDB(ctx context.Context, db *sql.DB, targetID string) (string, error) {
	return unmergeAccountsDBWithOptions(ctx, db, targetID, false)
}

func unmergeAccountsDBWithOptions(ctx context.Context, db *sql.DB, targetID string, preserveNewOnLocal bool) (string, error) {
	if db == nil {
		return "", errors.New("master database unavailable")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var sourceID string
	var mergeAt int64
	if err = tx.QueryRowContext(ctx, `SELECT sourceid,createat FROM workspace_openid_account_merges WHERE targetid=$1 FOR UPDATE`, targetID).Scan(&sourceID, &mergeAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", &mergeBlockedError{"this account has no active OpenID merge"}
		}
		return "", err
	}
	var authService string
	var authData sql.NullString
	var targetPassword string
	if err = tx.QueryRowContext(ctx, `SELECT authservice,authdata,password FROM users WHERE id=$1 FOR UPDATE`, targetID).Scan(&authService, &authData, &targetPassword); err != nil {
		return "", err
	}
	var sourceService, sourcePassword string
	var sourceAuth sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT authservice,authdata,password FROM users WHERE id=$1 FOR UPDATE`, sourceID).Scan(&sourceService, &sourceAuth, &sourcePassword); err != nil {
		return "", err
	}
	var savedPassword string
	if err = tx.QueryRowContext(ctx, `SELECT password FROM workspace_linked_credentials WHERE userid=$1 FOR UPDATE`, targetID).Scan(&savedPassword); err != nil {
		return "", err
	}
	if authService != model.ServiceOpenid || !authData.Valid || authData.String == "" || savedPassword == "" ||
		(targetPassword != "" && targetPassword != savedPassword) || sourceService != "" || sourceAuth.Valid || sourcePassword != "" {
		return "", &mergeBlockedError{"linked identities or saved local credential have changed"}
	}
	var archiveUsers, archiveTargetUsers, archiveTargetPreferences, archiveTargetNotices int
	if err = tx.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE tablename='users'), count(*) FILTER (WHERE tablename='target_users'),
		count(*) FILTER (WHERE tablename='target_preferences'), count(*) FILTER (WHERE tablename='target_productnoticeviewstate')
		FROM workspace_openid_merge_archive WHERE sourceid=$1 AND targetid=$2`, sourceID, targetID).
		Scan(&archiveUsers, &archiveTargetUsers, &archiveTargetPreferences, &archiveTargetNotices); err != nil {
		return "", err
	}
	if archiveUsers != 1 || archiveTargetUsers != 1 || archiveTargetPreferences == 0 {
		return "", &mergeBlockedError{"complete pre-merge account snapshot is unavailable"}
	}
	var unsupported int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM workspace_openid_merge_archive
		WHERE sourceid=$1 AND targetid=$2 AND tablename IN
		('sidebarchannels','group_channel',
		'direct_reactions','direct_threads','direct_postacknowledgements','direct_translations')`, sourceID, targetID).Scan(&unsupported); err != nil {
		return "", err
	}
	if unsupported != 0 {
		return "", &mergeBlockedError{"this merge includes data that cannot yet be restored exactly"}
	}
	var mismatchedSubject bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_openid_merge_archive
		WHERE sourceid=$1 AND targetid=$2 AND tablename='users' AND rowdata->>'authdata' IS DISTINCT FROM $3)`, sourceID, targetID, authData.String).Scan(&mismatchedSubject); err != nil {
		return "", err
	}
	if mismatchedSubject {
		return "", &mergeBlockedError{"OpenID subject differs from the pre-merge account"}
	}
	rows, err := tx.QueryContext(ctx, `SELECT rowdata->>'id',rowdata->>'name' FROM workspace_openid_merge_archive
		WHERE sourceid=$1 AND targetid=$2 AND tablename='direct_channel_before_merge'`, sourceID, targetID)
	if err != nil {
		return "", err
	}
	byName := map[string]string{}
	var sourceChannels []struct{ id, name string }
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			_ = rows.Close()
			return "", err
		}
		byName[name] = id
		if strings.Contains(name, sourceID) {
			sourceChannels = append(sourceChannels, struct{ id, name string }{id, name})
		}
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return "", err
	}
	_ = rows.Close()
	if len(sourceChannels) == 0 || len(byName) != 2*len(sourceChannels) {
		return "", &mergeBlockedError{"direct channel restoration snapshot is incomplete"}
	}
	var pairs []unmergePair
	for _, source := range sourceChannels {
		targetChannel := byName[strings.Replace(source.name, sourceID, targetID, 1)]
		if targetChannel == "" {
			return "", &mergeBlockedError{"matching local direct channel snapshot is missing"}
		}
		pairs = append(pairs, unmergePair{source.id, targetChannel})
	}
	var newSourcePosts, newSourceFiles, newTargetPosts, newTargetFiles int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM posts WHERE userid=$1 AND createat>$2`, sourceID, mergeAt).Scan(&newSourcePosts); err != nil {
		return "", err
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM fileinfo WHERE creatorid=$1 AND createat>$2`, sourceID, mergeAt).Scan(&newSourceFiles); err != nil {
		return "", err
	}
	if newSourcePosts != 0 || newSourceFiles != 0 {
		return "", &mergeBlockedError{"the separated OpenID account has new posts or files"}
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM posts WHERE userid=$1 AND createat>$2`, targetID, mergeAt).Scan(&newTargetPosts); err != nil {
		return "", err
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM fileinfo WHERE creatorid=$1 AND createat>$2`, targetID, mergeAt).Scan(&newTargetFiles); err != nil {
		return "", err
	}
	if !preserveNewOnLocal && (newTargetPosts != 0 || newTargetFiles != 0) {
		return "", &mergeBlockedError{"new posts or files exist after linking; their original login cannot be reconstructed"}
	}
	var sourceHasNewState bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM preferences WHERE userid=$1 UNION ALL
		SELECT 1 FROM productnoticeviewstate WHERE userid=$1 UNION ALL
		SELECT 1 FROM teammembers WHERE userid=$1 UNION ALL
		SELECT 1 FROM channelmembers WHERE userid=$1 UNION ALL
		SELECT 1 FROM status WHERE userid=$1 UNION ALL
		SELECT 1 FROM usertermsofservice WHERE userid=$1 UNION ALL
		SELECT 1 FROM sidebarcategories WHERE userid=$1 UNION ALL
		SELECT 1 FROM sidebarchannels WHERE userid=$1)`, sourceID).Scan(&sourceHasNewState); err != nil {
		return "", err
	}
	if sourceHasNewState {
		return "", &mergeBlockedError{"the separated OpenID account has new state after linking"}
	}
	var changedSettings bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM preferences p JOIN workspace_openid_merge_archive s
		ON s.sourceid=$1 AND s.targetid=$2 AND s.tablename='preferences'
		AND p.userid=$2 AND p.category=s.rowdata->>'category' AND p.name=s.rowdata->>'name'
		WHERE NOT EXISTS (SELECT 1 FROM workspace_openid_merge_archive t
		WHERE t.sourceid=$1 AND t.targetid=$2 AND t.tablename='target_preferences'
		AND t.rowdata->>'category'=p.category AND t.rowdata->>'name'=p.name
		AND t.rowdata->>'value'=p.value)
		AND p.value IS DISTINCT FROM s.rowdata->>'value')`, sourceID, targetID).Scan(&changedSettings); err != nil {
		return "", err
	}
	if changedSettings {
		return "", &mergeBlockedError{"a merged preference changed after linking"}
	}
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM productnoticeviewstate p JOIN workspace_openid_merge_archive s
		ON s.sourceid=$1 AND s.targetid=$2 AND s.tablename='productnoticeviewstate'
		AND p.userid=$2 AND p.noticeid=s.rowdata->>'noticeid'
		WHERE NOT EXISTS (SELECT 1 FROM workspace_openid_merge_archive t
		WHERE t.sourceid=$1 AND t.targetid=$2 AND t.tablename='target_productnoticeviewstate'
		AND t.rowdata->>'noticeid'=p.noticeid AND t.rowdata->>'viewed'=p.viewed::text
		AND t.rowdata->>'timestamp'=p.timestamp::text)
		AND (p.viewed::text,p.timestamp::text) IS DISTINCT FROM
		(s.rowdata->>'viewed',s.rowdata->>'timestamp'))`, sourceID, targetID).Scan(&changedSettings); err != nil {
		return "", err
	}
	if changedSettings {
		return "", &mergeBlockedError{"a merged product notice changed after linking"}
	}
	for _, pair := range pairs {
		channelRows, lockErr := tx.QueryContext(ctx, `SELECT id FROM channels WHERE id IN ($1,$2) ORDER BY id FOR UPDATE`, pair.sourceChannel, pair.targetChannel)
		if lockErr != nil {
			return "", lockErr
		}
		locked := 0
		for channelRows.Next() {
			locked++
		}
		if err = channelRows.Err(); err != nil {
			_ = channelRows.Close()
			return "", err
		}
		_ = channelRows.Close()
		if locked != 2 {
			return "", &mergeBlockedError{"one direct channel is missing"}
		}
		var changed int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM posts WHERE channelid=$1 AND (createat>$2 OR updateat>$2)`, pair.targetChannel, mergeAt).Scan(&changed); err != nil {
			return "", err
		}
		if changed != 0 {
			return "", &mergeBlockedError{"direct messages changed after linking"}
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM fileinfo WHERE channelid=$1 AND (createat>$2 OR updateat>$2)`, pair.targetChannel, mergeAt).Scan(&changed); err != nil {
			return "", err
		}
		if changed != 0 {
			return "", &mergeBlockedError{"direct message files changed after linking"}
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM reactions WHERE channelid=$1 AND (createat>$2 OR updateat>$2)`, pair.targetChannel, mergeAt).Scan(&changed); err != nil {
			return "", err
		}
		if changed != 0 {
			return "", &mergeBlockedError{"direct message reactions changed after linking"}
		}
		if err = restoreDirectPair(ctx, tx, sourceID, targetID, pair); err != nil {
			return "", err
		}
	}
	if err = restoreSourceOwnership(ctx, tx, sourceID, targetID, mergeAt); err != nil {
		return "", err
	}
	if err = restoreMergedPreferences(ctx, tx, sourceID, targetID); err != nil {
		return "", err
	}
	if err = restoreNamedPreferences(ctx, tx, sourceID, targetID); err != nil {
		return "", err
	}
	for _, pair := range pairs {
		if err = restoreDirectPreferences(ctx, tx, sourceID, targetID, pair); err != nil {
			return "", err
		}
	}
	if err = restoreMergedNotices(ctx, tx, sourceID, targetID); err != nil {
		return "", err
	}
	for _, table := range []string{"status", "usertermsofservice"} {
		if _, err = tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE userid=$1`, sourceID); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+table+` SELECT (jsonb_populate_record(NULL::`+table+`,a.rowdata)).*
			FROM workspace_openid_merge_archive a WHERE a.sourceid=$1 AND a.targetid=$2 AND a.tablename=$3`,
			sourceID, targetID, table); err != nil {
			return "", err
		}
	}
	// Restore both login identities last so a failed data restoration cannot
	// leave either login pointing at a partially moved account.
	_, err = tx.ExecContext(ctx, `UPDATE users u SET authservice='',authdata=NULL,password=$3,
		username=a.rowdata->>'username',email=a.rowdata->>'email',firstname=a.rowdata->>'firstname',
		lastname=a.rowdata->>'lastname',nickname=a.rowdata->>'nickname',position=a.rowdata->>'position',updateat=$4
		FROM workspace_openid_merge_archive a WHERE u.id=$1 AND a.sourceid=$2 AND a.targetid=$1 AND a.tablename='target_users'`,
		targetID, sourceID, savedPassword, model.GetMillis())
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE users u SET authservice=$3,authdata=$4,password='',deleteat=0,
		username=a.rowdata->>'username',email=a.rowdata->>'email',firstname=a.rowdata->>'firstname',
		lastname=a.rowdata->>'lastname',nickname=a.rowdata->>'nickname',position=a.rowdata->>'position',updateat=$5
		FROM workspace_openid_merge_archive a WHERE u.id=$1 AND a.sourceid=$1 AND a.targetid=$2 AND a.tablename='users'`,
		sourceID, targetID, model.ServiceOpenid, authData.String, model.GetMillis())
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE userid IN ($1,$2)`, sourceID, targetID); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM workspace_linked_credentials WHERE userid=$1`, targetID); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS workspace_openid_unmerge_history
		(targetid varchar(26) NOT NULL,sourceid varchar(26) NOT NULL,mergeat bigint NOT NULL,unmergeat bigint NOT NULL)`); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_openid_unmerge_history VALUES ($1,$2,$3,$4)`, targetID, sourceID, mergeAt, model.GetMillis()); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS workspace_openid_unmerge_archive_history
		(LIKE workspace_openid_merge_archive INCLUDING ALL)`); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_openid_unmerge_archive_history
		SELECT * FROM workspace_openid_merge_archive WHERE sourceid=$1 AND targetid=$2`, sourceID, targetID); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM workspace_openid_merge_archive WHERE sourceid=$1 AND targetid=$2`, sourceID, targetID); err != nil {
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

func restoreDirectPair(ctx context.Context, tx *sql.Tx, sourceID, targetID string, pair unmergePair) error {
	var membersArchived, membersCurrent int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM workspace_openid_merge_archive WHERE sourceid=$1 AND targetid=$2
		AND tablename='direct_channel_member_before_merge' AND rowdata->>'channelid' IN ($3,$4)`,
		sourceID, targetID, pair.sourceChannel, pair.targetChannel).Scan(&membersArchived); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM channelmembers WHERE channelid=$1`, pair.targetChannel).Scan(&membersCurrent); err != nil {
		return err
	}
	if membersArchived != 4 || membersCurrent != 2 {
		return &mergeBlockedError{"direct channel membership changed or its snapshot is incomplete"}
	}
	for _, table := range []string{"posts", "fileinfo"} {
		var expected int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM workspace_openid_merge_archive
			WHERE sourceid=$1 AND targetid=$2 AND tablename=$3 AND rowdata->>'channelid'=$4`,
			sourceID, targetID, "direct_"+table, pair.sourceChannel).Scan(&expected); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE `+table+` p SET channelid=a.rowdata->>'channelid'
			FROM workspace_openid_merge_archive a WHERE a.sourceid=$1 AND a.targetid=$2
			AND a.tablename=$3 AND a.rowdata->>'channelid'=$4 AND p.id=a.rowdata->>'id' AND p.channelid=$5`,
			sourceID, targetID, "direct_"+table, pair.sourceChannel, pair.targetChannel)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if int(count) != expected {
			return &mergeBlockedError{"direct message content changed or its snapshot is incomplete"}
		}
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM channelmemberhistory h USING workspace_openid_merge_archive a
		WHERE a.sourceid=$1 AND a.targetid=$2 AND a.tablename='direct_channelmemberhistory'
		AND a.rowdata->>'channelid'=$3 AND h.channelid=$4 AND h.jointime=(a.rowdata->>'jointime')::bigint
		AND h.userid=CASE WHEN a.rowdata->>'userid'=$1 THEN $2 ELSE a.rowdata->>'userid' END`,
		sourceID, targetID, pair.sourceChannel, pair.targetChannel)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO channelmemberhistory
		SELECT (jsonb_populate_record(NULL::channelmemberhistory,a.rowdata)).*
		FROM workspace_openid_merge_archive a WHERE a.sourceid=$1 AND a.targetid=$2
		AND a.tablename='direct_channelmemberhistory' AND a.rowdata->>'channelid'=$3
		ON CONFLICT DO NOTHING`, sourceID, targetID, pair.sourceChannel)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM channelmembers WHERE channelid IN ($1,$2)`, pair.sourceChannel, pair.targetChannel)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO channelmembers SELECT (jsonb_populate_record(NULL::channelmembers,a.rowdata)).*
		FROM workspace_openid_merge_archive a WHERE a.sourceid=$1 AND a.targetid=$2
		AND a.tablename='direct_channel_member_before_merge' AND a.rowdata->>'channelid' IN ($3,$4)`,
		sourceID, targetID, pair.sourceChannel, pair.targetChannel)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE channels c SET
		name=a.rowdata->>'name',displayname=a.rowdata->>'displayname',creatorid=a.rowdata->>'creatorid',
		deleteat=(a.rowdata->>'deleteat')::bigint,updateat=(a.rowdata->>'updateat')::bigint,
		lastpostat=(a.rowdata->>'lastpostat')::bigint,lastrootpostat=(a.rowdata->>'lastrootpostat')::bigint,
		totalmsgcount=(a.rowdata->>'totalmsgcount')::bigint,totalmsgcountroot=(a.rowdata->>'totalmsgcountroot')::bigint
		FROM workspace_openid_merge_archive a WHERE a.sourceid=$1 AND a.targetid=$2
		AND a.tablename='direct_channel_before_merge' AND c.id=a.rowdata->>'id' AND c.id IN ($3,$4)`,
		sourceID, targetID, pair.sourceChannel, pair.targetChannel)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 2 {
		return fmt.Errorf("restore direct channels: rows=%d: %w", count, err)
	}
	return nil
}

func restoreMergedPreferences(ctx context.Context, tx *sql.Tx, sourceID, targetID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM preferences p USING workspace_openid_merge_archive a
		WHERE p.userid=$2 AND a.sourceid=$1 AND a.targetid=$2 AND a.tablename='preferences'
		AND p.category=a.rowdata->>'category' AND p.name=a.rowdata->>'name'`, sourceID, targetID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO preferences SELECT (jsonb_populate_record(NULL::preferences,t.rowdata)).*
		FROM workspace_openid_merge_archive t WHERE t.sourceid=$1 AND t.targetid=$2
		AND t.tablename='target_preferences' AND EXISTS (SELECT 1 FROM workspace_openid_merge_archive s
		WHERE s.sourceid=$1 AND s.targetid=$2 AND s.tablename='preferences'
		AND s.rowdata->>'category'=t.rowdata->>'category' AND s.rowdata->>'name'=t.rowdata->>'name')`, sourceID, targetID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM preferences WHERE userid=$1`, sourceID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO preferences SELECT (jsonb_populate_record(NULL::preferences,a.rowdata)).*
		FROM workspace_openid_merge_archive a WHERE a.sourceid=$1 AND a.targetid=$2 AND a.tablename='preferences'`, sourceID, targetID)
	return err
}

func restoreDirectPreferences(ctx context.Context, tx *sql.Tx, sourceID, targetID string, pair unmergePair) error {
	// A stale write to the old channel ID must not be silently overwritten.
	var changed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM workspace_openid_merge_archive a JOIN preferences p
		ON p.userid=a.rowdata->>'userid' AND p.category=a.rowdata->>'category' AND p.name=$3
		WHERE a.sourceid=$1 AND a.targetid=$2 AND a.tablename='direct_preferences_source'
		AND a.rowdata->>'name'=$3 AND p.value IS DISTINCT FROM a.rowdata->>'value')`,
		sourceID, targetID, pair.sourceChannel).Scan(&changed); err != nil {
		return err
	}
	if changed {
		return &mergeBlockedError{"a direct channel preference changed after linking"}
	}
	// Remove only the target-channel key created by the merge. If its value
	// changed later, keep that newer preference on the local channel.
	if _, err := tx.ExecContext(ctx, `DELETE FROM preferences p USING workspace_openid_merge_archive a
		WHERE a.sourceid=$1 AND a.targetid=$2 AND a.tablename='direct_preferences_source'
		AND a.rowdata->>'name'=$3 AND p.userid=a.rowdata->>'userid'
		AND p.category=a.rowdata->>'category' AND p.name=$4
		AND NOT EXISTS (SELECT 1 FROM workspace_openid_merge_archive t
			WHERE t.sourceid=$1 AND t.targetid=$2 AND t.tablename='direct_preferences_target'
			AND t.rowdata->>'name'=$4 AND t.rowdata->>'userid'=a.rowdata->>'userid'
			AND t.rowdata->>'category'=a.rowdata->>'category')
		AND (p.userid=$1 OR p.value=a.rowdata->>'value')`,
		sourceID, targetID, pair.sourceChannel, pair.targetChannel); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO preferences
		SELECT (jsonb_populate_record(NULL::preferences,a.rowdata)).*
		FROM workspace_openid_merge_archive a
		WHERE a.sourceid=$1 AND a.targetid=$2 AND a.tablename='direct_preferences_source'
		AND a.rowdata->>'name'=$3
		ON CONFLICT (userid,category,name) DO NOTHING`, sourceID, targetID, pair.sourceChannel)
	return err
}

func restoreMergedNotices(ctx context.Context, tx *sql.Tx, sourceID, targetID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM productnoticeviewstate p USING workspace_openid_merge_archive a
		WHERE p.userid=$2 AND a.sourceid=$1 AND a.targetid=$2 AND a.tablename='productnoticeviewstate'
		AND p.noticeid=a.rowdata->>'noticeid'`, sourceID, targetID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO productnoticeviewstate SELECT (jsonb_populate_record(NULL::productnoticeviewstate,t.rowdata)).*
		FROM workspace_openid_merge_archive t WHERE t.sourceid=$1 AND t.targetid=$2
		AND t.tablename='target_productnoticeviewstate' AND EXISTS (SELECT 1 FROM workspace_openid_merge_archive s
		WHERE s.sourceid=$1 AND s.targetid=$2 AND s.tablename='productnoticeviewstate'
		AND s.rowdata->>'noticeid'=t.rowdata->>'noticeid')`, sourceID, targetID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM productnoticeviewstate WHERE userid=$1`, sourceID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO productnoticeviewstate SELECT (jsonb_populate_record(NULL::productnoticeviewstate,a.rowdata)).*
		FROM workspace_openid_merge_archive a WHERE a.sourceid=$1 AND a.targetid=$2 AND a.tablename='productnoticeviewstate'`, sourceID, targetID)
	return err
}
