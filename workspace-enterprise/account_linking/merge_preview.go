package account_linking

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

type mergeReference struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Rows   int64  `json:"rows"`
	Kind   string `json:"kind"`
}

type mergeConflict struct {
	Table      string   `json:"table"`
	Column     string   `json:"column"`
	KeyColumns []string `json:"key_columns"`
	Rows       int64    `json:"rows"`
}

type mergePreview struct {
	SourceUserID string           `json:"source_user_id"`
	TargetUserID string           `json:"target_user_id"`
	References   []mergeReference `json:"references"`
	Conflicts    []mergeConflict  `json:"conflicts"`
	Complete     bool             `json:"complete"`
}

func (s *Service) registerOpenIDMergePreview() {
	handler := s.previewOpenIDMerge
	s.app.Srv().Router.Handle(model.APIURLSuffix+"/users/{user_id:[A-Za-z0-9]+}/openid_merge_preview", &web.Handler{
		Srv:            s.app.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodGet)
}

func (s *Service) previewOpenIDMerge(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !c.IsSystemAdmin() {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}
	if c.App.License() == nil {
		c.Err = model.NewAppError("PreviewOpenIDMerge", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	c.RequireUserId()
	if c.Err != nil {
		return
	}

	targetID := c.Params.UserId
	sourceID := r.URL.Query().Get("source_user_id")
	if !model.IsValidId(targetID) || !model.IsValidId(sourceID) || targetID == sourceID {
		c.SetInvalidParam("source_user_id")
		return
	}
	target, appErr := c.App.GetUser(c.AppContext, targetID)
	if appErr != nil {
		c.Err = appErr
		return
	}
	source, appErr := c.App.GetUser(c.AppContext, sourceID)
	if appErr != nil {
		c.Err = appErr
		return
	}
	if target.AuthService != "" || target.DeleteAt != 0 || target.IsBot ||
		source.AuthService != model.ServiceOpenid || source.AuthData == nil || source.DeleteAt != 0 || source.IsBot {
		c.SetInvalidParam("source_user_id")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	references, err := s.findUserReferences(ctx, sourceID)
	if err != nil {
		c.Logger.Error("Unable to inspect OpenID account data", mlog.Err(err))
		c.Err = model.NewAppError("PreviewOpenIDMerge", "app.user.get_profiles.app_error", nil, "", http.StatusInternalServerError)
		return
	}
	conflicts, err := s.findUserConflicts(ctx, sourceID, targetID, references)
	if err != nil {
		c.Logger.Error("Unable to inspect OpenID merge conflicts", mlog.Err(err))
		c.Err = model.NewAppError("PreviewOpenIDMerge", "app.user.get_profiles.app_error", nil, "", http.StatusInternalServerError)
		return
	}
	response := mergePreview{SourceUserID: sourceID, TargetUserID: targetID, References: references, Conflicts: conflicts}
	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(response); err != nil {
		c.Logger.Warn("Unable to write OpenID merge preview", mlog.Err(err))
	}
}

func (s *Service) findUserReferences(ctx context.Context, userID string) ([]mergeReference, error) {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return nil, fmt.Errorf("master database is unavailable")
	}
	columns, err := db.QueryContext(ctx, `
		SELECT c.table_name, c.column_name, c.data_type
		FROM information_schema.columns c
		JOIN information_schema.tables t
		  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		WHERE c.table_schema = current_schema()
		  AND t.table_type = 'BASE TABLE'
		  AND (
		    (c.data_type IN ('character varying', 'text', 'character')
		      AND (lower(c.column_name) LIKE '%userid%'
		           OR lower(c.column_name) LIKE '%user_id%'
		           OR lower(c.column_name) IN ('creatorid', 'ownerid', 'senderid', 'recipientid', 'mute_id')))
		    OR (c.data_type IN ('character varying', 'text', 'character', 'json', 'jsonb')
		      AND lower(c.column_name) IN ('props', 'participants'))
		  )
		ORDER BY c.table_name, c.column_name
	`)
	if err != nil {
		return nil, err
	}
	type column struct{ table, name, dataType string }
	var userColumns []column
	for columns.Next() {
		var candidate column
		if err = columns.Scan(&candidate.table, &candidate.name, &candidate.dataType); err != nil {
			_ = columns.Close()
			return nil, err
		}
		userColumns = append(userColumns, candidate)
	}
	if err = columns.Err(); err != nil {
		_ = columns.Close()
		return nil, err
	}
	if err = columns.Close(); err != nil {
		return nil, err
	}

	references := make([]mergeReference, 0)
	for _, candidate := range userColumns {
		kind := "exact"
		match := quoteSQLIdentifier(candidate.name) + " = $1"
		name := strings.ToLower(candidate.name)
		if strings.HasSuffix(name, "userids") || name == "props" || name == "participants" {
			kind = "embedded"
			match = "position($1 in " + quoteSQLIdentifier(candidate.name) + "::text) > 0"
		}
		query := "SELECT count(*) FROM " + quoteSQLIdentifier(candidate.table) + " WHERE " + match
		var count int64
		if err = db.QueryRowContext(ctx, query, userID).Scan(&count); err != nil {
			return nil, fmt.Errorf("inspect %s.%s: %w", candidate.table, candidate.name, err)
		}
		if count > 0 {
			references = append(references, mergeReference{Table: candidate.table, Column: candidate.name, Rows: count, Kind: kind})
		}
	}
	return references, nil
}

func quoteSQLIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func (s *Service) findUserConflicts(ctx context.Context, sourceID, targetID string, references []mergeReference) ([]mergeConflict, error) {
	db := s.app.Srv().Store().GetInternalMasterDB()
	rows, err := db.QueryContext(ctx, `
		SELECT k.table_name, k.column_name
		FROM information_schema.table_constraints t
		JOIN information_schema.key_column_usage k
		  ON k.constraint_catalog = t.constraint_catalog
		 AND k.constraint_schema = t.constraint_schema
		 AND k.constraint_name = t.constraint_name
		WHERE t.table_schema = current_schema() AND t.constraint_type = 'PRIMARY KEY'
		ORDER BY k.table_name, k.ordinal_position
	`)
	if err != nil {
		return nil, err
	}
	primaryKeys := make(map[string][]string)
	for rows.Next() {
		var table, column string
		if err = rows.Scan(&table, &column); err != nil {
			_ = rows.Close()
			return nil, err
		}
		primaryKeys[table] = append(primaryKeys[table], column)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}

	conflicts := make([]mergeConflict, 0)
	for _, reference := range references {
		if reference.Kind != "exact" {
			continue
		}
		primaryKey := primaryKeys[reference.Table]
		inKey := false
		otherColumns := make([]string, 0, len(primaryKey))
		for _, column := range primaryKey {
			if column == reference.Column {
				inKey = true
			} else {
				otherColumns = append(otherColumns, column)
			}
		}
		if !inKey {
			continue
		}
		conditions := []string{"source." + quoteSQLIdentifier(reference.Column) + " = $1", "target." + quoteSQLIdentifier(reference.Column) + " = $2"}
		for _, column := range otherColumns {
			identifier := quoteSQLIdentifier(column)
			conditions = append(conditions, "source."+identifier+" = target."+identifier)
		}
		query := "SELECT count(*) FROM " + quoteSQLIdentifier(reference.Table) + " source CROSS JOIN " + quoteSQLIdentifier(reference.Table) + " target WHERE " + strings.Join(conditions, " AND ")
		var count int64
		if err = db.QueryRowContext(ctx, query, sourceID, targetID).Scan(&count); err != nil {
			return nil, fmt.Errorf("inspect conflict %s.%s: %w", reference.Table, reference.Column, err)
		}
		if count > 0 {
			conflicts = append(conflicts, mergeConflict{Table: reference.Table, Column: reference.Column, KeyColumns: otherColumns, Rows: count})
		}
	}
	return conflicts, nil
}
