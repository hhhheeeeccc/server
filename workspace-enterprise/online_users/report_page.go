package online_users

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/mattermost/mattermost/server/public/model"
	sq "github.com/mattermost/squirrel"
)

// reportPage is local to the licensed online-user report. Its keyset uses the
// native display order (column ASC/DESC, Id ASC) even for empty column values.
// A previous page selects in the reverse of BOTH keys, then restores display
// order. No native route or shared store implementation is replaced.
func (s *Service) reportPage(ctx context.Context, filter *model.UserReportOptions) ([]*model.UserReport, *model.AppError) {
	if appErr := filter.IsValid(); appErr != nil {
		return nil, appErr
	}
	if filter.PageSize <= 0 || filter.PageSize > model.ReportingMaxPageSize {
		return nil, model.NewAppError("GetOnlineUsersForReporting", "api.getUsersForReporting.invalid_page_size", nil, "", http.StatusBadRequest)
	}
	query, args, err := reportPageQuery(filter).PlaceholderFormat(sq.Dollar).ToSql()
	if err != nil {
		return nil, reportPageError(err)
	}
	db := s.app.Srv().Store().GetInternalReplicaDB()
	if db == nil {
		return nil, reportPageError(fmt.Errorf("report database connection is unavailable"))
	}
	// The raw database handle does not add the store's configured query timeout.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(*s.app.Config().SqlSettings.QueryTimeout)*time.Second)
	defer cancel()
	rows := []*model.UserReportQuery{}
	if err = sqlx.NewDb(db, "postgres").SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, reportPageError(err)
	}
	reports := make([]*model.UserReport, len(rows))
	for i, row := range rows {
		reports[i] = row.ToReport() // Preserve native non-profile field clearing.
	}
	return reports, nil
}

func reportPageError(err error) *model.AppError {
	return model.NewAppError("GetOnlineUsersForReporting", "app.report.get_user_report.store_error", nil, "", http.StatusInternalServerError).Wrap(err)
}

func reportPageQuery(filter *model.UserReportOptions) sq.SelectBuilder {
	column := "Users." + filter.SortColumn // Validated by UserReportOptions.IsValid.
	previous := filter.Direction == "prev"
	displayDirection, selectionDirection, idDirection := "ASC", "ASC", "ASC"
	if filter.SortDesc {
		displayDirection = "DESC"
	}
	selectionDirection = displayDirection
	if previous {
		idDirection = "DESC"
		if selectionDirection == "ASC" {
			selectionDirection = "DESC"
		} else {
			selectionDirection = "ASC"
		}
	}
	page := sq.Select("Users.*").From("Users").
		Where("Users.Id NOT IN (SELECT UserId FROM Bots)")
	page = reportFilters(page, filter)
	if filter.FromId != "" {
		columnComparison, idComparison := ">", ">"
		if selectionDirection == "DESC" {
			columnComparison = "<"
		}
		if previous {
			idComparison = "<"
		}
		page = page.Where(fmt.Sprintf("(%s %s ? OR (%s = ? AND Users.Id %s ?))", column, columnComparison, column, idComparison), filter.FromColumnValue, filter.FromColumnValue, filter.FromId)
	}
	page = page.OrderBy(column+" "+selectionDirection, "Users.Id "+idDirection).Limit(uint64(filter.PageSize))

	// Select a bounded page before computing its report aggregates. Explicit
	// columns match the native UserReportQuery; ToReport applies its privacy rule.
	columns := []string{
		"Id", "CreateAt", "UpdateAt", "DeleteAt", "Username", "Password", "AuthData", "AuthService", "Email", "EmailVerified",
		"Nickname", "FirstName", "LastName", "Position", "Roles", "AllowMarketing", "Props", "NotifyProps", "LastPasswordUpdate",
		"LastPictureUpdate", "FailedAttempts", "Locale", "Timezone", "MfaActive", "MfaSecret", "MfaUsedTimestamps", "RemoteId", "LastLogin",
	}
	for i := range columns {
		columns[i] = "Users." + columns[i]
	}
	columns = append(columns,
		"(SELECT MAX(s.LastActivityAt) FROM Status s WHERE s.UserId = Users.Id) AS LastStatusAt",
		"ps.LastPostDate", "COALESCE(ps.DaysActive, 0) AS DaysActive", "ps.TotalPosts",
		"(SELECT COUNT(*) FROM ChannelMembers cm INNER JOIN Channels c ON c.Id = cm.ChannelId AND c.DeleteAt = 0 AND c.Type IN ('O','P') WHERE cm.UserId = Users.Id) AS ChannelCount",
		"COALESCE((SELECT string_agg(t.DisplayName, ', ' ORDER BY t.DisplayName) FROM TeamMembers tm INNER JOIN Teams t ON t.Id = tm.TeamId AND t.DeleteAt = 0 WHERE tm.UserId = Users.Id AND tm.DeleteAt = 0), '') AS Teams",
	)
	// Share the page with the statistics query. A grouped join lets PostgreSQL
	// scan PostStats once when an installation lacks its UserId index, rather
	// than requiring a correlated scan per user.
	stats := sq.Select("ps.UserId", "MAX(ps.LastPostDate) AS LastPostDate", "COUNT(ps.Day) AS DaysActive", "SUM(ps.NumPosts) AS TotalPosts").
		From("PostStats ps").Where("ps.UserId IN (SELECT Id FROM page)").GroupBy("ps.UserId")
	if filter.StartAt > 0 {
		stats = stats.Where(sq.GtOrEq{"ps.Day": time.UnixMilli(filter.StartAt).Format("2006-01-02")})
	}
	if filter.EndAt > 0 {
		stats = stats.Where(sq.Lt{"ps.Day": time.UnixMilli(filter.EndAt).Format("2006-01-02")})
	}
	return sq.Select(columns...).PrefixExpr(sq.ConcatExpr("WITH page AS (", page, ")")).From("page Users").
		JoinClause(sq.ConcatExpr("LEFT JOIN (", stats, ") ps ON ps.UserId = Users.Id")).
		OrderBy(column+" "+displayDirection, "Users.Id ASC")
}

// Keep the native report's role, guest, membership, deletion and search
// semantics. Filter-oracle integration tests compare full sanitized records.
func reportFilters(query sq.SelectBuilder, filter *model.UserReportOptions) sq.SelectBuilder {
	role := filter.Role
	switch filter.GuestFilter {
	case model.GuestFilterAll, model.GuestFilterSingleChannel, model.GuestFilterMultipleChannel:
		role = model.SystemGuestRoleId
		if filter.GuestFilter != model.GuestFilterAll {
			comparison := "= 1"
			if filter.GuestFilter == model.GuestFilterMultipleChannel {
				comparison = "> 1"
			}
			query = query.Where("(SELECT COUNT(*) FROM ChannelMembers cm INNER JOIN Channels c ON c.Id = cm.ChannelId AND c.DeleteAt = 0 AND c.Type IN ('O','P') WHERE cm.UserId = Users.Id) " + comparison)
		}
	}
	if role != "" {
		query = query.Where("Users.Roles LIKE LOWER(?)", "%"+reportSearchTerm(role, "\\")+"%")
	}
	if filter.HasNoTeam {
		query = query.Where("Users.Id NOT IN (SELECT UserId FROM TeamMembers WHERE DeleteAt = 0)")
	} else if filter.Team != "" {
		query = query.Join("TeamMembers tm ON (tm.UserId = Users.Id AND tm.DeleteAt = 0)").Where(sq.Eq{"tm.TeamId": filter.Team})
	}
	if filter.HideActive {
		query = query.Where(sq.Gt{"Users.DeleteAt": 0})
	}
	if filter.HideInactive {
		query = query.Where(sq.Eq{"Users.DeleteAt": 0})
	}
	if strings.TrimSpace(filter.SearchTerm) != "" {
		for _, term := range strings.Fields(reportSearchTerm(filter.SearchTerm, "*")) {
			term = strings.TrimLeft(term, "@")
			parts := sq.Or{}
			for _, field := range []string{"Username", "FirstName", "LastName", "Nickname", "Email"} {
				parts = append(parts, sq.Expr("lower(Users."+field+") LIKE lower(?) escape '*'", "%"+term+"%"))
			}
			parts = append(parts, sq.Eq{"Users.Id": term})
			query = query.Where(parts)
		}
	}
	return query
}

func reportSearchTerm(term, escape string) string {
	term = strings.ReplaceAll(term, escape, "")
	return strings.NewReplacer("%", escape+"%", "_", escape+"_").Replace(term)
}
