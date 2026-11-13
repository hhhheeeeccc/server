package online_users

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const recentActivityWindow = 5 * time.Minute

type Service struct {
	app *app.App
}

func Register(a *app.App) {
	service := &Service{app: a}
	usersHandler := service.getUsers
	countHandler := service.getCount

	a.Srv().Router.Handle(model.APIURLSuffix+"/reports/users", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     usersHandler,
		HandlerName:    web.GetHandlerName(usersHandler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodGet).Queries("show_online_only", "true")

	a.Srv().Router.Handle(model.APIURLSuffix+"/reports/users/count", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     countHandler,
		HandlerName:    web.GetHandlerName(countHandler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodGet).Queries("show_online_only", "true")
}

func (s *Service) getUsers(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionSysconsoleReadUserManagementUsers) {
		c.SetPermissionError(model.PermissionSysconsoleReadUserManagementUsers)
		return
	}
	if s.app.License() == nil {
		c.Err = model.NewAppError("GetOnlineUsersForReporting", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	options, appErr := userReportOptions(r.URL.Query(), true)
	if appErr != nil {
		c.Err = appErr
		return
	}
	requestedPageSize := options.PageSize
	if requestedPageSize <= 0 || requestedPageSize > model.ReportingMaxPageSize {
		c.Err = model.NewAppError("GetOnlineUsersForReporting", "api.getUsersForReporting.invalid_page_size", nil, "", http.StatusBadRequest)
		return
	}

	filtered, appErr := s.onlinePage(r.Context(), c, options, requestedPageSize, model.GetMillis()-recentActivityWindow.Milliseconds())
	if appErr != nil {
		c.Err = appErr
		return
	}

	if err := json.NewEncoder(w).Encode(filtered); err != nil {
		c.Logger.Warn("Error writing online-user report", mlog.Err(err))
	}
}

// onlinePage applies the online predicate before the requested page size.
// reportPage reverses a previous-page batch back into display order, so earlier batches are prepended
// and the records nearest to the original cursor are retained.
func (s *Service) onlinePage(ctx context.Context, c *web.Context, options *model.UserReportOptions, requestedPageSize int, cutoff int64) ([]*model.UserReport, *model.AppError) {
	options.PageSize = model.ReportingMaxPageSize
	filtered := make([]*model.UserReport, 0, requestedPageSize)

	for {
		reports, appErr := s.reportPage(ctx, options)
		if appErr != nil {
			return nil, appErr
		}
		if len(reports) == 0 {
			break
		}
		onlineIDs, appErr := s.onlineIDsForReports(ctx, reports, cutoff)
		if appErr != nil {
			return nil, appErr
		}

		batch := make([]*model.UserReport, 0, len(reports))
		for _, report := range reports {
			if _, online := onlineIDs[report.Id]; online {
				batch = append(batch, report)
			}
		}

		if options.Direction == "prev" {
			filtered = append(batch, filtered...)
			if len(filtered) >= requestedPageSize {
				return filtered[len(filtered)-requestedPageSize:], nil
			}
		} else {
			filtered = append(filtered, batch...)
			if len(filtered) >= requestedPageSize {
				return filtered[:requestedPageSize], nil
			}
		}

		if len(reports) < model.ReportingMaxPageSize {
			break
		}

		cursor := reports[len(reports)-1]
		if options.Direction == "prev" {
			cursor = reports[0]
		}
		fromColumnValue := reportSortValue(cursor, options.SortColumn)
		if cursor.Id == "" ||
			(cursor.Id == options.FromId && fromColumnValue == options.FromColumnValue) {
			break
		}
		options.FromId = cursor.Id
		options.FromColumnValue = fromColumnValue
	}

	return filtered, nil
}

func reportSortValue(report *model.UserReport, sortColumn string) string {
	switch sortColumn {
	case "CreateAt":
		return strconv.FormatInt(report.CreateAt, 10)
	case "FirstName":
		return report.FirstName
	case "LastName":
		return report.LastName
	case "Nickname":
		return report.Nickname
	case "Email":
		return report.Email
	case "Roles":
		return report.Roles
	default:
		return report.Username
	}
}

func (s *Service) getCount(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionSysconsoleReadUserManagementUsers) {
		c.SetPermissionError(model.PermissionSysconsoleReadUserManagementUsers)
		return
	}
	if s.app.License() == nil {
		c.Err = model.NewAppError("GetOnlineUserCountForReporting", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	options, appErr := userReportOptions(r.URL.Query(), false)
	if appErr != nil {
		c.Err = appErr
		return
	}
	// The core report API has no online predicate. Page through its filtered
	// report instead of materializing every detailed user report at once.
	count, appErr := s.countOnlineReports(r.Context(), options, model.ReportingMaxPageSize, model.GetMillis()-recentActivityWindow.Milliseconds())
	if appErr != nil {
		c.Err = appErr
		return
	}
	if err := json.NewEncoder(w).Encode(count); err != nil {
		c.Logger.Warn("Error writing online-user count", mlog.Err(err))
	}
}

func (s *Service) countOnlineReports(ctx context.Context, options *model.UserReportOptions, batchSize int, cutoff int64) (int64, *model.AppError) {
	options.ReportingBaseOptions = model.ReportingBaseOptions{SortColumn: "Username", Direction: "next", PageSize: batchSize}
	var count int64
	for {
		reports, reportErr := s.reportPage(ctx, options)
		if reportErr != nil {
			return 0, reportErr
		}
		onlineIDs, appErr := s.onlineIDsForReports(ctx, reports, cutoff)
		if appErr != nil {
			return 0, appErr
		}
		for _, report := range reports {
			if _, online := onlineIDs[report.Id]; online {
				count++
			}
		}
		if len(reports) < options.PageSize {
			break
		}
		last := reports[len(reports)-1]
		if last.Id == options.FromId && last.Username == options.FromColumnValue {
			return 0, model.NewAppError("GetOnlineUserCountForReporting", "app.report.get_user_report.store_error", nil, "report cursor made no progress", http.StatusInternalServerError)
		}
		options.FromId = last.Id
		options.FromColumnValue = last.Username
	}
	return count, nil
}

func userReportOptions(values url.Values, includePaging bool) (*model.UserReportOptions, *model.AppError) {
	teamFilter := values.Get("team_filter")
	if teamFilter != "" && !model.IsValidId(teamFilter) {
		return nil, model.NewAppError("GetOnlineUsersForReporting", "api.getUsersForReporting.invalid_team_filter", nil, "", http.StatusBadRequest)
	}

	hideActive := values.Get("hide_active") == "true"
	hideInactive := values.Get("hide_inactive") == "true"
	if hideActive && hideInactive {
		return nil, model.NewAppError("GetOnlineUsersForReporting", "api.getUsersForReporting.invalid_active_filter", nil, "", http.StatusBadRequest)
	}

	options := &model.UserReportOptions{
		Team:         teamFilter,
		Role:         values.Get("role_filter"),
		HasNoTeam:    values.Get("has_no_team") == "true",
		HideActive:   hideActive,
		HideInactive: hideInactive,
		SearchTerm:   values.Get("search_term"),
		GuestFilter:  values.Get("guest_filter"),
	}
	if !includePaging {
		return options, nil
	}

	pageSize := 50
	if parsed, err := strconv.ParseInt(values.Get("page_size"), 10, 64); err == nil {
		pageSize = int(parsed)
	}
	direction := "next"
	if values.Get("direction") == "prev" {
		direction = "prev"
	}
	sortColumn := values.Get("sort_column")
	if sortColumn == "" {
		sortColumn = "Username"
	}
	options.ReportingBaseOptions = model.ReportingBaseOptions{
		SortDesc:        values.Get("sort_direction") == "desc",
		Direction:       direction,
		PageSize:        pageSize,
		SortColumn:      sortColumn,
		FromColumnValue: values.Get("from_column_value"),
		FromId:          values.Get("from_id"),
		DateRange:       values.Get("date_range"),
	}
	options.PopulateDateRange(time.Now())
	return options, nil
}

func (s *Service) onlineIDsForReports(ctx context.Context, reports []*model.UserReport, cutoff int64) (map[string]struct{}, *model.AppError) {
	ids := make([]string, 0, len(reports))
	for _, report := range reports {
		ids = append(ids, report.Id)
	}
	online := make(map[string]struct{}, len(ids))
	if len(ids) == 0 {
		return online, nil
	}
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return nil, model.NewAppError("GetOnlineUsersForReporting", "app.report.get_user_report.store_error", nil,
			"master database connection is unavailable", http.StatusInternalServerError)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT userid
		FROM status
		WHERE userid = ANY($1) AND (lastactivityat >= $2 OR status = $3)
	`, pq.Array(ids), cutoff, model.StatusOnline)
	if err != nil {
		return nil, model.NewAppError("GetOnlineUsersForReporting", "app.report.get_user_report.store_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, model.NewAppError("GetOnlineUsersForReporting", "app.report.get_user_report.store_error", nil, "", http.StatusInternalServerError).Wrap(err)
		}
		online[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, model.NewAppError("GetOnlineUsersForReporting", "app.report.get_user_report.store_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	return online, nil
}
