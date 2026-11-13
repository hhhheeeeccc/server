package preferences

import (
	"encoding/json"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const maxUpdatePreferences = 100

type Service struct {
	app *app.App
}

func Register(a *app.App) {
	service := &Service{app: a}
	handler := service.updatePreferences

	a.Srv().Router.Handle(model.APIURLSuffix+"/users/{user_id:[A-Za-z0-9]+}/preferences", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodPut)
}

func (s *Service) updatePreferences(c *web.Context, w http.ResponseWriter, r *http.Request) {
	c.RequireUserId()
	if c.Err != nil {
		return
	}

	auditRec := c.MakeAuditRecord(model.AuditEventUpdatePreferences, model.AuditStatusFail)
	defer c.LogAuditRec(auditRec)

	if !c.App.SessionHasPermissionToUser(c.AppContext, *c.AppContext.Session(), c.Params.UserId) {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}

	var prefs model.Preferences
	if err := model.StructFromJSONLimited(r.Body, &prefs); err != nil {
		c.SetInvalidParamWithErr("preferences", err)
		return
	}
	if len(prefs) == 0 || len(prefs) > maxUpdatePreferences {
		c.SetInvalidParam("preferences")
		return
	}

	containsLegacyValue := false
	channelMap := make(map[string]*model.Channel)
	for i := range prefs {
		pref := &prefs[i]
		if pref.UserId != c.Params.UserId {
			c.Err = model.NewAppError("UpdatePreferences", "api.preference.update_preferences.set.app_error", nil,
				"userId="+c.Params.UserId+", preference.UserId="+pref.UserId, http.StatusForbidden)
			return
		}

		if pref.Category == model.PreferenceCategoryFlaggedPost {
			post, appErr := c.App.GetSinglePost(c.AppContext, pref.Name, false)
			if appErr != nil {
				c.SetInvalidParam("preference.name")
				return
			}
			channel, ok := channelMap[post.ChannelId]
			if !ok {
				channel, appErr = c.App.GetChannel(c.AppContext, post.ChannelId)
				if appErr != nil {
					c.Err = appErr
					return
				}
				channelMap[post.ChannelId] = channel
			}
			if ok, _ := c.App.SessionHasPermissionToReadChannel(c.AppContext, *c.AppContext.Session(), channel); !ok {
				c.SetPermissionError(model.PermissionReadChannelContent)
				return
			}
		}

		if isLegacyVisibleDMsGMsValue(pref) {
			containsLegacyValue = true
			validationCopy := *pref
			validationCopy.Value = "1"
			if appErr := validationCopy.IsValid(); appErr != nil {
				c.Err = appErr
				return
			}
			continue
		}
		if appErr := pref.IsValid(); appErr != nil {
			c.Err = appErr
			return
		}
	}

	if !containsLegacyValue {
		if appErr := c.App.UpdatePreferences(c.AppContext, c.Params.UserId, prefs); appErr != nil {
			c.Err = appErr
			return
		}
		auditRec.Success()
		web.ReturnStatusOK(w)
		return
	}

	if s.app.License() == nil {
		c.Err = model.NewAppError("UpdatePreferences", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}

	if appErr := s.saveLegacyCompatiblePreferences(r, prefs); appErr != nil {
		c.Err = appErr
		return
	}
	if err := c.App.Srv().Store().Channel().UpdateSidebarChannelsByPreferences(prefs); err != nil {
		c.Err = model.NewAppError("UpdatePreferences", "api.preference.update_preferences.update_sidebar.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		return
	}

	c.App.Publish(model.NewWebSocketEvent(model.WebsocketEventSidebarCategoryUpdated, "", "", c.Params.UserId, nil, ""))
	message := model.NewWebSocketEvent(model.WebsocketEventPreferencesChanged, "", "", c.Params.UserId, nil, "")
	prefsJSON, err := json.Marshal(prefs)
	if err != nil {
		c.Err = model.NewAppError("UpdatePreferences", "api.marshal_error", nil, "", http.StatusInternalServerError).Wrap(err)
		return
	}
	message.Add("preferences", string(prefsJSON))
	c.App.Publish(message)
	pluginContext := &plugin.Context{
		RequestId:      c.AppContext.RequestId(),
		SessionId:      c.AppContext.Session().Id,
		IPAddress:      c.AppContext.IPAddress(),
		AcceptLanguage: c.AppContext.AcceptLanguage(),
		UserAgent:      c.AppContext.UserAgent(),
		ConnectionId:   c.AppContext.ConnectionId(),
	}
	app := c.App
	app.Srv().Go(func() {
		app.Channels().RunMultiHook(func(hooks plugin.Hooks, _ *model.Manifest) bool {
			hooks.PreferencesHaveChanged(pluginContext, prefs)
			return true
		}, plugin.PreferencesHaveChangedID)
	})

	auditRec.Success()
	web.ReturnStatusOK(w)
}

func isLegacyVisibleDMsGMsValue(pref *model.Preference) bool {
	return pref.Category == model.PreferenceCategorySidebarSettings &&
		pref.Name == model.PreferenceLimitVisibleDmsGms &&
		(pref.Value == "0" || pref.Value == "10000")
}

func (s *Service) saveLegacyCompatiblePreferences(r *http.Request, prefs model.Preferences) *model.AppError {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return model.NewAppError("UpdatePreferences", "app.preference.save.updating.app_error", nil,
			"master database connection is unavailable", http.StatusInternalServerError)
	}

	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		return model.NewAppError("UpdatePreferences", "app.preference.save.updating.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	defer tx.Rollback()

	for i := range prefs {
		prefs[i].PreUpdate()
		if _, err = tx.ExecContext(r.Context(), `
			INSERT INTO preferences (userid, category, name, value)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (userid, category, name)
			DO UPDATE SET value = EXCLUDED.value
		`, prefs[i].UserId, prefs[i].Category, prefs[i].Name, prefs[i].Value); err != nil {
			return model.NewAppError("UpdatePreferences", "app.preference.save.updating.app_error", nil, "", http.StatusBadRequest).Wrap(err)
		}
	}

	if err = tx.Commit(); err != nil {
		return model.NewAppError("UpdatePreferences", "app.preference.save.updating.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	return nil
}
