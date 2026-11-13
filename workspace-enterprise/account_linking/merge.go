package account_linking

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

// The merge accepts only references for which this package has an explicit
// transfer rule. Installations with other user-owned data get a conflict and
// retain both accounts. In particular, embedded IDs and plugin data cannot be
// silently orphaned by changing the OpenID auth owner.
var transferableReferences = map[string]bool{
	"posts.userid":                  true,
	"fileinfo.creatorid":            true,
	"channels.creatorid":            true,
	"teammembers.userid":            true,
	"channelmembers.userid":         true,
	"channelmemberhistory.userid":   true,
	"preferences.userid":            true,
	"preferences.name":              true,
	"productnoticeviewstate.userid": true,
	"usertermsofservice.userid":     true,
	"sidebarcategories.userid":      true,
	"sidebarchannels.userid":        true,
	"status.userid":                 true,
	"sessions.userid":               true,
	"audits.userid":                 true, // historical audit attribution remains immutable
}

type mergeRequest struct {
	SourceUserID string `json:"source_user_id"`
}

type mergeBlockedError struct{ reason string }

func (e *mergeBlockedError) Error() string { return e.reason }

func (s *Service) registerOpenIDMerge() {
	handler := s.mergeOpenIDAccount
	s.app.Srv().Router.Handle(model.APIURLSuffix+"/users/{user_id:[A-Za-z0-9]+}/openid_merge", &web.Handler{
		Srv: s.app.Srv(), HandleFunc: handler, HandlerName: web.GetHandlerName(handler),
		RequireSession: true, RequireMfa: true,
	}).Methods(http.MethodPost)
	statusHandler := s.getOpenIDMergeStatus
	s.app.Srv().Router.Handle(model.APIURLSuffix+"/users/{user_id:[A-Za-z0-9]+}/openid_merge_status", &web.Handler{
		Srv: s.app.Srv(), HandleFunc: statusHandler, HandlerName: web.GetHandlerName(statusHandler),
		RequireSession: true, RequireMfa: true,
	}).Methods(http.MethodGet)
}

func (s *Service) initMergeMetadata() {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		s.app.Log().Error("Unable to initialize OpenID merge metadata: database is unavailable")
		return
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS workspace_openid_account_merges (
		targetid varchar(26) PRIMARY KEY, sourceid varchar(26) UNIQUE NOT NULL,
		sourceusername text NOT NULL, createat bigint NOT NULL)`)
	if err != nil {
		s.app.Log().Error("Unable to initialize OpenID merge metadata", mlog.Err(err))
	}
}

func (s *Service) getOpenIDMergeStatus(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !c.IsSystemAdmin() {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}
	if c.App.License() == nil {
		c.Err = model.NewAppError("GetOpenIDMergeStatus", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	c.RequireUserId()
	if c.Err != nil {
		return
	}
	var sourceID, sourceUsername, sourceFirstName, sourceLastName string
	var fastLink bool
	err := s.app.Srv().Store().GetInternalMasterDB().QueryRowContext(r.Context(),
		`SELECT m.sourceid,m.sourceusername,COALESCE(source.firstname,''),COALESCE(source.lastname,''),
		 EXISTS(SELECT 1 FROM workspace_openid_fast_links f WHERE f.targetid=m.targetid AND f.sourceid=m.sourceid)
		 FROM workspace_openid_account_merges m
		 JOIN users u ON u.id=m.targetid JOIN workspace_linked_credentials lc ON lc.userid=m.targetid
		 JOIN users source ON source.id=m.sourceid
		 WHERE m.targetid=$1 AND u.authservice=$2 AND u.authdata IS NOT NULL AND u.deleteat=0`,
		c.Params.UserId, model.ServiceOpenid).Scan(&sourceID, &sourceUsername, &sourceFirstName, &sourceLastName, &fastLink)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		c.Logger.Error("Unable to load OpenID merge status", mlog.Err(err))
		c.Err = model.NewAppError("GetOpenIDMergeStatus", "app.user.get.app_error", nil, "", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"linked": err == nil, "source_user_id": sourceID, "source_username": sourceUsername,
		"source_first_name": sourceFirstName, "source_last_name": sourceLastName, "fast_link": fastLink,
	})
}

func (s *Service) mergeOpenIDAccount(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !c.IsSystemAdmin() {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}
	if c.App.License() == nil {
		c.Err = model.NewAppError("MergeOpenIDAccount", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	c.RequireUserId()
	if c.Err != nil {
		return
	}
	var request mergeRequest
	if err := decodeOpenIDActionRequest(w, r, &request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("MergeOpenIDAccount", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("source_user_id", err)
		return
	}
	if !model.IsValidId(request.SourceUserID) || request.SourceUserID == c.Params.UserId {
		c.SetInvalidParam("source_user_id")
		return
	}
	auditRec := c.MakeAuditRecord(model.AuditEventUpdateUserAuth, model.AuditStatusFail)
	auditRec.AddMeta("source_user_id", request.SourceUserID)
	auditRec.AddMeta("target_user_id", c.Params.UserId)
	defer c.LogAuditRec(auditRec)
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.linkOpenIDIdentity(ctx, request.SourceUserID, c.Params.UserId); err != nil {
		var blocked *mergeBlockedError
		if errors.As(err, &blocked) {
			c.Err = model.NewAppError("MergeOpenIDAccount", "app.user.update_auth_data.email_exists.app_error", nil, blocked.reason, http.StatusConflict)
			return
		}
		c.Logger.Error("OpenID account merge failed", mlog.Err(err))
		c.Err = model.NewAppError("MergeOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError)
		return
	}
	// Store and application caches are outside PostgreSQL and must be refreshed
	// after commit; sessions of both identities were removed in the transaction.
	if c.App.Config().ClusterSettings.Enable != nil && *c.App.Config().ClusterSettings.Enable {
		if cacheErr := c.App.Srv().Platform().InvalidateAllCaches(); cacheErr != nil {
			c.Logger.Error("Unable to invalidate caches after OpenID merge", mlog.Err(cacheErr))
		}
	} else {
		c.App.Srv().Store().User().ClearCaches()
		c.App.Srv().Store().Channel().ClearCaches()
		c.App.Srv().Store().Team().ClearCaches()
		c.App.Srv().Store().Post().ClearCaches()
	}
	c.App.InvalidateCacheForUser(request.SourceUserID)
	c.App.InvalidateCacheForUser(c.Params.UserId)
	c.App.Srv().Platform().ClearUserSessionCache(request.SourceUserID)
	c.App.Srv().Platform().ClearUserSessionCache(c.Params.UserId)
	auditRec.Success()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"user_id": c.Params.UserId})
}

func (s *Service) mergeAccounts(ctx context.Context, sourceID, targetID string) error {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return errors.New("master database unavailable")
	}
	return mergeAccountsDB(ctx, db, sourceID, targetID)
}

func mergeAccountsDB(ctx context.Context, db *sql.DB, sourceID, targetID string) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Stable lock order prevents two administrators from interleaving a merge.
	rows, err := tx.QueryContext(ctx, `SELECT id, authservice, authdata, password, username, deleteat FROM users WHERE id IN ($1,$2) ORDER BY id FOR UPDATE`, sourceID, targetID)
	if err != nil {
		return err
	}
	type account struct {
		service  string
		auth     sql.NullString
		password string
		username string
		deleted  int64
	}
	accounts := make(map[string]account, 2)
	for rows.Next() {
		var id string
		var a account
		if err = rows.Scan(&id, &a.service, &a.auth, &a.password, &a.username, &a.deleted); err != nil {
			_ = rows.Close()
			return err
		}
		accounts[id] = a
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	source, sourceOK := accounts[sourceID]
	target, targetOK := accounts[targetID]
	if !sourceOK || !targetOK || source.deleted != 0 || target.deleted != 0 || source.service != model.ServiceOpenid || !source.auth.Valid || source.auth.String == "" || target.service != "" || target.auth.Valid || target.password == "" {
		return &mergeBlockedError{"source must be an active OpenID account and target an active local account with a password"}
	}
	var botCount int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM bots WHERE userid IN ($1,$2)`, sourceID, targetID).Scan(&botCount); err != nil {
		return err
	}
	if botCount != 0 {
		return &mergeBlockedError{"bot accounts cannot be merged"}
	}
	var linkedCount int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM workspace_linked_credentials WHERE userid IN ($1,$2)`, sourceID, targetID).Scan(&linkedCount); err != nil {
		return err
	}
	if linkedCount != 0 {
		return &mergeBlockedError{"an account already has a linked local credential"}
	}
	directRenames, directMerges, err := collectDirectChannelChanges(ctx, tx, sourceID, targetID)
	if err != nil {
		return err
	}
	for _, pair := range directMerges {
		if err = checkChannelMergeReferences(ctx, tx, pair.sourceChannelID); err != nil {
			return err
		}
	}
	groupRenames, err := collectGroupChannelRenames(ctx, tx, sourceID, targetID)
	if err != nil {
		return err
	}
	if err = checkMergeReferences(ctx, tx, sourceID); err != nil {
		return err
	}
	if err = checkMergeConflicts(ctx, tx, sourceID, targetID); err != nil {
		return err
	}
	var differingTerms bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM usertermsofservice s
		JOIN usertermsofservice t ON t.userid=$2 WHERE s.userid=$1
		AND s.termsofserviceid IS DISTINCT FROM t.termsofserviceid)`, sourceID, targetID).Scan(&differingTerms); err != nil {
		return err
	}
	if differingTerms {
		return &mergeBlockedError{"accounts accepted different terms of service"}
	}
	mergeID, now := model.NewId(), model.GetMillis()
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS workspace_openid_merge_archive (
		mergeid varchar(26) NOT NULL, sourceid varchar(26) NOT NULL, targetid varchar(26) NOT NULL,
		tablename text NOT NULL, rowdata jsonb NOT NULL, createat bigint NOT NULL
	)`)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS workspace_openid_account_merges (
		targetid varchar(26) PRIMARY KEY, sourceid varchar(26) UNIQUE NOT NULL,
		sourceusername text NOT NULL, createat bigint NOT NULL)`)
	if err != nil {
		return err
	}
	if err = archiveMergeRow(ctx, tx, mergeID, sourceID, targetID, "users", `SELECT to_jsonb(u) - 'password' - 'mfasecret' FROM users u WHERE id=$6`, now, sourceID); err != nil {
		return err
	}
	if err = archiveMergeRow(ctx, tx, mergeID, sourceID, targetID, "target_users", `SELECT to_jsonb(u) - 'password' - 'mfasecret' FROM users u WHERE id=$6`, now, targetID); err != nil {
		return err
	}
	for _, table := range []string{"preferences", "productnoticeviewstate"} {
		if err = archiveMergeRow(ctx, tx, mergeID, sourceID, targetID, "target_"+table,
			`SELECT to_jsonb(s) FROM `+table+` s WHERE s.userid=$6`, now, targetID); err != nil {
			return err
		}
	}
	for _, table := range []string{"posts", "fileinfo", "channels", "teammembers", "channelmembers", "channelmemberhistory", "preferences", "status"} {
		column := "userid"
		if table == "fileinfo" || table == "channels" {
			column = "creatorid"
		}
		if err = archiveMergeRow(ctx, tx, mergeID, sourceID, targetID, table,
			`SELECT to_jsonb(s) FROM `+table+` s WHERE s.`+column+`=$6`, now, sourceID); err != nil {
			return err
		}
	}
	for _, rename := range directRenames {
		if err = archiveMergeRow(ctx, tx, mergeID, sourceID, targetID, "direct_channel", `SELECT to_jsonb(c) FROM channels c WHERE c.id=$6`, now, rename.id); err != nil {
			return err
		}
	}
	for _, rename := range groupRenames {
		if err = archiveMergeRow(ctx, tx, mergeID, sourceID, targetID, "group_channel", `SELECT to_jsonb(c) FROM channels c WHERE c.id=$6`, now, rename.id); err != nil {
			return err
		}
	}
	if err = mergeSidebars(ctx, tx, mergeID, sourceID, targetID, now); err != nil {
		return err
	}
	for _, pair := range directMerges {
		if err = mergeDuplicateDirectChannel(ctx, tx, mergeID, sourceID, targetID, pair, now); err != nil {
			return err
		}
	}
	for _, rename := range directRenames {
		if _, err = tx.ExecContext(ctx, `UPDATE channels SET name=$2 WHERE id=$1`, rename.id, rename.name); err != nil {
			return err
		}
	}
	for _, rename := range groupRenames {
		if _, err = tx.ExecContext(ctx, `UPDATE channels SET name=$2, displayname=$3 WHERE id=$1`, rename.id, rename.name, rename.displayName); err != nil {
			return err
		}
	}
	for _, pair := range []struct{ table, key string }{{"teammembers", "teamid"}, {"channelmembers", "channelid"}, {"preferences", "category,name"}} {
		condition := "s." + pair.key + "=t." + pair.key
		if pair.table == "preferences" {
			condition = "s.category=t.category AND s.name=t.name"
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM `+pair.table+` s USING `+pair.table+` t WHERE s.userid=$1 AND t.userid=$2 AND `+condition, sourceID, targetID)
		if err != nil {
			return err
		}
	}
	// Product notices are per-user UI state. Preserve the local account's
	// value if both identities have seen the same notice; archive the source.
	if err = archiveMergeRow(ctx, tx, mergeID, sourceID, targetID, "productnoticeviewstate",
		`SELECT to_jsonb(s) FROM productnoticeviewstate s WHERE s.userid=$6`, now, sourceID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM productnoticeviewstate s USING productnoticeviewstate t
		WHERE s.userid=$1 AND t.userid=$2 AND s.noticeid=t.noticeid`, sourceID, targetID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE productnoticeviewstate SET userid=$2 WHERE userid=$1`, sourceID, targetID)
	if err != nil {
		return err
	}
	if err = archiveMergeRow(ctx, tx, mergeID, sourceID, targetID, "usertermsofservice",
		`SELECT to_jsonb(s) FROM usertermsofservice s WHERE s.userid=$6`, now, sourceID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM usertermsofservice WHERE userid=$1
		AND EXISTS(SELECT 1 FROM usertermsofservice WHERE userid=$2)`, sourceID, targetID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE usertermsofservice SET userid=$2 WHERE userid=$1`, sourceID, targetID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM status s USING status t WHERE s.userid=$1 AND t.userid=$2`, sourceID, targetID)
	if err != nil {
		return err
	}
	for _, ref := range []struct{ table, column string }{
		{"posts", "userid"}, {"fileinfo", "creatorid"}, {"channels", "creatorid"},
		{"teammembers", "userid"}, {"channelmembers", "userid"},
		{"channelmemberhistory", "userid"}, {"preferences", "userid"}, {"status", "userid"},
	} {
		_, err = tx.ExecContext(ctx, `UPDATE `+ref.table+` SET `+ref.column+`=$2 WHERE `+ref.column+`=$1`, sourceID, targetID)
		if err != nil {
			return err
		}
	}
	if err = archiveMergeRow(ctx, tx, mergeID, sourceID, targetID, "preferences_name", `SELECT to_jsonb(p) FROM preferences p WHERE name=$6`, now, sourceID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM preferences p USING preferences t WHERE p.name=$1 AND t.name=$2 AND p.userid=t.userid AND p.category=t.category`, sourceID, targetID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE preferences SET name=$2 WHERE name=$1`, sourceID, targetID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE userid IN ($1,$2)`, sourceID, targetID)
	if err != nil {
		return err
	}
	if err = upsertCredential(ctx, tx, targetID, target.password, now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE users SET authservice='', authdata=NULL, password='', deleteat=$2, updateat=$2 WHERE id=$1`, sourceID, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE users SET authservice=$2, authdata=$3, password='', updateat=$4 WHERE id=$1`, targetID, model.ServiceOpenid, source.auth.String, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workspace_openid_account_merges (targetid,sourceid,sourceusername,createat)
		VALUES ($1,$2,$3,$4)`, targetID, sourceID, source.username, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func archiveMergeRow(ctx context.Context, tx *sql.Tx, mergeID, sourceID, targetID, table, rowQuery string, now int64, args ...any) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO workspace_openid_merge_archive (mergeid,sourceid,targetid,tablename,rowdata,createat)
		SELECT $1,$2,$3,$4,original.rowdata,$5 FROM (`+rowQuery+`) original(rowdata)`, append([]any{mergeID, sourceID, targetID, table, now}, args...)...)
	return err
}

func checkMergeConflicts(ctx context.Context, tx *sql.Tx, sourceID, targetID string) error {
	// Refuse overlapping memberships with different privileges. The target's
	// preference and presence rows win; source rows remain in the archive.
	for _, table := range []string{"teammembers", "channelmembers"} {
		key := "teamid"
		extra := " OR s.deleteat IS DISTINCT FROM t.deleteat"
		if table == "channelmembers" {
			key, extra = "channelid", ""
		}
		var mismatches int
		query := `SELECT count(*) FROM ` + table + ` s JOIN ` + table + ` t ON s.` + key + `=t.` + key + ` WHERE s.userid=$1 AND t.userid=$2 AND (s.roles IS DISTINCT FROM t.roles OR s.schemeuser IS DISTINCT FROM t.schemeuser OR s.schemeadmin IS DISTINCT FROM t.schemeadmin OR s.schemeguest IS DISTINCT FROM t.schemeguest` + extra + `)`
		if err := tx.QueryRowContext(ctx, query, sourceID, targetID).Scan(&mismatches); err != nil {
			return err
		}
		if mismatches != 0 {
			return &mergeBlockedError{fmt.Sprintf("%s has conflicting membership privileges", table)}
		}
	}
	var duplicateHistory int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM channelmemberhistory s JOIN channelmemberhistory t ON s.channelid=t.channelid AND s.jointime=t.jointime WHERE s.userid=$1 AND t.userid=$2`, sourceID, targetID).Scan(&duplicateHistory); err != nil {
		return err
	}
	if duplicateHistory != 0 {
		return &mergeBlockedError{"channel membership history has duplicate keys"}
	}
	return nil
}

type directChannelRename struct{ id, name string }
type directChannelMerge struct{ sourceChannelID, targetChannelID, peerID string }

type groupChannelRename struct{ id, name, displayName string }

func collectGroupChannelRenames(ctx context.Context, tx *sql.Tx, sourceID, targetID string) ([]groupChannelRename, error) {
	var targetUsername string
	if err := tx.QueryRowContext(ctx, `SELECT username FROM users WHERE id=$1`, targetID).Scan(&targetUsername); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id FROM channels c JOIN channelmembers cm ON cm.channelid=c.id
		WHERE cm.userid=$1 AND c.type='G' ORDER BY c.id FOR UPDATE OF c`, sourceID)
	if err != nil {
		return nil, err
	}
	var channelIDs []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		channelIDs = append(channelIDs, id)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	var renames []groupChannelRename
	for _, channelID := range channelIDs {
		members, memberErr := tx.QueryContext(ctx, `SELECT u.id,u.username FROM channelmembers cm JOIN users u ON u.id=cm.userid
			WHERE cm.channelid=$1 ORDER BY u.id`, channelID)
		if memberErr != nil {
			return nil, memberErr
		}
		var ids []string
		var users []*model.User
		foundSource, foundTarget := false, false
		for members.Next() {
			var id, username string
			if memberErr = members.Scan(&id, &username); memberErr != nil {
				_ = members.Close()
				return nil, memberErr
			}
			if id == sourceID {
				foundSource = true
				id = targetID
				username = targetUsername
			} else if id == targetID {
				foundTarget = true
			}
			ids = append(ids, id)
			users = append(users, &model.User{Username: username})
		}
		if memberErr = members.Err(); memberErr != nil {
			_ = members.Close()
			return nil, memberErr
		}
		_ = members.Close()
		if !foundSource || foundTarget || len(ids) < 3 {
			return nil, &mergeBlockedError{"group message participants overlap or are incomplete"}
		}
		name := model.GetGroupNameFromUserIds(ids)
		var duplicate bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM channels WHERE type='G' AND name=$1 AND id<>$2)`, name, channelID).Scan(&duplicate); err != nil {
			return nil, err
		}
		if duplicate {
			return nil, &mergeBlockedError{"both accounts have a group channel with the same participants"}
		}
		renames = append(renames, groupChannelRename{channelID, name, model.GetGroupDisplayNameFromUsers(users, true)})
	}
	return renames, nil
}

func collectDirectChannelChanges(ctx context.Context, tx *sql.Tx, sourceID, targetID string) ([]directChannelRename, []directChannelMerge, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.name FROM channels c JOIN channelmembers cm ON cm.channelid=c.id
		WHERE cm.userid=$1 AND c.type='D' ORDER BY c.id FOR UPDATE OF c`, sourceID)
	if err != nil {
		return nil, nil, err
	}
	type channel struct{ id, name string }
	var channels []channel
	for rows.Next() {
		var item channel
		if err = rows.Scan(&item.id, &item.name); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		channels = append(channels, item)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, err
	}
	_ = rows.Close()
	var renames []directChannelRename
	var merges []directChannelMerge
	for _, item := range channels {
		ids := strings.Split(item.name, "__")
		if len(ids) != 2 || (ids[0] != sourceID && ids[1] != sourceID) || !model.IsValidId(ids[0]) || !model.IsValidId(ids[1]) || item.name != model.GetDMNameFromIds(ids[0], ids[1]) {
			return nil, nil, &mergeBlockedError{"direct channel has an unexpected participant name"}
		}
		peer := ids[0]
		if peer == sourceID {
			peer = ids[1]
		}
		newName := model.GetDMNameFromIds(targetID, peer)
		var duplicateID string
		err = tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE type='D' AND name=$1 AND id<>$2 AND deleteat=0`, newName, item.id).Scan(&duplicateID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, err
		}
		if err == nil {
			if peer == targetID {
				return nil, nil, &mergeBlockedError{"merging a direct channel between the two accounts needs a separate rule"}
			}
			merges = append(merges, directChannelMerge{item.id, duplicateID, peer})
			continue
		}
		renames = append(renames, directChannelRename{item.id, newName})
	}
	return renames, merges, nil
}

var transferableChannelReferences = map[string]bool{
	"channels.id":                    true,
	"posts.channelid":                true,
	"fileinfo.channelid":             true,
	"reactions.channelid":            true,
	"threads.channelid":              true,
	"channelmembers.channelid":       true,
	"channelmemberhistory.channelid": true,
	"sidebarchannels.channelid":      true,
	"postacknowledgements.channelid": true,
	"translations.channelid":         true,
	"preferences.name":               true,
}

func checkChannelMergeReferences(ctx context.Context, tx *sql.Tx, sourceChannelID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT table_name,column_name,data_type FROM information_schema.columns
		WHERE table_schema=current_schema() AND data_type IN ('character varying','text','character','json','jsonb','bytea') ORDER BY table_name,column_name`)
	if err != nil {
		return err
	}
	type column struct{ table, name, dataType string }
	var columns []column
	for rows.Next() {
		var item column
		if err = rows.Scan(&item.table, &item.name, &item.dataType); err != nil {
			_ = rows.Close()
			return err
		}
		columns = append(columns, item)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, item := range columns {
		if item.table == "workspace_openid_merge_archive" || item.table == "workspace_openid_unmerge_archive_history" || item.table == "audits" {
			continue
		}
		qualified := strings.ToLower(item.table + "." + item.name)
		exact := transferableChannelReferences[qualified]
		predicate := `position($1 in ` + quoteSQLIdentifier(item.name) + `::text)>0`
		if item.dataType == "bytea" {
			predicate = `position(convert_to($1,'UTF8') in ` + quoteSQLIdentifier(item.name) + `)>0`
		}
		if exact {
			predicate = quoteSQLIdentifier(item.name) + `=$1`
		}
		var found bool
		query := `SELECT EXISTS(SELECT 1 FROM ` + quoteSQLIdentifier(item.table) + ` WHERE ` + predicate + `)`
		if err = tx.QueryRowContext(ctx, query, sourceChannelID).Scan(&found); err != nil {
			return fmt.Errorf("scan channel reference %s: %w", qualified, err)
		}
		if found && !exact {
			return &mergeBlockedError{"unsupported channel reference in " + qualified}
		}
	}
	return nil
}

func mergeDuplicateDirectChannel(ctx context.Context, tx *sql.Tx, mergeID, sourceUserID, targetUserID string, pair directChannelMerge, now int64) error {
	// A direct channel must contain precisely the two participants in its name.
	// A stale or extra membership needs explicit repair before merging history.
	var sourceMembers, targetMembers int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM channelmembers WHERE channelid=$1 AND userid IN ($2,$3)`,
		pair.sourceChannelID, sourceUserID, pair.peerID).Scan(&sourceMembers); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM channelmembers WHERE channelid=$1 AND userid IN ($2,$3)`,
		pair.targetChannelID, targetUserID, pair.peerID).Scan(&targetMembers); err != nil {
		return err
	}
	var sourceTotal, targetTotal int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM channelmembers WHERE channelid=$1`, pair.sourceChannelID).Scan(&sourceTotal); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM channelmembers WHERE channelid=$1`, pair.targetChannelID).Scan(&targetTotal); err != nil {
		return err
	}
	if sourceMembers != 2 || targetMembers != 2 || sourceTotal != 2 || targetTotal != 2 {
		return &mergeBlockedError{"direct channel memberships do not match their participants"}
	}
	for _, channelID := range []string{pair.sourceChannelID, pair.targetChannelID} {
		if err := archiveMergeRow(ctx, tx, mergeID, sourceUserID, targetUserID, "direct_channel_before_merge",
			`SELECT to_jsonb(c) FROM channels c WHERE id=$6`, now, channelID); err != nil {
			return err
		}
		if err := archiveMergeRow(ctx, tx, mergeID, sourceUserID, targetUserID, "direct_channel_member_before_merge",
			`SELECT to_jsonb(cm) FROM channelmembers cm WHERE channelid=$6`, now, channelID); err != nil {
			return err
		}
	}
	for _, item := range []struct{ archive, channelID string }{
		{"direct_preferences_source", pair.sourceChannelID},
		{"direct_preferences_target", pair.targetChannelID},
	} {
		if err := archiveMergeRow(ctx, tx, mergeID, sourceUserID, targetUserID, item.archive,
			`SELECT to_jsonb(p) FROM preferences p WHERE p.name=$6`, now, item.channelID); err != nil {
			return err
		}
	}
	// Preferences such as channel_open_time use the channel ID as their key.
	// Keep the target-channel value on collision and archive the source value.
	if _, err := tx.ExecContext(ctx, `DELETE FROM preferences s USING preferences t
		WHERE s.name=$1 AND t.name=$2 AND s.userid=t.userid AND s.category=t.category`, pair.sourceChannelID, pair.targetChannelID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE preferences SET name=$2 WHERE name=$1`, pair.sourceChannelID, pair.targetChannelID); err != nil {
		return err
	}
	for _, table := range []string{"posts", "fileinfo", "reactions", "threads", "channelmemberhistory", "sidebarchannels", "postacknowledgements", "translations"} {
		if err := archiveMergeRow(ctx, tx, mergeID, sourceUserID, targetUserID, "direct_"+table,
			`SELECT to_jsonb(s) FROM `+table+` s WHERE s.channelid=$6`, now, pair.sourceChannelID); err != nil {
			return err
		}
	}
	for _, table := range []string{"posts", "fileinfo", "reactions", "threads", "postacknowledgements", "translations"} {
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET channelid=$2 WHERE channelid=$1`, pair.sourceChannelID, pair.targetChannelID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sidebarchannels s USING sidebarchannels t WHERE s.channelid=$1 AND t.channelid=$2
		AND s.userid=t.userid AND s.categoryid=t.categoryid`, pair.sourceChannelID, pair.targetChannelID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sidebarchannels SET channelid=$2 WHERE channelid=$1`, pair.sourceChannelID, pair.targetChannelID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO channelmemberhistory(channelid,userid,jointime,leavetime)
		SELECT $2,CASE WHEN userid=$3 THEN $4 ELSE userid END,jointime,leavetime
		FROM channelmemberhistory WHERE channelid=$1 ON CONFLICT DO NOTHING`,
		pair.sourceChannelID, pair.targetChannelID, sourceUserID, targetUserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM channelmemberhistory WHERE channelid=$1`, pair.sourceChannelID); err != nil {
		return err
	}
	// Mattermost message counters are cumulative and may include posts no
	// longer present in the table. Recounting live rows would silently reset
	// unread state for both participants.
	if _, err := tx.ExecContext(ctx, `UPDATE channels t SET
		totalmsgcount=t.totalmsgcount+s.totalmsgcount,
		totalmsgcountroot=t.totalmsgcountroot+s.totalmsgcountroot,
		lastpostat=GREATEST(t.lastpostat,s.lastpostat),
		lastrootpostat=GREATEST(t.lastrootpostat,s.lastrootpostat),
		updateat=$3 FROM channels s WHERE t.id=$2 AND s.id=$1`, pair.sourceChannelID, pair.targetChannelID, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE channelmembers t SET
		msgcount=LEAST(c.totalmsgcount,COALESCE(t.msgcount,0)+COALESCE(s.msgcount,0)),
		msgcountroot=LEAST(c.totalmsgcountroot,COALESCE(t.msgcountroot,0)+COALESCE(s.msgcountroot,0)),
		mentioncount=LEAST(c.totalmsgcount,COALESCE(t.mentioncount,0)+COALESCE(s.mentioncount,0)),
		mentioncountroot=LEAST(c.totalmsgcountroot,COALESCE(t.mentioncountroot,0)+COALESCE(s.mentioncountroot,0)),
		urgentmentioncount=LEAST(c.totalmsgcount,COALESCE(t.urgentmentioncount,0)+COALESCE(s.urgentmentioncount,0)),
		lastviewedat=GREATEST(COALESCE(t.lastviewedat,0),COALESCE(s.lastviewedat,0)),
		lastupdateat=GREATEST(COALESCE(t.lastupdateat,0),COALESCE(s.lastupdateat,0))
		FROM channelmembers s,channels c WHERE t.channelid=$2 AND s.channelid=$1 AND c.id=$2
		AND t.userid=CASE WHEN s.userid=$3 THEN $4 ELSE s.userid END`,
		pair.sourceChannelID, pair.targetChannelID, sourceUserID, targetUserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM channelmembers WHERE channelid=$1`, pair.sourceChannelID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE channels SET deleteat=$2,updateat=$2,totalmsgcount=0,totalmsgcountroot=0 WHERE id=$1`, pair.sourceChannelID, now)
	return err
}

func mergeSidebars(ctx context.Context, tx *sql.Tx, mergeID, sourceID, targetID string, now int64) error {
	for _, table := range []string{"sidebarcategories", "sidebarchannels"} {
		if err := archiveMergeRow(ctx, tx, mergeID, sourceID, targetID, table,
			`SELECT to_jsonb(s) FROM `+table+` s WHERE s.userid=$6`, now, sourceID); err != nil {
			return err
		}
	}
	// When a channel is already in the local account's sidebar, its placement
	// wins. The source placement remains available in the archive.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sidebarchannels s USING sidebarchannels t
		WHERE s.userid=$1 AND t.userid=$2 AND s.channelid=t.channelid`, sourceID, targetID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,teamid,type FROM sidebarcategories WHERE userid=$1 ORDER BY id`, sourceID)
	if err != nil {
		return err
	}
	type category struct{ id, teamID, kind string }
	var categories []category
	for rows.Next() {
		var item category
		if err = rows.Scan(&item.id, &item.teamID, &item.kind); err != nil {
			_ = rows.Close()
			return err
		}
		categories = append(categories, item)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, item := range categories {
		newID := strings.ReplaceAll(item.id, sourceID, targetID)
		matched := false
		if item.kind == "channels" || item.kind == "favorites" || item.kind == "direct_messages" {
			var existingID string
			lookup := tx.QueryRowContext(ctx, `SELECT id FROM sidebarcategories WHERE userid=$1 AND teamid=$2 AND type=$3 LIMIT 1`, targetID, item.teamID, item.kind)
			err = lookup.Scan(&existingID)
			switch {
			case err == nil:
				newID, matched = existingID, true
			case errors.Is(err, sql.ErrNoRows):
			default:
				return err
			}
		}
		var occupied bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sidebarcategories WHERE id=$1 AND userid<>$2)`, newID, sourceID).Scan(&occupied); err != nil {
			return err
		}
		if occupied && !matched {
			return &mergeBlockedError{"sidebar category ID already belongs to another user"}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sidebarchannels SET categoryid=$2,userid=$3 WHERE userid=$1 AND categoryid=$4`, sourceID, newID, targetID, item.id); err != nil {
			return err
		}
		if matched {
			_, err = tx.ExecContext(ctx, `DELETE FROM sidebarcategories WHERE id=$1 AND userid=$2`, item.id, sourceID)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE sidebarcategories SET id=$2,userid=$3 WHERE id=$1 AND userid=$4`, item.id, newID, targetID, sourceID)
		}
		if err != nil {
			return err
		}
	}
	var orphaned int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM sidebarchannels WHERE userid=$1`, sourceID).Scan(&orphaned); err != nil {
		return err
	}
	if orphaned != 0 {
		return &mergeBlockedError{"sidebar channels have no matching category"}
	}
	return nil
}

func checkMergeReferences(ctx context.Context, tx *sql.Tx, sourceID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT table_name,column_name,data_type FROM information_schema.columns
		WHERE table_schema=current_schema() AND data_type IN ('character varying','text','character','json','jsonb','bytea') ORDER BY table_name,column_name`)
	if err != nil {
		return err
	}
	type column struct{ table, name, dataType string }
	var columns []column
	for rows.Next() {
		var c column
		if err = rows.Scan(&c.table, &c.name, &c.dataType); err != nil {
			_ = rows.Close()
			return err
		}
		columns = append(columns, c)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, c := range columns {
		if c.table == "workspace_openid_merge_archive" || c.table == "workspace_openid_unmerge_archive_history" || c.table == "workspace_openid_unmerge_history" {
			continue
		}
		// Audit entries are immutable historical evidence. The source account
		// remains as a tombstone, and its full row is archived with the mapping.
		if c.table == "audits" {
			continue
		}
		qualified := strings.ToLower(c.table + "." + c.name)
		if qualified == "channels.name" {
			var unexpected bool
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM channels c WHERE position($1 in c.name)>0
				AND (c.type<>'D' OR NOT EXISTS(SELECT 1 FROM channelmembers cm WHERE cm.channelid=c.id AND cm.userid=$1)))`, sourceID).Scan(&unexpected)
			if err != nil {
				return err
			}
			if unexpected {
				return &mergeBlockedError{"unsupported user reference in channels.name"}
			}
			continue
		}
		if qualified == "sidebarcategories.id" || qualified == "sidebarchannels.categoryid" {
			var otherOwner bool
			query := `SELECT EXISTS(SELECT 1 FROM ` + quoteSQLIdentifier(c.table) + ` WHERE position($1 in ` + quoteSQLIdentifier(c.name) + `::text)>0 AND userid<>$2)`
			if err = tx.QueryRowContext(ctx, query, sourceID, sourceID).Scan(&otherOwner); err != nil {
				return err
			}
			if otherOwner {
				return &mergeBlockedError{"sidebar reference belongs to another user"}
			}
			continue
		}
		isExact := transferableReferences[qualified]
		// The source user row survives as an inactive audit tombstone and is
		// archived in full. Other users' embedded references still block.
		where := ""
		args := []any{sourceID}
		if c.table == "users" {
			where = ` AND id<>$1`
		}
		predicate := `position($1 in ` + quoteSQLIdentifier(c.name) + `::text)>0`
		if c.dataType == "bytea" {
			predicate = `position(convert_to($1,'UTF8') in ` + quoteSQLIdentifier(c.name) + `)>0`
		}
		if isExact {
			predicate = quoteSQLIdentifier(c.name) + `=$1`
		}
		query := `SELECT EXISTS(SELECT 1 FROM ` + quoteSQLIdentifier(c.table) + ` WHERE ` + predicate + where + `)`
		var found bool
		if err = tx.QueryRowContext(ctx, query, args...).Scan(&found); err != nil {
			return fmt.Errorf("scan %s: %w", qualified, err)
		}
		if found && !isExact {
			return &mergeBlockedError{"unsupported user reference in " + qualified}
		}
	}
	return nil
}
