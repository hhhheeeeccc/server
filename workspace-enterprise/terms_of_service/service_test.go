package terms_of_service_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	_ "github.com/mattermost/enterprise"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestTermsOfServiceRequiresLicensedFeatureAndAdmin(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	ctx := context.Background()
	text := "Workspace terms " + model.NewId()

	_, response, err := th.SystemAdminClient.CreateTermsOfService(ctx, text, th.SystemAdminUser.Id)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	license := model.NewTestLicense()
	license.Features.CustomTermsOfService = model.NewPointer(false)
	th.App.Srv().SetLicense(license)
	_, response, err = th.SystemAdminClient.CreateTermsOfService(ctx, text, th.SystemAdminUser.Id)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	current, _, err := th.SystemAdminClient.GetTermsOfService(ctx, "")
	require.NoError(t, err)
	require.Empty(t, current.Id, "a license without this feature must not create terms")

	license.Features.CustomTermsOfService = model.NewPointer(true)
	th.App.Srv().SetLicense(license)
	_, response, err = th.Client.CreateTermsOfService(ctx, text, th.BasicUser.Id)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	created, _, err := th.SystemAdminClient.CreateTermsOfService(ctx, text, th.SystemAdminUser.Id)
	require.NoError(t, err)
	require.NotEmpty(t, created.Id)
	require.Equal(t, text, created.Text)
	resubmitted, _, err := th.SystemAdminClient.CreateTermsOfService(ctx, text, th.SystemAdminUser.Id)
	require.NoError(t, err)
	require.Equal(t, created.Id, resubmitted.Id, "repeating the current text must not create another version")

	th.App.Srv().SetLicense(nil)
	current, _, err = th.Client.GetTermsOfService(ctx, "")
	require.NoError(t, err)
	require.Equal(t, created.Id, current.Id, "existing terms remain readable without a license")
}

func TestConcurrentTermsPublicationCreatesOneVersion(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicense()
	license.Features.CustomTermsOfService = model.NewPointer(true)
	th.App.Srv().SetLicense(license)
	text := "Concurrent Workspace terms " + model.NewId()
	const callers = 8
	start := make(chan struct{})
	results := make(chan string, callers)
	errors := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			<-start
			created, _, err := th.SystemAdminClient.CreateTermsOfService(context.Background(), text, th.SystemAdminUser.Id)
			if err != nil {
				errors <- err
				return
			}
			results <- created.Id
		})
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	ids := map[string]bool{}
	for id := range results {
		ids[id] = true
	}
	require.Len(t, ids, 1, "concurrent identical publications must return the same version")
	var count int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM TermsOfService WHERE Text=$1`, text).Scan(&count))
	require.Equal(t, 1, count)
	first, _, err := th.SystemAdminClient.GetTermsOfService(context.Background(), "")
	require.NoError(t, err)
	second, _, err := th.SystemAdminClient.CreateTermsOfService(context.Background(), text+" updated", th.SystemAdminUser.Id)
	require.NoError(t, err)
	require.Greater(t, second.CreateAt, first.CreateAt, "a new version must sort after the previous one")
	latest, _, err := th.SystemAdminClient.GetTermsOfService(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, second.Id, latest.Id, "Enterprise read must see the published version")
	coreLatest, appErr := th.App.GetLatestTermsOfService()
	require.Nil(t, appErr)
	require.Equal(t, second.Id, coreLatest.Id, "publishing must invalidate the cached previous version")
}

func TestTermsPublicationRejectsOversizedAndTrailingBodies(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicense()
	license.Features.CustomTermsOfService = model.NewPointer(true)
	th.App.Srv().SetLicense(license)
	rejectedText := "rejected-terms-" + model.NewId()
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"trailing JSON", `{"text":"` + rejectedText + `"} true`, http.StatusBadRequest},
		{"unknown field", `{"text":"` + rejectedText + `","user_id":"other-user"}`, http.StatusBadRequest},
		{"oversized body", `{"text":"` + strings.Repeat("x", 2<<20) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := th.SystemAdminClient.DoAPIRequestWithHeaders(context.Background(), http.MethodPost, "/terms_of_service", tc.body, nil)
			require.Error(t, err)
			require.Equal(t, tc.status, response.StatusCode)
			require.NoError(t, response.Body.Close())
		})
	}
	var published int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM TermsOfService WHERE Text=$1`, rejectedText).Scan(&published))
	require.Zero(t, published)
}

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}
