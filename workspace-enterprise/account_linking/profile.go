package account_linking

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

type openIDNameUpdate struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// The normal user PATCH rejects OAuth names before reaching the provider. Our
// OpenID provider keeps the stored profile on login, so an administrator can
// safely set the name through this limited Enterprise route.
func (s *Service) registerOpenIDProfile() {
	handler := s.updateOpenIDName
	s.app.Srv().Router.Handle(model.APIURLSuffix+"/users/{user_id:[A-Za-z0-9]+}/openid_name", &web.Handler{
		Srv: s.app.Srv(), HandleFunc: handler, HandlerName: web.GetHandlerName(handler),
		RequireSession: true, RequireMfa: true,
	}).Methods(http.MethodPut)
}

func (s *Service) updateOpenIDName(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !c.IsSystemAdmin() {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}
	if c.App.License() == nil {
		c.Err = model.NewAppError("UpdateOpenIDName", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	c.RequireUserId()
	if c.Err != nil {
		return
	}
	var update openIDNameUpdate
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&update); err != nil {
		c.SetInvalidParamWithErr("name", err)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		c.SetInvalidParam("name")
		return
	}
	update.FirstName = strings.TrimSpace(update.FirstName)
	update.LastName = strings.TrimSpace(update.LastName)
	if len([]rune(update.FirstName)) > model.UserFirstNameMaxRunes || len([]rune(update.LastName)) > model.UserLastNameMaxRunes {
		c.SetInvalidParam("name")
		return
	}
	user, appErr := c.App.GetUser(c.AppContext, c.Params.UserId)
	if appErr != nil {
		c.Err = appErr
		return
	}
	if user.AuthService != model.ServiceOpenid || user.DeleteAt != 0 || user.IsBot {
		c.SetInvalidParam("user_id")
		return
	}
	if user.Password != "" {
		// Older merges left the local hash on Users as well as in the linked
		// credential table. Mattermost rejects every subsequent profile save
		// while an OpenID user has both AuthData and Password. Repair only when
		// the saved credential exactly matches the existing hash.
		db := s.app.Srv().Store().GetInternalMasterDB()
		if db == nil {
			c.Err = model.NewAppError("UpdateOpenIDName", "app.user.update.finding.app_error", nil, "database unavailable", http.StatusInternalServerError)
			return
		}
		if err := clearDuplicatedLinkedPassword(r.Context(), db, user.Id); err != nil {
			c.Logger.Error("Unable to normalize linked OpenID credential", mlog.Err(err))
			c.Err = model.NewAppError("UpdateOpenIDName", "app.user.update.finding.app_error", nil, "linked credential does not match the user password", http.StatusConflict)
			return
		}
		c.App.Srv().Store().User().ClearCaches()
		c.App.InvalidateCacheForUser(user.Id)
		user, appErr = c.App.GetUser(c.AppContext, user.Id)
		if appErr != nil {
			c.Err = appErr
			return
		}
	}
	auditRec := c.MakeAuditRecord(model.AuditEventUpdateUser, model.AuditStatusFail)
	auditRec.AddEventPriorState(user)
	defer c.LogAuditRec(auditRec)
	user.FirstName = update.FirstName
	user.LastName = update.LastName
	updated, appErr := c.App.UpdateUser(c.AppContext, user, true)
	if appErr != nil {
		c.Logger.Error("Unable to update OpenID profile name", mlog.Err(appErr))
		c.Err = appErr
		return
	}
	auditRec.AddEventResultState(updated)
	auditRec.Success()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(updated)
}

func clearDuplicatedLinkedPassword(ctx context.Context, db *sql.DB, userID string) error {
	result, err := db.ExecContext(ctx, `UPDATE users u SET password=''
		FROM workspace_linked_credentials lc WHERE u.id=$1 AND lc.userid=u.id
		AND u.authservice=$2 AND u.authdata IS NOT NULL AND u.deleteat=0
		AND u.password<>'' AND u.password=lc.password`, userID, model.ServiceOpenid)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("linked credential mismatch")
	}
	return nil
}
