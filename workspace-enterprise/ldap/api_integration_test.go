package ldap

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/app/platform"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	app.RegisterLdapInterface(New)
	app.RegisterJobsLdapSyncInterface(NewSyncJob)
	platform.RegisterLdapDiagnosticInterface(NewDiagnostics)
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func setupDirectoryAPI(t *testing.T) (*api4.TestHelper, *testDirectory) {
	t.Helper()
	f := newTestDirectory(t, "")
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.LdapSettings = f.settings()
		// Directory/API behavior is independent of the optional Redis cache.
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		*cfg.EmailSettings.SendEmailNotifications = false
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	return th, f
}

func listDirectoryGroups(t *testing.T, th *api4.TestHelper, query string) (int, []*groupResponse) {
	t.Helper()
	response, err := th.SystemAdminClient.DoAPIGet(th.Context.Context(), "/ldap/groups?"+query, "")
	require.NoError(t, err)
	defer response.Body.Close()
	var result struct {
		Count  int              `json:"count"`
		Groups []*groupResponse `json:"groups"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
	return result.Count, result.Groups
}

func TestLDAPGroupsHTTPListingLinkingAndPermissions(t *testing.T) {
	th, f := setupDirectoryAPI(t)
	ctx := th.Context.Context()
	count, groups := listDirectoryGroups(t, th, "page=0&per_page=2")
	require.Equal(t, 3, count)
	require.Len(t, groups, 2)
	require.Equal(t, "Alpha", groups[0].DisplayName)
	require.Equal(t, "group-a", groups[0].RemoteID)
	require.Nil(t, groups[0].ID)
	count, groups = listDirectoryGroups(t, th, "page=1&per_page=2")
	require.Equal(t, 3, count)
	require.Len(t, groups, 1)
	require.Equal(t, "Gamma", groups[0].DisplayName)
	count, groups = listDirectoryGroups(t, th, "page=0&per_page=2&q=ALP")
	require.Equal(t, 1, count)
	require.Equal(t, "group-a", groups[0].RemoteID)

	linked, _, err := th.SystemAdminClient.LinkLdapGroup(ctx, "group-b")
	require.NoError(t, err)
	require.True(t, model.IsValidId(linked.Id))
	require.Equal(t, "Beta", linked.DisplayName)
	count, groups = listDirectoryGroups(t, th, "page=0&per_page=10&is_linked=true")
	require.Equal(t, 1, count)
	require.Equal(t, linked.Id, *groups[0].ID)
	require.False(t, *groups[0].HasSyncables)
	_, err = th.Store.Group().CreateGroupSyncable(model.NewGroupTeam(linked.Id, th.BasicTeam.Id, false))
	require.NoError(t, err)
	count, groups = listDirectoryGroups(t, th, "page=0&per_page=10&is_configured=true")
	require.Equal(t, 1, count)
	require.True(t, *groups[0].HasSyncables)
	count, groups = listDirectoryGroups(t, th, "page=0&per_page=10&is_linked=false")
	require.Equal(t, 2, count)
	require.Len(t, groups, 2)
	_, _, err = th.SystemAdminClient.UnlinkLdapGroup(ctx, "group-b")
	require.NoError(t, err)
	count, groups = listDirectoryGroups(t, th, "page=0&per_page=10&is_linked=true")
	require.Zero(t, count)
	require.Empty(t, groups)

	before := f.binds.Load()
	response, err := th.Client.DoAPIGet(ctx, "/ldap/groups?page=0&per_page=10", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.Equal(t, before, f.binds.Load(), "unauthorized requests must not contact the directory")
	_, resp, err := th.Client.LinkLdapGroup(ctx, "group-a")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	_, resp, err = th.SystemAdminClient.LinkLdapGroup(ctx, "missing")
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.LDAPGroups = new(false)
	require.True(t, th.Server.SetLicense(license))
	response, err = th.SystemAdminClient.DoAPIGet(ctx, "/ldap/groups?page=0&per_page=10", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
}

func TestLDAPDiagnosticsHTTPUnsavedSettingsAndResults(t *testing.T) {
	th, f := setupDirectoryAPI(t)
	ctx := th.Context.Context()
	// The wizard can test a configured directory before enabling LDAP.
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.LdapSettings.Enable = new(false)
		cfg.LdapSettings.EnableSync = new(false)
	})
	body := `{"BindPassword":"` + model.FakeSetting + `"}`
	response, err := th.SystemAdminClient.DoAPIPost(ctx, "/ldap/test_connection", body)
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, "fixture-password", *th.App.Config().LdapSettings.BindPassword)
	require.False(t, *th.App.Config().LdapSettings.Enable)
	response, err = th.SystemAdminClient.DoAPIPost(ctx, "/ldap/test_diagnostics?test=attributes", body)
	require.NoError(t, err)
	var results []model.LdapDiagnosticResult
	require.NoError(t, json.NewDecoder(response.Body).Decode(&results))
	response.Body.Close()
	byName := map[string]model.LdapDiagnosticResult{}
	for _, result := range results {
		byName[result.TestName] = result
	}
	require.Equal(t, 2, byName["IdAttribute"].TotalCount)
	require.Equal(t, 2, byName["IdAttribute"].EntriesWithValue)
	require.Equal(t, 1, byName["EmailAttribute"].EntriesWithValue)
	require.Len(t, byName["EmailAttribute"].SampleResults, 1)
	require.Empty(t, byName["EmailAttribute"].Error)

	response, err = th.SystemAdminClient.DoAPIPost(ctx, "/ldap/test_diagnostics?test=group_attributes", body)
	require.NoError(t, err)
	require.NoError(t, json.NewDecoder(response.Body).Decode(&results))
	response.Body.Close()
	require.Len(t, results, 2)
	require.Equal(t, 3, results[0].EntriesWithValue)
	response, err = th.SystemAdminClient.DoAPIPost(ctx, "/ldap/test_diagnostics?test=filters", `{"UserFilter":"invalid-filter"}`)
	require.NoError(t, err)
	require.NoError(t, json.NewDecoder(response.Body).Decode(&results))
	response.Body.Close()
	for _, result := range results {
		if result.TestName == "UserFilter" {
			require.NotEmpty(t, result.Error)
			require.Zero(t, result.TotalCount)
		}
		if result.TestName == "GroupFilter" {
			require.Empty(t, result.Error)
			require.Equal(t, 3, result.TotalCount)
		}
	}
	require.Equal(t, "(objectClass=person)", *th.App.Config().LdapSettings.UserFilter)
	before := f.binds.Load()
	response, err = th.SystemAdminClient.DoAPIPost(ctx, "/ldap/test_connection", `{"LdapServer":"another.example.test"}`)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.Equal(t, before, f.binds.Load())
	response, err = th.Client.DoAPIPost(ctx, "/ldap/test_connection", body)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.Equal(t, before, f.binds.Load())
	vendor, version, vendorErr := th.App.LdapDiagnostic().GetVendorNameAndVendorVersion(th.Context)
	require.NoError(t, vendorErr)
	require.Equal(t, "Fixture LDAP", vendor)
	require.Equal(t, "1.0", version)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.LdapSettings.Enable = new(true) })
	_, err = th.SystemAdminClient.TestLdap(ctx)
	require.NoError(t, err)
}

func TestLDAPDisabledEmptyDirectoryAndFailures(t *testing.T) {
	th, f := setupDirectoryAPI(t)
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.LdapSettings.Enable = new(false)
		cfg.LdapSettings.EnableSync = new(false)
	})
	count, groups := listDirectoryGroups(t, th, "page=0&per_page=10")
	require.Zero(t, count)
	require.Empty(t, groups)
	require.Zero(t, f.binds.Load())
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.LdapSettings.Enable = new(true)
		cfg.LdapSettings.BindPassword = new("incorrect")
	})
	response, err := th.SystemAdminClient.DoAPIGet(th.Context.Context(), "/ldap/groups?page=0&per_page=10", "")
	require.Error(t, err)
	require.Equal(t, http.StatusBadGateway, response.StatusCode, "connection failures must not look like an empty successful directory")
	th.Server.SetLicense(nil)
	response, err = th.SystemAdminClient.DoAPIGet(th.Context.Context(), "/ldap/groups?page=0&per_page=10", "")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
}
