package account_linking

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const candidatePageSize = 50

type openIDCandidate struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	AuthData  string `json:"auth_data"`
}

type openIDCandidatesResponse struct {
	Users   []openIDCandidate `json:"users"`
	HasMore bool              `json:"has_more"`
}

func (s *Service) registerOpenIDCandidates() {
	handler := s.listOpenIDCandidates
	s.app.Srv().Router.Handle(model.APIURLSuffix+"/users/openid_link_candidates", &web.Handler{
		Srv:            s.app.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodGet)
}

func (s *Service) listOpenIDCandidates(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !c.IsSystemAdmin() {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}
	if c.App.License() == nil {
		c.Err = model.NewAppError("ListOpenIDCandidates", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(query) > 100 {
		c.SetInvalidParam("q")
		return
	}
	page := 0
	if raw := r.URL.Query().Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		if err != nil || page < 0 || page > 100000 {
			c.SetInvalidParam("page")
			return
		}
	}

	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		c.Err = model.NewAppError("ListOpenIDCandidates", "app.user.get_profiles.app_error", nil, "database is unavailable", http.StatusInternalServerError)
		return
	}
	rows, err := db.QueryContext(r.Context(), `
		SELECT id, username, COALESCE(email, ''), COALESCE(firstname, ''),
		       COALESCE(lastname, ''), authdata
		FROM users
		WHERE authservice = $1 AND deleteat = 0
		  AND NOT EXISTS (SELECT 1 FROM bots WHERE bots.userid = users.id)
		  AND NOT EXISTS (SELECT 1 FROM workspace_linked_credentials WHERE workspace_linked_credentials.userid = users.id)
		  AND authdata IS NOT NULL AND authdata <> ''
		  AND ($2 = '' OR position(lower($2) in lower(username)) > 0
		       OR position(lower($2) in lower(COALESCE(email, ''))) > 0
		       OR position(lower($2) in lower(COALESCE(firstname, ''))) > 0
		       OR position(lower($2) in lower(COALESCE(lastname, ''))) > 0
		       OR position(lower($2) in lower(authdata)) > 0)
		ORDER BY lower(username), id
		LIMIT $3 OFFSET $4
	`, model.ServiceOpenid, query, candidatePageSize+1, page*candidatePageSize)
	if err != nil {
		c.Logger.Error("Unable to list OpenID link candidates", mlog.Err(err))
		c.Err = model.NewAppError("ListOpenIDCandidates", "app.user.get_profiles.app_error", nil, "", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	response := openIDCandidatesResponse{Users: make([]openIDCandidate, 0, candidatePageSize)}
	for rows.Next() {
		var candidate openIDCandidate
		if err = rows.Scan(&candidate.ID, &candidate.Username, &candidate.Email, &candidate.FirstName, &candidate.LastName, &candidate.AuthData); err != nil {
			c.Logger.Error("Unable to read OpenID link candidate", mlog.Err(err))
			c.Err = model.NewAppError("ListOpenIDCandidates", "app.user.get_profiles.app_error", nil, "", http.StatusInternalServerError)
			return
		}
		if len(response.Users) == candidatePageSize {
			response.HasMore = true
			break
		}
		response.Users = append(response.Users, candidate)
	}
	if err = rows.Err(); err != nil {
		c.Logger.Error("Unable to finish listing OpenID link candidates", mlog.Err(err))
		c.Err = model.NewAppError("ListOpenIDCandidates", "app.user.get_profiles.app_error", nil, "", http.StatusInternalServerError)
		return
	}
	if err = json.NewEncoder(w).Encode(response); err != nil {
		c.Logger.Warn("Unable to write OpenID link candidates", mlog.Err(err))
	}
}
