package online_users

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestEnterpriseReportPageMatchesNativeFiltersAndSanitization(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
	service := &Service{app: th.App}
	prefix := "report_" + model.NewId()
	var users []*model.User
	for i := range 9 {
		roles := model.SystemUserRoleId
		if i == 1 {
			roles += " " + model.SystemAdminRoleId
		} else if i >= 2 && i <= 4 {
			roles = model.SystemGuestRoleId
		}
		user, err := th.Store.User().Save(th.Context, &model.User{
			Username: fmt.Sprintf("%s_%d", prefix, i), Email: fmt.Sprintf("%s_%d@example.com", prefix, i), Roles: roles,
		})
		require.NoError(t, err)
		users = append(users, user)
	}
	for _, i := range []int{0, 1, 2, 3, 4, 6, 7} {
		_, err := th.Store.Team().SaveMember(th.Context, &model.TeamMember{TeamId: th.BasicTeam.Id, UserId: users[i].Id, Roles: model.TeamUserRoleId}, 100)
		require.NoError(t, err)
	}
	for _, entry := range []struct{ user, channel string }{
		{users[2].Id, th.BasicChannel.Id},
		{users[3].Id, th.BasicChannel.Id},
		{users[3].Id, th.BasicPrivateChannel.Id},
		{users[2].Id, th.BasicDeletedChannel.Id},
	} {
		_, err := th.Store.Channel().SaveMember(th.Context, &model.ChannelMember{UserId: entry.user, ChannelId: entry.channel, Roles: model.ChannelUserRoleId, NotifyProps: model.GetDefaultChannelNotifyProps()})
		require.NoError(t, err)
	}
	_, err := th.Store.Bot().Save(&model.Bot{UserId: users[7].Id, Username: users[7].Username, OwnerId: th.SystemAdminUser.Id})
	require.NoError(t, err)
	db := th.Store.GetInternalMasterDB()
	_, err = db.Exec(`UPDATE TeamMembers SET DeleteAt=1 WHERE TeamId=$1 AND UserId=$2`, th.BasicTeam.Id, users[6].Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE Users SET DeleteAt=1 WHERE Id=$1`, users[5].Id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE Users SET FirstName='MiX_% literal', LastName='إدارة', Nickname='nick',
		Password='not-for-report', MfaSecret='not-for-report', MfaUsedTimestamps='["123"]',
		EmailVerified=true, AllowMarketing=true, LastPasswordUpdate=123, LastLogin=456,
		AuthData='admin-visible-auth', AuthService='ldap', NotifyProps='{"email":"false"}', Props='{"marker":"retained"}' WHERE Id=$1`, users[0].Id)
	require.NoError(t, err)
	require.NoError(t, th.Store.Status().SaveOrUpdate(&model.Status{UserId: users[0].Id, Status: model.StatusOnline, LastActivityAt: 12345}))
	day := time.Date(2025, time.January, 1, 12, 0, 0, 0, time.UTC)
	for _, offset := range []time.Duration{0, 24 * time.Hour, 25 * time.Hour} {
		_, err := th.Store.Post().Save(th.Context, &model.Post{UserId: users[0].Id, ChannelId: th.BasicChannel.Id, Message: "report fixture", CreateAt: day.Add(offset).UnixMilli()})
		require.NoError(t, err)
	}
	require.NoError(t, th.Store.User().RefreshPostStatsForUsers())

	cases := []struct {
		name     string
		filter   model.UserReportOptions
		expected int
	}{
		{"all and bot exclusion", model.UserReportOptions{}, 8},
		{"active", model.UserReportOptions{HideInactive: true}, 7},
		{"inactive", model.UserReportOptions{HideActive: true}, 1},
		{"team", model.UserReportOptions{Team: th.BasicTeam.Id}, 5},
		{"no team", model.UserReportOptions{HasNoTeam: true}, 3},
		{"no team takes precedence", model.UserReportOptions{HasNoTeam: true, Team: th.BasicTeam.Id}, 3},
		{"admin role", model.UserReportOptions{Role: "SYSTEM_ADMIN"}, 1},
		{"literal role wildcard", model.UserReportOptions{Role: "%"}, 0},
		{"all guests", model.UserReportOptions{GuestFilter: model.GuestFilterAll}, 3},
		{"single channel guest excludes archived", model.UserReportOptions{GuestFilter: model.GuestFilterSingleChannel}, 1},
		{"multiple channel guest", model.UserReportOptions{GuestFilter: model.GuestFilterMultipleChannel}, 1},
		{"guest takes role precedence", model.UserReportOptions{GuestFilter: model.GuestFilterAll, Role: model.SystemAdminRoleId}, 3},
		{"case folded multi token search", model.UserReportOptions{SearchTerm: prefix + " @mix"}, 1},
		{"arabic search", model.UserReportOptions{SearchTerm: prefix + " إدارة"}, 1},
		{"literal percent", model.UserReportOptions{SearchTerm: prefix + " %"}, 1},
		{"literal underscore percent", model.UserReportOptions{SearchTerm: prefix + " _%"}, 1},
		{"search escape removal", model.UserReportOptions{SearchTerm: prefix + " *mix"}, 1},
		{"id search", model.UserReportOptions{SearchTerm: users[0].Id}, 1},
		{"at username search", model.UserReportOptions{SearchTerm: "@" + prefix}, 8},
		{"empty last token", model.UserReportOptions{SearchTerm: prefix + " *"}, 8},
		{"date aggregate boundaries", model.UserReportOptions{ReportingBaseOptions: model.ReportingBaseOptions{StartAt: day.Add(24 * time.Hour).UnixMilli(), EndAt: day.Add(48 * time.Hour).UnixMilli()}}, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filter := tc.filter
			filter.SortColumn, filter.Direction, filter.PageSize = "Username", "next", model.ReportingMaxPageSize
			if filter.SearchTerm == "" {
				filter.SearchTerm = prefix
			}
			native, appErr := th.App.GetUsersForReporting(&filter)
			require.Nil(t, appErr)
			require.Len(t, native, tc.expected, "independent native filter oracle")
			filter.PageSize = 2
			got := []*model.UserReport{}
			for pageNumber := 0; pageNumber < 10; pageNumber++ {
				page, appErr := service.reportPage(th.Context.Context(), &filter)
				require.Nil(t, appErr)
				require.LessOrEqual(t, len(page), filter.PageSize)
				got = append(got, page...)
				if len(page) < filter.PageSize {
					break
				}
				last := page[len(page)-1]
				filter.FromId, filter.FromColumnValue = last.Id, last.Username
			}
			require.Equal(t, native, got, "complete sanitized profiles and aggregates must match")
			for _, report := range got {
				require.Empty(t, report.Password)
				require.Empty(t, report.MfaSecret)
				require.Nil(t, report.MfaUsedTimestamps)
				require.False(t, report.EmailVerified)
				require.False(t, report.AllowMarketing)
				require.Zero(t, report.LastPasswordUpdate)
				if report.Id == users[0].Id {
					require.Equal(t, "retained", report.Props["marker"])
					require.Equal(t, int64(456), report.LastLogin)
					require.NotNil(t, report.TotalPosts)
					if tc.name == "date aggregate boundaries" {
						require.Equal(t, 2, *report.TotalPosts)
					} else {
						require.Equal(t, 3, *report.TotalPosts)
					}
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(th.Context.Context())
	cancel()
	_, appErr := service.reportPage(ctx, &model.UserReportOptions{ReportingBaseOptions: model.ReportingBaseOptions{SortColumn: "Username", PageSize: 1}})
	require.NotNil(t, appErr)
}

func TestEnterpriseOnlineReportAuthorizationAndValidation(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	for _, route := range []string{"users?show_online_only=true", "users/count?show_online_only=true"} {
		code, _ := onlineReportRequest(t, model.NewAPIv4Client(th.Client.URL), route)
		require.Equal(t, http.StatusUnauthorized, code)
		code, _ = onlineReportRequest(t, th.Client, route)
		require.Equal(t, http.StatusForbidden, code)
	}
	for _, invalid := range []string{"page_size=0", "page_size=101", "sort_column=" + url.QueryEscape("Username;DROP"), "guest_filter=invalid", "team_filter=invalid", "hide_active=true&hide_inactive=true"} {
		code, body := onlineReportRequest(t, th.SystemAdminClient, "users?show_online_only=true&"+invalid)
		require.Equal(t, http.StatusBadRequest, code, string(body))
	}
	// Neither the native API nor its license contract is replaced by this repair.
	th.Server.SetLicense(nil)
	require.Nil(t, th.Server.License())
	code, body := onlineReportRequest(t, th.SystemAdminClient, "users?page_size=1&search_term="+url.QueryEscape(th.BasicUser.Username))
	require.Equal(t, http.StatusOK, code, string(body))
	var native []model.UserReport
	require.NoError(t, json.Unmarshal(body, &native))
	require.Len(t, native, 1)
	for _, route := range []string{"users?show_online_only=true", "users/count?show_online_only=true"} {
		code, body = onlineReportRequest(t, th.SystemAdminClient, route)
		require.Equal(t, http.StatusForbidden, code, string(body))
	}
}
