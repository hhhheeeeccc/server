package ip_filtering

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"

	"github.com/mattermost/enterprise/cloud"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/config"
	"github.com/stretchr/testify/require"
)

func TestPolicyWithBinaryParameters(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterprise)
	license.Features.Cloud = model.NewPointer(true)
	require.True(t, th.Server.SetLicense(license))
	parsed, err := url.Parse(*th.App.Config().SqlSettings.DataSource)
	require.NoError(t, err)
	query := parsed.Query()
	query.Set("binary_parameters", "yes")
	parsed.RawQuery = query.Encode()
	binaryDB, err := sql.Open("postgres", parsed.String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, binaryDB.Close()) })
	require.NoError(t, binaryDB.Ping())

	s := &Service{app: th.App, db: binaryDB}
	ctx := th.Context.Context()
	require.NoError(t, s.initPolicy(ctx))
	previous, err := s.readPolicy(ctx)
	require.NoError(t, err)
	requested := model.AllowedIPRanges{{CIDRBlock: "127.0.0.0/8", Enabled: true}}
	_, err = s.ApplyIPFilters(&requested)
	require.NoError(t, err)
	stored, err := s.readPolicy(ctx)
	require.NoError(t, err)
	require.Equal(t, requested, *stored)
	t.Cleanup(func() { _, _ = s.ApplyIPFilters(previous) })
}

func TestMain(m *testing.M) {
	app.RegisterCloudInterface(cloud.New)
	app.RegisterIPFilteringInterface(New)
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestIPFilteringAPIAndHTTPEnforcement(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterprise)
	license.Features.Cloud = model.NewPointer(true)
	require.True(t, th.Server.SetLicense(license))
	ctx := th.Context.Context()

	initial, _, err := th.SystemAdminClient.GetIPFilters(ctx)
	require.NoError(t, err)
	require.Empty(t, *initial)
	_, response, err := th.Client.GetIPFilters(ctx)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	requested := model.AllowedIPRanges{{CIDRBlock: "127.0.0.3/8", Enabled: true, Description: "local test"}}
	applied, _, err := th.SystemAdminClient.ApplyIPFilters(ctx, &requested)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.0/8", (*applied)[0].CIDRBlock)
	stored, _, err := th.SystemAdminClient.GetIPFilters(ctx)
	require.NoError(t, err)
	require.Equal(t, applied, stored)
	var version int64
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT Version FROM WorkspaceIPFilters WHERE Id=1`).Scan(&version))
	require.Equal(t, int64(1), version)

	service := th.App.IPFiltering().(*Service)
	served := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { served = true; w.WriteHeader(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodGet, "/api/v4/system/ping", nil)
	request.RemoteAddr = "10.0.0.5:1234"
	request.Header.Set("X-Forwarded-For", "127.0.0.1")
	result := httptest.NewRecorder()
	service.enforce(next).ServeHTTP(result, request)
	require.Equal(t, http.StatusForbidden, result.Code)
	require.False(t, served, "a direct client cannot spoof the forwarding header")

	request.RemoteAddr = "127.0.0.1:1234"
	result = httptest.NewRecorder()
	service.enforce(next).ServeHTTP(result, request)
	require.Equal(t, http.StatusNoContent, result.Code)
	require.True(t, served)

	lockout := model.AllowedIPRanges{{CIDRBlock: "10.0.0.0/8", Enabled: true}}
	_, response, err = th.SystemAdminClient.ApplyIPFilters(ctx, &lockout)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	stored, _, err = th.SystemAdminClient.GetIPFilters(ctx)
	require.NoError(t, err)
	require.Equal(t, applied, stored)
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT Version FROM WorkspaceIPFilters WHERE Id=1`).Scan(&version))
	require.Equal(t, int64(1), version)
	rawResponse, err := th.SystemAdminClient.DoAPIPost(ctx, "/ip_filtering", `[{"cidr_block":"10.0.0.0/8","enabled":true}]`+"\nnull")
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rawResponse.StatusCode)
	require.NoError(t, rawResponse.Body.Close())
	stored, _, err = th.SystemAdminClient.GetIPFilters(ctx)
	require.NoError(t, err)
	require.Equal(t, applied, stored, "trailing JSON must not bypass the administrator lockout check")
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT Version FROM WorkspaceIPFilters WHERE Id=1`).Scan(&version))
	require.Equal(t, int64(1), version)
	for _, corrupted := range []string{`null`, `[{"cidr_block":"not-a-cidr","enabled":true}]`} {
		_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE WorkspaceIPFilters SET Policy=$1::jsonb WHERE Id=1`, corrupted)
		require.NoError(t, err)
		result = httptest.NewRecorder()
		service.enforce(next).ServeHTTP(result, request)
		require.Equal(t, http.StatusServiceUnavailable, result.Code, corrupted)
		_, _, err = th.SystemAdminClient.GetIPFilters(ctx)
		require.Error(t, err, corrupted)
	}
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE WorkspaceIPFilters SET Policy=$1::jsonb WHERE Id=1`, `[{"cidr_block":"127.0.0.0/8","enabled":true}]`)
	require.NoError(t, err)
	result = httptest.NewRecorder()
	service.enforce(next).ServeHTTP(result, request)
	require.Equal(t, http.StatusNoContent, result.Code)

	_, err = th.Store.GetInternalMasterDB().Exec(`DELETE FROM WorkspaceIPFilters WHERE Id=1`)
	require.NoError(t, err)
	result = httptest.NewRecorder()
	service.enforce(next).ServeHTTP(result, request)
	require.Equal(t, http.StatusServiceUnavailable, result.Code, "missing shared policy must fail closed")

	th.Server.SetLicense(nil)
	_, response, err = th.SystemAdminClient.GetIPFilters(ctx)
	require.Error(t, err)
	require.Equal(t, http.StatusNotImplemented, response.StatusCode)
}

func TestIPFilteringProxyAndCIDRValidation(t *testing.T) {
	t.Setenv("WORKSPACE_IP_FILTER_TRUSTED_PROXIES", "192.0.2.0/24, 2001:db8::/32")
	t.Setenv("WORKSPACE_IP_FILTER_CLIENT_HEADER", "X-Real-IP")
	trusted, header, err := trustedProxyConfig()
	require.NoError(t, err)
	require.Len(t, trusted, 2)
	require.Equal(t, "X-Real-IP", header)
	t.Setenv("WORKSPACE_IP_FILTER_CLIENT_HEADER", "")
	_, _, err = trustedProxyConfig()
	require.Error(t, err, "a trusted proxy range needs an explicit overwritten client header")

	s := &Service{trusted: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, header: "X-Real-IP"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.3:1234"
	r.Header.Set("X-Real-IP", "2001:db8::10")
	ip, ok := s.clientIP(r)
	require.True(t, ok)
	require.Equal(t, "2001:db8::10", ip.String())
	r.Header.Set("X-Real-IP", "2001:db8::10, 10.0.0.1")
	_, ok = s.clientIP(r)
	require.False(t, ok)
	r.Header.Set("X-Real-IP", "2001:db8::10")
	r.Header.Add("X-Real-IP", "10.0.0.1")
	_, ok = s.clientIP(r)
	require.False(t, ok, "a trusted proxy must supply exactly one client IP header")
	r.RemoteAddr = "198.51.100.4:1234"
	ip, ok = s.clientIP(r)
	require.True(t, ok)
	require.Equal(t, "198.51.100.4", ip.String())

	invalid := model.AllowedIPRanges{{CIDRBlock: "not-an-ip", Enabled: true}}
	_, err = normalizePolicy(&invalid)
	require.Error(t, err)
	duplicate := model.AllowedIPRanges{{CIDRBlock: "10.0.0.1/8"}, {CIDRBlock: "10.0.0.0/8"}}
	_, err = normalizePolicy(&duplicate)
	require.Error(t, err)
	mapped := model.AllowedIPRanges{{CIDRBlock: "::ffff:192.0.2.8/120", Enabled: true}}
	canonical, err := normalizePolicy(&mapped)
	require.NoError(t, err)
	require.Equal(t, "192.0.2.0/24", (*canonical)[0].CIDRBlock)
	require.True(t, policyAllows(canonical, netip.MustParseAddr("192.0.2.10")))
	tooBroad := model.AllowedIPRanges{{CIDRBlock: "::ffff:192.0.2.0/95", Enabled: true}}
	_, err = normalizePolicy(&tooBroad)
	require.Error(t, err)

	t.Setenv("WORKSPACE_IP_FILTER_TRUSTED_PROXIES", "::ffff:192.0.2.0/120")
	t.Setenv("WORKSPACE_IP_FILTER_CLIENT_HEADER", "X-Real-IP")
	trusted, header, err = trustedProxyConfig()
	require.NoError(t, err)
	require.Equal(t, netip.MustParsePrefix("192.0.2.0/24"), trusted[0])
	mappedService := &Service{trusted: trusted, header: header}
	r.RemoteAddr = "192.0.2.3:1234"
	r.Header.Set("X-Real-IP", "198.51.100.4")
	ip, ok = mappedService.clientIP(r)
	require.True(t, ok)
	require.Equal(t, "198.51.100.4", ip.String())

	t.Setenv("WORKSPACE_IP_FILTER_TRUSTED_PROXIES", "::ffff:192.0.2.0/95")
	_, _, err = trustedProxyConfig()
	require.Error(t, err, "a mapped trusted range must not extend beyond IPv4 address space")
}

func TestIPFilteringFailsClosedWhenPolicyDatabaseIsUnavailable(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterprise)
	license.Features.Cloud = model.NewPointer(true)
	require.True(t, th.Server.SetLicense(license))

	policy := model.AllowedIPRanges{{CIDRBlock: "127.0.0.0/8", Enabled: true}}
	_, err := th.App.IPFiltering().(*Service).ApplyIPFilters(&policy)
	require.NoError(t, err)

	// Use a separate connection pool so the application can recover normally.
	// Closing it models an unavailable policy store without disrupting the test
	// server's own database connections.
	unavailableDB, err := sql.Open("postgres", *th.App.Config().SqlSettings.DataSource)
	require.NoError(t, err)
	require.NoError(t, unavailableDB.Ping())
	require.NoError(t, unavailableDB.Close())
	s := &Service{app: th.App, db: unavailableDB}
	served := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v4/system/ping", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	result := httptest.NewRecorder()
	s.enforce(next).ServeHTTP(result, request)
	require.Equal(t, http.StatusServiceUnavailable, result.Code)
	require.False(t, served, "a previously allowed client must not bypass a failed policy read")
	_, err = s.GetIPFilters()
	require.Error(t, err)
	_, err = s.ApplyIPFilters(&policy)
	require.Error(t, err)

	result = httptest.NewRecorder()
	th.App.IPFiltering().(*Service).enforce(next).ServeHTTP(result, request)
	require.Equal(t, http.StatusNoContent, result.Code, "the healthy store still serves the policy")
}

func TestIPFilteringReadHasDeadlineWhenConnectionPoolIsExhausted(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterprise)
	license.Features.Cloud = model.NewPointer(true)
	require.True(t, th.Server.SetLicense(license))

	blockedDB, err := sql.Open("postgres", *th.App.Config().SqlSettings.DataSource)
	require.NoError(t, err)
	blockedDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, blockedDB.Close()) })
	tx, err := blockedDB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer tx.Rollback()

	s := &Service{app: th.App, db: blockedDB}
	served := false
	request := httptest.NewRequest(http.MethodGet, "/api/v4/system/ping", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	result := httptest.NewRecorder()
	s.enforce(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(result, request)
	require.Equal(t, http.StatusServiceUnavailable, result.Code)
	require.False(t, served)
}

func TestIPFilteringPolicyIsSharedBetweenTwoAppInstances(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterprise)
	license.Features.Cloud = model.NewPointer(true)
	require.True(t, th.Server.SetLicense(license))

	peerConfig := th.App.Config().Clone()
	peerConfig.ServiceSettings.ListenAddress = model.NewPointer("localhost:0")
	memory, err := config.NewMemoryStoreWithOptions(&config.MemoryStoreOptions{IgnoreEnvironmentOverrides: true})
	require.NoError(t, err)
	require.NoError(t, memory.Set(peerConfig))
	configStore, err := config.NewStoreFromBacking(memory, nil, false)
	require.NoError(t, err)
	peerServer, err := app.NewServer(app.ConfigStore(configStore))
	require.NoError(t, err)
	t.Cleanup(func() { peerServer.Shutdown(); _ = configStore.Close() })
	_, err = api4.Init(peerServer)
	require.NoError(t, err)
	require.True(t, peerServer.SetLicense(license))
	require.NoError(t, peerServer.Start())

	first := th.App.IPFiltering().(*Service)
	peer := peerServer.IPFiltering.(*Service)
	request := httptest.NewRequest(http.MethodGet, "/api/v4/system/ping", nil)
	request.RemoteAddr = "10.2.3.4:1234"
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	serveOnPeer := func() int {
		response := httptest.NewRecorder()
		peer.enforce(next).ServeHTTP(response, request)
		return response.Code
	}
	require.Equal(t, http.StatusNoContent, serveOnPeer())

	allowLocal := model.AllowedIPRanges{{CIDRBlock: "127.0.0.0/8", Enabled: true}}
	_, err = first.ApplyIPFilters(&allowLocal)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, serveOnPeer(), "the peer must read the first node's committed policy")
	allowBoth := model.AllowedIPRanges{{CIDRBlock: "127.0.0.0/8", Enabled: true}, {CIDRBlock: "10.0.0.0/8", Enabled: true}}
	_, err = first.ApplyIPFilters(&allowBoth)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, serveOnPeer(), "the peer must see the next policy without restart")
}
