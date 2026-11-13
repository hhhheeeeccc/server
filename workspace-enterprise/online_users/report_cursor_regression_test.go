package online_users

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestReviewOnlineReportEmptyAndTiedCursors(t *testing.T) {
	for _, tc := range []struct {
		name, firstName, direction string
	}{
		{"empty first name", "", "asc"},
		{"duplicate first name descending", "same", "desc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
			require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
			prefix := "cursor_" + model.NewId()
			var users []*model.User
			for i := range 2 {
				user, err := th.Store.User().Save(th.Context, &model.User{
					Username: fmt.Sprintf("%s_%d", prefix, i), Email: fmt.Sprintf("%s_%d@example.com", prefix, i),
					FirstName: tc.firstName, Roles: model.SystemUserRoleId,
				})
				require.NoError(t, err)
				require.NoError(t, th.Store.Status().SaveOrUpdate(&model.Status{UserId: user.Id, Status: model.StatusOnline}))
				users = append(users, user)
			}
			sort.Slice(users, func(i, j int) bool { return users[i].Id < users[j].Id })
			query := url.Values{"show_online_only": {"true"}, "page_size": {"1"}, "sort_column": {"FirstName"}, "sort_direction": {tc.direction}, "search_term": {prefix}}
			code, body := onlineReportRequest(t, th.SystemAdminClient, "users?"+query.Encode())
			require.Equal(t, http.StatusOK, code, string(body))
			var first []model.UserReport
			require.NoError(t, json.Unmarshal(body, &first))
			require.Len(t, first, 1)
			query.Set("from_id", first[0].Id)
			query.Set("from_column_value", first[0].FirstName)
			code, body = onlineReportRequest(t, th.SystemAdminClient, "users?"+query.Encode())
			require.Equal(t, http.StatusOK, code, string(body))
			var second []model.UserReport
			require.NoError(t, json.Unmarshal(body, &second))
			require.Len(t, second, 1, "the remaining online user must not disappear")
			require.NotEqual(t, first[0].Id, second[0].Id, "the cursor must progress even when names are empty")
		})
	}
}

func TestReviewOnlineReportContinuesPastEmptyNamesBatch(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	prefix := "sparse_" + model.NewId()
	var ids []string
	for i := range model.ReportingMaxPageSize + 1 {
		user, err := th.Store.User().Save(th.Context, &model.User{
			Username: fmt.Sprintf("%s_%d", prefix, i), Email: fmt.Sprintf("%s_%d@example.com", prefix, i), Roles: model.SystemUserRoleId,
		})
		require.NoError(t, err)
		ids = append(ids, user.Id)
	}
	sort.Strings(ids)
	target := ids[len(ids)-1]
	require.NoError(t, th.Store.Status().SaveOrUpdate(&model.Status{UserId: target, Status: model.StatusOnline}))
	query := url.Values{"show_online_only": {"true"}, "page_size": {"1"}, "sort_column": {"FirstName"}, "search_term": {prefix}}
	code, body := onlineReportRequest(t, th.SystemAdminClient, "users?"+query.Encode())
	require.Equal(t, http.StatusOK, code, string(body))
	var page []model.UserReport
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page, 1, "online filtering must keep scanning beyond the first 100 empty names")
	require.Equal(t, target, page[0].Id)
}

func TestEnterpriseOnlineReportSparsePreviousBatches(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	prefix := "sparse_prev_" + model.NewId()
	var ids []string
	for i := range 151 {
		user, err := th.Store.User().Save(th.Context, &model.User{
			Username: fmt.Sprintf("%s_%d", prefix, i), Email: fmt.Sprintf("%s_%d@example.com", prefix, i), Roles: model.SystemUserRoleId,
		})
		require.NoError(t, err)
		ids = append(ids, user.Id)
	}
	sort.Strings(ids)
	for _, i := range []int{0, 1, 100, 150} {
		require.NoError(t, th.Store.Status().SaveOrUpdate(&model.Status{UserId: ids[i], Status: model.StatusOnline}))
	}
	query := url.Values{
		"show_online_only": {"true"}, "page_size": {"2"}, "sort_column": {"FirstName"},
		"sort_direction": {"desc"}, "search_term": {prefix},
	}
	pageIDs := func() []string {
		code, body := onlineReportRequest(t, th.SystemAdminClient, "users?"+query.Encode())
		require.Equal(t, http.StatusOK, code, string(body))
		var page []model.UserReport
		require.NoError(t, json.Unmarshal(body, &page))
		result := make([]string, len(page))
		for i, report := range page {
			result[i] = report.Id
		}
		return result
	}
	require.Equal(t, []string{ids[0], ids[1]}, pageIDs())
	query.Set("from_id", ids[1])
	query.Set("from_column_value", "")
	require.Equal(t, []string{ids[100], ids[150]}, pageIDs())
	query.Set("direction", "prev")
	query.Set("from_id", ids[150])
	require.Equal(t, []string{ids[1], ids[100]}, pageIDs(), "retain the nearest two online records across multiple source batches")
	query.Set("from_id", ids[100])
	require.Equal(t, []string{ids[0], ids[1]}, pageIDs())
	code, body := onlineReportRequest(t, th.SystemAdminClient, "users/count?show_online_only=true&search_term="+url.QueryEscape(prefix))
	require.Equal(t, http.StatusOK, code, string(body))
	var count int
	require.NoError(t, json.Unmarshal(body, &count))
	require.Equal(t, 4, count)
}
