package account_linking

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

// Service provides the OpenID linking API and read-only merge inspection.
// Linking an identity owned by a second account is rejected until a complete
// data merge can be performed.
type Service struct {
	app *app.App
}

func decodeOpenIDActionRequest(w http.ResponseWriter, r *http.Request, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("OpenID action request must be an object")
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("unexpected data after OpenID action request")
		}
		return err
	}
	return nil
}

func Register(a *app.App) *Service {
	service := &Service{app: a}
	service.initCredentials()
	service.initMergeMetadata()
	service.initFastLinks()
	service.registerOpenIDCandidates()
	service.registerOpenIDMergePreview()
	service.registerOpenIDMerge()
	service.registerOpenIDUnmerge()
	service.registerOpenIDProfile()
	registerRoute(a, false, service.updateUserAuth)
	registerRoute(a, true, service.updateUserAuth)
	service.registerCredentialRoutes(false)
	service.registerCredentialRoutes(true)
	return service
}

func registerRoute(a *app.App, local bool, handler func(*web.Context, http.ResponseWriter, *http.Request)) {
	router := a.Srv().Router
	if local {
		router = a.Srv().LocalRouter
	}
	router.Handle(model.APIURLSuffix+"/users/{user_id:[A-Za-z0-9]+}/auth", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: !local,
		RequireMfa:     !local,
		IsLocal:        local,
	}).Methods(http.MethodPut)
}

func (s *Service) updateUserAuth(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !c.IsSystemAdmin() {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}
	c.RequireUserId()
	if c.Err != nil {
		return
	}
	if c.App.License() == nil {
		c.Err = model.NewAppError("LinkOpenIDAccount", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}

	var userAuth model.UserAuth
	if err := decodeOpenIDActionRequest(w, r, &userAuth); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("LinkOpenIDAccount", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("user", err)
		return
	}
	if !userAuth.IsValid() {
		c.Err = model.NewAppError("LinkOpenIDAccount", "api.user.update_user_auth.invalid_request", nil, "", http.StatusBadRequest)
		return
	}
	if userAuth.AuthService == model.UserAuthServiceEmail {
		userAuth.AuthService = ""
	}

	auditRec := c.MakeAuditRecord(model.AuditEventUpdateUserAuth, model.AuditStatusFail)
	model.AddEventParameterAuditableToAuditRec(auditRec, "user_auth", &userAuth)
	defer c.LogAuditRec(auditRec)

	target, appErr := c.App.GetUser(c.AppContext, c.Params.UserId)
	if appErr != nil {
		c.Err = appErr
		return
	}
	auditRec.AddEventPriorState(target)
	linked, err := s.isAccountInOpenIDLink(r.Context(), target.Id)
	if err != nil {
		c.Logger.Error("Unable to inspect OpenID link before changing authentication", mlog.Err(err))
		c.Err = model.NewAppError("LinkOpenIDAccount", "app.user.update_auth_data.app_error", nil, "", http.StatusInternalServerError)
		return
	}
	if linked {
		c.Err = model.NewAppError("LinkOpenIDAccount", "app.user.update_auth_data.email_exists.app_error", nil,
			"unlink the OpenID accounts before changing their authentication", http.StatusConflict)
		return
	}

	if userAuth.AuthData == nil {
		restored := false
		if target.AuthService == model.ServiceOpenid {
			restored, appErr = s.restoreLocalAuthWithCredential(r.Context(), target.Id)
		}
		if appErr == nil && restored {
			c.App.InvalidateCacheForUser(target.Id)
			appErr = c.App.RevokeAllSessions(c.AppContext, target.Id)
		} else if appErr == nil {
			_, appErr = c.App.UpdateUserAuth(c.AppContext, target.Id, &userAuth)
		}
		if appErr != nil {
			c.Err = appErr
			return
		}
		s.writeSuccess(c, w, auditRec, target.Id, &userAuth)
		return
	}

	owner, err := c.App.Srv().Store().User().GetByAuthData(userAuth.AuthData)
	if err != nil && !store.IsErrNotFound(err) {
		c.Logger.Error("Unable to find the current owner of an authentication identifier", mlog.Err(err))
		c.Err = model.NewAppError("LinkOpenIDAccount", "app.user.get_by_auth.other.app_error", nil, "", http.StatusInternalServerError)
		return
	}
	if store.IsErrNotFound(err) || owner.Id == target.Id {
		if userAuth.AuthService == model.ServiceOpenid && target.Password != "" {
			appErr = s.updateOpenIDAuthWithCredential(r.Context(), target, *userAuth.AuthData)
			if appErr == nil {
				c.App.InvalidateCacheForUser(target.Id)
				appErr = c.App.RevokeAllSessions(c.AppContext, target.Id)
			}
		} else {
			_, appErr = c.App.UpdateUserAuth(c.AppContext, target.Id, &userAuth)
		}
		if appErr != nil {
			c.Err = appErr
			return
		}
		s.writeSuccess(c, w, auditRec, target.Id, &userAuth)
		return
	}
	if owner.AuthService != userAuth.AuthService {
		c.Err = model.NewAppError(
			"LinkOpenIDAccount",
			"app.user.update_auth_data.email_exists.app_error",
			nil,
			fmt.Sprintf("auth_data is already assigned to service=%s user_id=%s", owner.AuthService, owner.Id),
			http.StatusConflict,
		)
		return
	}
	if userAuth.AuthService != model.ServiceOpenid {
		c.Err = model.NewAppError(
			"LinkOpenIDAccount",
			"api.user.update_user_auth.invalid_request",
			nil,
			"automatic account consolidation is limited to OpenID",
			http.StatusBadRequest,
		)
		return
	}
	if target.IsBot || owner.IsBot {
		c.Err = model.NewAppError("LinkOpenIDAccount", "api.user.update_user_auth.invalid_request", nil, "bot accounts cannot be consolidated", http.StatusBadRequest)
		return
	}
	// An existing OpenID account owns posts, memberships, preferences and often
	// plugin data. Moving only its auth identifier strands those records under a
	// disabled user. Keep both accounts intact until the data merge is available.
	c.Err = model.NewAppError(
		"LinkOpenIDAccount",
		"app.user.update_auth_data.email_exists.app_error",
		nil,
		"OpenID identity belongs to another account; full data merge is not available",
		http.StatusConflict,
	)
}

func (s *Service) isAccountInOpenIDLink(ctx context.Context, userID string) (bool, error) {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return false, errors.New("master database unavailable")
	}
	var linked bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM workspace_openid_account_merges WHERE targetid=$1 OR sourceid=$1
	)`, userID).Scan(&linked)
	return linked, err
}

func (s *Service) writeSuccess(c *web.Context, w http.ResponseWriter, auditRec *model.AuditRecord, targetID string, userAuth *model.UserAuth) {
	auditRec.Success()
	auditRec.AddMeta("auth_service", userAuth.AuthService)
	c.LogAudit(fmt.Sprintf("updated user %s auth to service=%s", targetID, userAuth.AuthService))
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(userAuth); err != nil {
		c.Logger.Warn("Error while writing account-link response", mlog.Err(err))
	}
}
