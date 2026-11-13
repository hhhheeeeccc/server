package data_retention_job

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/enterprise/internal/objectstoretest"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Close the TCP response after a real successful DELETE. Drop all retries too,
// so SDK retry success cannot hide an ambiguous storage outcome from the worker.
func TestRetentionAmbiguousObjectReply(t *testing.T) {
	for _, emulator := range objectstoretest.Cases() {
		t.Run(emulator.Name, func(t *testing.T) {
			for _, tc := range []struct {
				name, part                                          string
				afterEffect, loseSQL, loseLicense, denyConfirmation bool
			}{
				{"before_effect", "body", false, false, false, false},
				{"after_effect", "body", true, false, false, false},
				{"pending_storage_config", "body", true, false, false, false},
				{"after_effect_sql_loss", "body", true, true, false, false},
				{"after_effect_license_removed", "body", true, false, true, false},
				{"confirmation_unavailable", "body", true, false, false, true},
				{"preview_before_effect", "preview", false, false, false, false},
				{"thumbnail_before_effect", "thumbnail", false, false, false, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					endpoint := os.Getenv(emulator.EndpointEnv)
					if endpoint == "" {
						t.Skipf("set %s for object-store retention tests", emulator.EndpointEnv)
					}
					if !strings.Contains(endpoint, "://") {
						endpoint = "http://" + endpoint
					}
					target, err := url.Parse(endpoint)
					require.NoError(t, err)
					endpointPath := target.Path
					target.Path, target.RawPath = "", ""
					proxy := httputil.NewSingleHostReverseProxy(target)
					var armed atomic.Bool
					var attempts, status atomic.Int32
					var targetPath string
					entered := make(chan struct{})
					var once sync.Once
					front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						selected := armed.Load() && strings.HasSuffix(r.URL.Path, "/"+targetPath)
						if selected && r.Method == http.MethodHead && tc.denyConfirmation && attempts.Load() > 0 {
							w.Header().Set("x-amz-error-code", "AccessDenied")
							w.Header().Set("x-ms-error-code", "AuthorizationFailure")
							w.WriteHeader(http.StatusForbidden)
							return
						}
						if !selected || r.Method != http.MethodDelete {
							proxy.ServeHTTP(w, r)
							return
						}
						attempts.Add(1)
						if tc.afterEffect {
							buffered := httptest.NewRecorder()
							proxy.ServeHTTP(buffered, r)
							status.CompareAndSwap(0, int32(buffered.Code))
						}
						once.Do(func() { close(entered) })
						connection, _, err := w.(http.Hijacker).Hijack()
						if err == nil {
							_ = connection.Close()
						}
					}))
					defer front.Close()
					proxyEndpoint := front.URL + endpointPath
					if emulator.Name == "s3" {
						proxyEndpoint = strings.TrimPrefix(proxyEndpoint, "http://")
					}
					settings := emulator.Setup(t, proxyEndpoint)
					settings.AmazonS3RequestTimeoutMilliseconds = new(int64(1200))
					settings.AzureRequestTimeoutMilliseconds = new(int64(1200))
					var alternate model.FileSettings
					if tc.name == "pending_storage_config" {
						alternateEndpoint := endpoint
						if emulator.Name == "s3" {
							alternateEndpoint = strings.TrimPrefix(endpoint, "http://")
						}
						alternate = emulator.Setup(t, alternateEndpoint)
					}
					th := api4.SetupConfig(t, func(cfg *model.Config) {
						cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
						cfg.FileSettings = settings
						cfg.ServiceSettings.AllowedUntrustedInternalConnections = new("127.0.0.1")
						cfg.DataRetentionSettings.EnableMessageDeletion = new(false)
						cfg.DataRetentionSettings.EnableFileDeletion = new(true)
						cfg.DataRetentionSettings.FileRetentionDays = new(0)
						cfg.DataRetentionSettings.FileRetentionHours = new(1)
						cfg.DataRetentionSettings.BatchSize = new(1)
						cfg.DataRetentionSettings.TimeBetweenBatchesMilliseconds = new(0)
					}).InitBasic(t)
					require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
					upload := func(name string, age time.Duration, owner string) *model.FileInfo {
						file, appErr := th.App.DoUploadFile(th.Context, time.Now().Add(-age), th.BasicTeam.Id, th.BasicChannel.Id, owner, name, []byte(name), false)
						require.Nil(t, appErr)
						return file
					}
					old := upload("expired.txt", 48*time.Hour, th.BasicUser.Id)
					next := upload("next.txt", 47*time.Hour, th.BasicUser.Id)
					current := upload("current.txt", 0, th.BasicUser.Id)
					bookmark := upload("bookmark.txt", 48*time.Hour, model.BookmarkFileOwner)
					old.PreviewPath, old.ThumbnailPath = old.Path+".preview", old.Path+".thumb"
					for _, path := range []string{old.PreviewPath, old.ThumbnailPath} {
						_, appErr := th.App.WriteFile(bytes.NewReader([]byte("preview")), path)
						require.Nil(t, appErr)
					}
					db := th.Store.GetInternalMasterDB()
					_, err = db.Exec(`UPDATE FileInfo SET PreviewPath=$1,ThumbnailPath=$2 WHERE Id=$3`, old.PreviewPath, old.ThumbnailPath, old.Id)
					require.NoError(t, err)
					sourceBackend := th.App.FileBackend()
					targetPath = old.Path
					switch tc.part {
					case "preview":
						targetPath = old.PreviewPath
					case "thumbnail":
						targetPath = old.ThumbnailPath
					}
					armed.Store(true)
					ctx, cancel := context.WithTimeout(th.Context.Context(), 20*time.Second)
					defer cancel()
					done := make(chan error, 1)
					go func() {
						_, err := New(th.Server).MakeWorker().(*Worker).execute(th.Context.WithContext(ctx), th.Context.Logger(), make(chan struct{}))
						done <- err
					}()
					select {
					case <-entered:
					case <-ctx.Done():
						t.Fatal("no DELETE reached fault proxy")
					}
					if tc.afterEffect {
						want := http.StatusNoContent
						if emulator.Name == "azure" {
							want = http.StatusAccepted
						}
						require.EqualValues(t, want, status.Load(), "the effect must precede the lost TCP reply")
					}
					if tc.loseSQL {
						var pid int
						require.NoError(t, db.QueryRow(`SELECT pid FROM pg_locks WHERE locktype='advisory'
                            AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
                            AND classid=$1::oid AND objid=$2::oid AND objsubid=1 AND granted`, retentionExecutionLock>>32, retentionExecutionLock&0xffffffff).Scan(&pid))
						var killed bool
						require.NoError(t, db.QueryRow(`SELECT pg_terminate_backend($1) WHERE EXISTS
                            (SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND datname=current_database())`, pid).Scan(&killed))
						require.True(t, killed)
					}
					if tc.loseLicense {
						th.Server.SetLicense(nil)
					}
					if tc.name == "pending_storage_config" {
						th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings = alternate })
						// Storage is initialized at server startup. A configuration
						// update must not redirect this in-flight removal or its HEAD.
						require.Same(t, sourceBackend, th.App.FileBackend())
						require.Same(t, sourceBackend, th.Server.FileBackend())
					}
					select {
					case err = <-done:
					case <-ctx.Done():
						t.Fatal("worker did not finish after the configured storage timeout")
					}
					armed.Store(false)
					assert.Error(t, err, "an ambiguous removal or incomplete preview must stop this execution")
					count := func(file *model.FileInfo) int {
						var n int
						require.NoError(t, db.QueryRow(`SELECT count(*) FROM FileInfo WHERE Id=$1`, file.Id).Scan(&n))
						return n
					}
					exists := func(path string) bool {
						found, err := sourceBackend.FileExists(path)
						require.NoError(t, err)
						return found
					}
					wantMetadata := 1
					if tc.afterEffect && !tc.denyConfirmation {
						wantMetadata = 0
					}
					bodyExists, metadata := exists(old.Path), count(old)
					t.Logf("ambiguous-object-reply part=%s after_effect=%t sql_loss=%t attempts=%d body_exists=%t metadata_remaining=%d expected_metadata=%d", tc.part, tc.afterEffect, tc.loseSQL, attempts.Load(), bodyExists, metadata, wantMetadata)
					assert.Equal(t, wantMetadata, metadata, "preserve metadata until all object paths are confirmed removed")
					assert.Equal(t, tc.part == "body" && !tc.afterEffect, bodyExists)
					assert.Equal(t, 1, count(next), "a failed or ambiguous removal must not begin the next file")
					assert.True(t, exists(next.Path))
					if tc.part != "body" {
						assert.True(t, exists(targetPath), "an unremoved preview must retain its recovery metadata")
					}
					for _, file := range []*model.FileInfo{current, bookmark} {
						require.Equal(t, 1, count(file))
						require.True(t, exists(file.Path))
					}
					if tc.loseLicense {
						_, err = New(th.Server).MakeWorker().(*Worker).execute(th.Context, th.Context.Logger(), make(chan struct{}))
						require.Error(t, err)
						require.Equal(t, 1, count(next))
						require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
					}
					if tc.name == "pending_storage_config" {
						th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings = settings })
					}
					_, err = New(th.Server).MakeWorker().(*Worker).execute(th.Context, th.Context.Logger(), make(chan struct{}))
					require.NoError(t, err)
					require.Zero(t, count(old))
					require.Zero(t, count(next))
					for _, path := range []string{old.Path, old.PreviewPath, old.ThumbnailPath} {
						assert.False(t, exists(path), "a fresh job must finish all remaining object paths")
					}
				})
			}
		})
	}
}
