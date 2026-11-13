package ip_filtering

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
)

// BenchmarkIPFilteringRequest measures the complete licensed middleware path,
// including the shared PostgreSQL read performed for every request.
func BenchmarkIPFilteringRequest(b *testing.B) {
	th := api4.SetupConfig(b, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(b)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterprise)
	license.Features.Cloud = model.NewPointer(true)
	if !th.Server.SetLicense(license) {
		b.Fatal("unable to set Cloud Enterprise test license")
	}
	service := th.App.IPFiltering().(*Service)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := service.enforce(next)

	for _, count := range []int{0, 1, 100} {
		b.Run(fmt.Sprintf("ranges_%d", count), func(b *testing.B) {
			policy := make(model.AllowedIPRanges, 0, count)
			if count > 0 {
				policy = append(policy, model.AllowedIPRange{CIDRBlock: "127.0.0.0/8", Enabled: true})
			}
			for n := 1; n < count; n++ {
				policy = append(policy, model.AllowedIPRange{CIDRBlock: fmt.Sprintf("10.%d.%d.0/24", n/256, n%256), Enabled: true})
			}
			if _, err := service.ApplyIPFilters(&policy); err != nil {
				b.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/api/v4/system/ping", nil)
			request.RemoteAddr = "127.0.0.1:1234"
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusNoContent {
					b.Fatalf("unexpected middleware status: %d", response.Code)
				}
			}
		})
	}
}
