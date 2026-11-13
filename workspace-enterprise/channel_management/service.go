package channel_management

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const (
	maxAutocompleteResults = 200
	maxBulkRenameBodyBytes = 4 << 10
)

type Service struct {
	app *app.App
}

type bulkRenameRequest struct {
	OldName        string `json:"old_name"`
	NewDisplayName string `json:"new_display_name"`
}

func (r *bulkRenameRequest) Auditable() map[string]any {
	return map[string]any{"old_name": r.OldName, "new_display_name": r.NewDisplayName}
}

type channelName struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

func Register(a *app.App) {
	service := &Service{app: a}
	bulkRename := service.bulkRename
	autocompleteNames := service.autocompleteNames
	countByName := service.countByName

	a.Srv().Router.Handle(model.APIURLSuffix+"/channels/bulk_rename", &web.Handler{
		Srv: a.Srv(), HandleFunc: bulkRename, HandlerName: web.GetHandlerName(bulkRename), RequireSession: true, RequireMfa: true,
	}).Methods(http.MethodPost)
	a.Srv().Router.Handle(model.APIURLSuffix+"/channels/autocomplete_names", &web.Handler{
		Srv: a.Srv(), HandleFunc: autocompleteNames, HandlerName: web.GetHandlerName(autocompleteNames), RequireSession: true, RequireMfa: true,
	}).Methods(http.MethodGet)
	a.Srv().Router.Handle(model.APIURLSuffix+"/channels/count_by_name", &web.Handler{
		Srv: a.Srv(), HandleFunc: countByName, HandlerName: web.GetHandlerName(countByName), RequireSession: true, RequireMfa: true,
	}).Methods(http.MethodGet)
}

func (s *Service) requireLicense(c *web.Context, where string) bool {
	if s.app.License() != nil {
		return true
	}
	c.Err = model.NewAppError(where, "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
	return false
}

func (s *Service) bulkRename(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !s.requireLicense(c, "BulkRenameChannels") {
		return
	}
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionSysconsoleWriteUserManagementChannels) {
		c.SetPermissionError(model.PermissionSysconsoleWriteUserManagementChannels)
		return
	}

	var body bulkRenameRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBulkRenameBodyBytes))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&body)
	if err == nil {
		var trailing any
		if trailingErr := decoder.Decode(&trailing); trailingErr != io.EOF {
			err = trailingErr
			if err == nil {
				err = errors.New("unexpected data after bulk rename request")
			}
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("BulkRenameChannels", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("bulk_rename", err)
		return
	}
	body.OldName = strings.TrimSpace(body.OldName)
	body.NewDisplayName = strings.TrimSpace(body.NewDisplayName)
	if body.OldName == "" {
		c.SetInvalidParam("old_name")
		return
	}
	if body.NewDisplayName == "" || utf8.RuneCountInString(body.NewDisplayName) > model.ChannelDisplayNameMaxRunes {
		c.SetInvalidParam("new_display_name")
		return
	}

	auditRecord := c.MakeAuditRecord("bulkRenameChannels", model.AuditStatusFail)
	defer c.LogAuditRec(auditRecord)
	model.AddEventParameterAuditableToAuditRec(auditRecord, "bulk_rename", &body)

	db := s.app.Srv().Store().GetInternalMasterDB()
	transaction, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		c.Err = model.NewAppError("BulkRenameChannels", model.NoTranslation, nil, "begin transaction", http.StatusInternalServerError).Wrap(err)
		return
	}
	defer transaction.Rollback()

	rows, err := transaction.QueryContext(r.Context(), `
		UPDATE channels
		SET displayname = $1, updateat = $2
		WHERE name = $3 AND type = $4 AND deleteat = 0
		RETURNING id
	`, body.NewDisplayName, model.GetMillis(), body.OldName, string(model.ChannelTypeOpen))
	if err != nil {
		c.Err = model.NewAppError("BulkRenameChannels", "app.channel.bulk_rename.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		return
	}
	channelIDs := make([]string, 0)
	for rows.Next() {
		var channelID string
		if err := rows.Scan(&channelID); err != nil {
			rows.Close()
			c.Err = model.NewAppError("BulkRenameChannels", "app.channel.bulk_rename.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
			return
		}
		channelIDs = append(channelIDs, channelID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		c.Err = model.NewAppError("BulkRenameChannels", "app.channel.bulk_rename.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		return
	}
	if err := rows.Close(); err != nil {
		c.Err = model.NewAppError("BulkRenameChannels", "app.channel.bulk_rename.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		return
	}
	if _, err := transaction.ExecContext(r.Context(), `
		UPDATE publicchannels SET displayname = $1
		WHERE name = $2 AND deleteat = 0
	`, body.NewDisplayName, body.OldName); err != nil {
		c.Err = model.NewAppError("BulkRenameChannels", "app.channel.bulk_rename.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		return
	}
	if err := transaction.Commit(); err != nil {
		c.Err = model.NewAppError("BulkRenameChannels", "app.channel.bulk_rename.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		return
	}

	s.app.Srv().Store().Channel().ClearCaches()
	for _, channelID := range channelIDs {
		channel, getErr := s.app.GetChannel(c.AppContext, channelID)
		if getErr != nil {
			c.Logger.Warn("Unable to publish bulk-renamed channel", mlog.String("channel_id", channelID), mlog.Err(getErr))
			continue
		}
		channelJSON, marshalErr := json.Marshal(channel)
		if marshalErr != nil {
			c.Logger.Warn("Unable to encode bulk-renamed channel", mlog.String("channel_id", channelID), mlog.Err(marshalErr))
			continue
		}
		event := model.NewWebSocketEvent(model.WebsocketEventChannelUpdated, "", channelID, "", nil, "")
		event.Add("channel", string(channelJSON))
		s.app.Publish(event)
	}

	auditRecord.Success()
	if err := json.NewEncoder(w).Encode(map[string]int64{"count": int64(len(channelIDs))}); err != nil {
		c.Logger.Warn("Error writing renamed-channel count", mlog.Err(err))
	}
}

func (s *Service) autocompleteNames(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !s.requireLicense(c, "AutocompleteChannelNames") {
		return
	}
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionSysconsoleReadUserManagementChannels) {
		c.SetPermissionError(model.PermissionSysconsoleReadUserManagementChannels)
		return
	}

	term := "%" + strings.ReplaceAll(strings.ToLower(strings.TrimSpace(r.URL.Query().Get("term"))), " ", "%") + "%"
	rows, err := s.app.Srv().Store().GetInternalReplicaDB().QueryContext(r.Context(), `
		SELECT name, displayname
		FROM channels
		WHERE deleteat = 0 AND type = $1
		  AND (LOWER(name) LIKE $2 OR LOWER(displayname) LIKE $2)
		ORDER BY displayname
		LIMIT $3
	`, string(model.ChannelTypeOpen), term, maxAutocompleteResults)
	if err != nil {
		c.Err = model.NewAppError("AutocompleteChannelNames", "app.channel.get_public_channels_names.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		return
	}
	defer rows.Close()
	names := make([]channelName, 0)
	for rows.Next() {
		var name channelName
		if err := rows.Scan(&name.Name, &name.DisplayName); err != nil {
			c.Err = model.NewAppError("AutocompleteChannelNames", "app.channel.get_public_channels_names.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
			return
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		c.Err = model.NewAppError("AutocompleteChannelNames", "app.channel.get_public_channels_names.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		return
	}
	if err := json.NewEncoder(w).Encode(names); err != nil {
		c.Logger.Warn("Error writing channel names", mlog.Err(err))
	}
}

func (s *Service) countByName(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !s.requireLicense(c, "GetChannelCountByName") {
		return
	}
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionSysconsoleReadUserManagementChannels) {
		c.SetPermissionError(model.PermissionSysconsoleReadUserManagementChannels)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		c.SetInvalidParam("name")
		return
	}
	var count int64
	if err := s.app.Srv().Store().GetInternalReplicaDB().QueryRowContext(r.Context(), `
		SELECT COUNT(*) FROM channels WHERE name = $1 AND type = $2 AND deleteat = 0
	`, name, string(model.ChannelTypeOpen)).Scan(&count); err != nil {
		c.Err = model.NewAppError("GetChannelCountByName", "app.channel.get_count_by_name.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]int64{"count": count}); err != nil {
		c.Logger.Warn("Error writing channel count", mlog.Err(err))
	}
}
