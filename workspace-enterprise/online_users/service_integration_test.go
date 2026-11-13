package online_users

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	app.RegisterComplianceInterface(func(a *app.App) einterfaces.ComplianceInterface { Register(a); return nil })
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func onlineReportRequest(t *testing.T, client *model.Client4, route string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, client.URL+"/api/v4/reports/"+route, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+client.AuthToken)
	response, err := client.HTTPClient.Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, body
}

func TestOnlineUserReportFiltersBeforePaginationAndCount(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))

	first := th.CreateUser(t)
	second := th.CreateUser(t)
	offline := th.CreateUser(t)
	for _, item := range []struct {
		user *model.User
		name string
	}{
		{first, "a_online_" + first.Id},
		{offline, "b_offline_" + offline.Id},
		{second, "c_online_" + second.Id},
	} {
		_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE Users SET Username=$1 WHERE Id=$2`, item.name, item.user.Id)
		require.NoError(t, err)
		item.user.Username = item.name
	}
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE Status SET Status=$1, LastActivityAt=0`, model.StatusOffline)
	require.NoError(t, err)
	for _, user := range []*model.User{first, second} {
		require.NoError(t, th.Store.Status().SaveOrUpdate(&model.Status{UserId: user.Id, Status: model.StatusOnline, LastActivityAt: model.GetMillis()}))
	}
	require.NoError(t, th.Store.Status().SaveOrUpdate(&model.Status{UserId: offline.Id, Status: model.StatusOffline, LastActivityAt: 0}))

	permissions := th.SaveDefaultRolePermissions(t)
	defer th.RestoreDefaultRolePermissions(t, permissions)
	th.AddPermissionToRole(t, model.PermissionSysconsoleReadUserManagementUsers.Id, model.SystemUserRoleId)

	code, body := onlineReportRequest(t, th.Client, "users/count?show_online_only=true")
	require.Equal(t, http.StatusOK, code, string(body))
	var count int
	require.NoError(t, json.Unmarshal(body, &count))
	require.Equal(t, 2, count)
	service := &Service{app: th.App}
	batchIDs, appErr := service.onlineIDsForReports(context.Background(), []*model.UserReport{{User: *first}}, model.GetMillis()-recentActivityWindow.Milliseconds())
	require.Nil(t, appErr)
	require.Equal(t, map[string]struct{}{first.Id: {}}, batchIDs, "a batch query must not collect unrelated online users")
	batchedCount, appErr := service.countOnlineReports(context.Background(), &model.UserReportOptions{}, 1, model.GetMillis()-recentActivityWindow.Milliseconds())
	require.Nil(t, appErr)
	require.Equal(t, int64(2), batchedCount)

	users := []*model.User{first, second}
	sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
	code, body = onlineReportRequest(t, th.Client, "users?show_online_only=true&page_size=1")
	require.Equal(t, http.StatusOK, code, string(body))
	var page []model.UserReport
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page, 1)
	require.Equal(t, users[0].Id, page[0].Id)

	cursor := url.Values{"show_online_only": {"true"}, "page_size": {"1"}, "from_id": {page[0].Id}, "from_column_value": {page[0].Username}}
	code, body = onlineReportRequest(t, th.Client, "users?"+cursor.Encode())
	require.Equal(t, http.StatusOK, code, string(body))
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page, 1)
	require.Equal(t, users[1].Id, page[0].Id)

	previous := url.Values{"show_online_only": {"true"}, "page_size": {"1"}, "direction": {"prev"}, "from_id": {page[0].Id}, "from_column_value": {page[0].Username}}
	code, body = onlineReportRequest(t, th.Client, "users?"+previous.Encode())
	require.Equal(t, http.StatusOK, code, string(body))
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page, 1)
	require.Equal(t, users[0].Id, page[0].Id)

	code, body = onlineReportRequest(t, th.Client, "users?show_online_only=true&page_size=1&sort_direction=desc")
	require.Equal(t, http.StatusOK, code, string(body))
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page, 1)
	require.Equal(t, users[1].Id, page[0].Id)
	descending := url.Values{"show_online_only": {"true"}, "page_size": {"1"}, "sort_direction": {"desc"}, "from_id": {page[0].Id}, "from_column_value": {page[0].Username}}
	code, body = onlineReportRequest(t, th.Client, "users?"+descending.Encode())
	require.Equal(t, http.StatusOK, code, string(body))
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page, 1)
	require.Equal(t, users[0].Id, page[0].Id)

	th.RemovePermissionFromRole(t, model.PermissionSysconsoleReadUserManagementUsers.Id, model.SystemUserRoleId)
	code, _ = onlineReportRequest(t, th.Client, "users/count?show_online_only=true")
	require.Equal(t, http.StatusForbidden, code)
}
