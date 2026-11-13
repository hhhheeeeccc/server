package data_retention_job

import (
	"context"
	"io"
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

// Delay actual object-store DELETE requests before the effect or after its
// acknowledgement, while leaving all SQL and other object requests available.
func TestRetentionObjectDeletionSessionLoss(t *testing.T) {
	for _, emulator := range objectstoretest.Cases() {
		t.Run(emulator.Name, func(t *testing.T) { testRetentionObjectSessionLoss(t, emulator) })
	}
}

func testRetentionObjectSessionLoss(t *testing.T, emulator objectstoretest.Emulator) {
	for _, tc := range []struct {
		name                   string
		successor, afterEffect bool
		interrupt              string
	}{
		{"before_effect/without_successor", false, false, ""},
		{"after_effect/without_successor", false, true, ""},
		{"before_effect/with_successor", true, false, ""},
		{"after_effect/with_successor", true, true, ""},
		{"license_removed", false, true, "license"},
		{"canceled", false, true, "cancel"},
		{"shutdown", false, true, "stop"},
	} {
		withSuccessor, afterEffect := tc.successor, tc.afterEffect
		t.Run(tc.name, func(t *testing.T) {
			endpoint := os.Getenv(emulator.EndpointEnv)
			if endpoint == "" {
				t.Skipf("set %s for object-store retention integration", emulator.EndpointEnv)
			}
			if !strings.Contains(endpoint, "://") {
				endpoint = "http://" + endpoint
			}
			target, err := url.Parse(endpoint)
			require.NoError(t, err)
			endpointPath := target.Path
			target.Path, target.RawPath = "", ""
			proxy := httputil.NewSingleHostReverseProxy(target)
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			drain := func() { releaseOnce.Do(func() { close(release) }) }
			var objectPath string
			var armed atomic.Bool
			var deletes, effectStatus atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				first := armed.Load() && r.Method == http.MethodDelete &&
					strings.HasSuffix(r.URL.Path, "/"+objectPath) && deletes.Add(1) == 1
				if !first {
					proxy.ServeHTTP(w, r)
					return
				}
				var buffered *httptest.ResponseRecorder
				if afterEffect {
					buffered = httptest.NewRecorder()
					proxy.ServeHTTP(buffered, r)
					effectStatus.Store(int32(buffered.Code))
				}
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				if buffered == nil {
					proxy.ServeHTTP(w, r)
					return
				}
				for key, values := range buffered.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(buffered.Code)
				_, _ = io.Copy(w, buffered.Body)
			}))
			defer server.Close()
			defer drain()
			proxyEndpoint := server.URL + endpointPath
			if emulator.Name == "s3" {
				proxyEndpoint = strings.TrimPrefix(proxyEndpoint, "http://")
			}
			fileSettings := emulator.Setup(t, proxyEndpoint)
			th := api4.SetupConfig(t, func(cfg *model.Config) {
				cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
				cfg.FileSettings = fileSettings
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
			objectPath = old.Path
			armed.Store(true)
			db := th.Store.GetInternalMasterDB()
			owner := func() int {
				var pid int
				_ = db.QueryRow(`SELECT pid FROM pg_locks WHERE locktype='advisory'
						AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
						AND classid=$1::oid AND objid=$2::oid AND objsubid=1 AND granted`,
					retentionExecutionLock>>32, retentionExecutionLock&0xffffffff).Scan(&pid)
				return pid
			}
			metadata := func(file *model.FileInfo) int {
				var count int
				require.NoError(t, db.QueryRow(`SELECT count(*) FROM FileInfo WHERE Id=$1`, file.Id).Scan(&count))
				return count
			}
			exists := func(file *model.FileInfo) bool {
				found, appErr := th.App.FileExists(file.Path)
				require.Nil(t, appErr)
				return found
			}
			type result struct {
				reason stopReason
				err    error
			}
			ctx, cancel := context.WithTimeout(th.Context.Context(), 25*time.Second)
			defer cancel()
			start := func(worker *Worker, cancelRequested <-chan struct{}) <-chan result {
				done := make(chan result, 1)
				go func() {
					reason, err := worker.execute(th.Context.WithContext(ctx), th.Context.Logger(), cancelRequested)
					done <- result{reason, err}
				}()
				return done
			}
			primary := New(th.Server).MakeWorker().(*Worker)
			firstCancel := make(chan struct{})
			var first <-chan result
			var primaryJob *model.Job
			if tc.interrupt == "" {
				first = start(primary, firstCancel)
			} else {
				primaryJob, err = th.Store.Job().Save(&model.Job{Id: model.NewId(), Type: model.JobTypeDataRetention,
					CreateAt: model.GetMillis(), Status: model.JobStatusPending, Data: model.StringMap{}})
				require.NoError(t, err)
				done := make(chan result, 1)
				first = done
				go func() {
					reason := continueWork
					if primary.doJob(primaryJob) {
						reason = workerStopped
					}
					done <- result{reason: reason}
				}()
			}
			var second <-chan result
			defer func() {
				drain()
				for _, done := range []<-chan result{first, second} {
					if done != nil {
						select {
						case <-done:
						case <-time.After(10 * time.Second):
							t.Error("worker did not drain")
						}
					}
				}
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("no actual object-store DELETE reached proxy")
			}
			if afterEffect {
				wantStatus := http.StatusNoContent
				if emulator.Name == "azure" {
					wantStatus = http.StatusAccepted
				}
				require.EqualValues(t, wantStatus, effectStatus.Load())
			}
			require.Equal(t, !afterEffect, exists(old), "probe must distinguish the external effect from its reply")
			require.Equal(t, 1, metadata(old))
			pid := owner()
			require.Positive(t, pid)
			var terminated bool
			require.NoError(t, db.QueryRow(`SELECT pg_terminate_backend($1) WHERE EXISTS
					(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND datname=current_database())`, pid).Scan(&terminated))
			require.True(t, terminated)
			require.Eventually(t, func() bool { return owner() == 0 }, time.Second, 10*time.Millisecond)
			if withSuccessor {
				second = start(New(th.Server).MakeWorker().(*Worker), make(chan struct{}))
				select {
				case outcome := <-second:
					require.NoError(t, outcome.err)
					require.Equal(t, continueWork, outcome.reason)
					second = nil
				case <-ctx.Done():
					t.Fatal("successor did not finish while predecessor HTTP reply was pending")
				}
				require.Zero(t, metadata(old))
				require.Zero(t, metadata(next))
			}
			switch tc.interrupt {
			case "license":
				th.Server.SetLicense(nil)
			case "cancel":
				require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, primaryJob.Id))
			case "stop":
				close(primary.stop)
			}
			drain()
			select {
			case outcome := <-first:
				first = nil
				if primaryJob == nil {
					require.ErrorContains(t, outcome.err, "original execution stopped")
					require.Equal(t, continueWork, outcome.reason)
				} else {
					stored, err := th.Store.Job().Get(th.Context, primaryJob.Id)
					require.NoError(t, err)
					wantStatus, wantReason := model.JobStatusError, continueWork
					switch tc.interrupt {
					case "cancel":
						wantStatus = model.JobStatusCanceled
					case "stop":
						wantStatus, wantReason = model.JobStatusPending, workerStopped
					default:
						require.Contains(t, stored.Data["error"], "original execution stopped")
					}
					require.Equal(t, wantReason, outcome.reason)
					require.Equal(t, wantStatus, stored.Status)
				}
			case <-ctx.Done():
				t.Fatal("predecessor did not finish after object-store reply was released")
			}
			remaining := metadata(old)
			t.Logf("object-session-loss after_effect=%t successor=%t killed_pid=%d delete_requests=%d body_exists=%t metadata_remaining=%d application_sql_available=true", afterEffect, withSuccessor, pid, deletes.Load(), exists(old), remaining)
			assert.Zero(t, remaining, "completed object deletion must not leave stale metadata after SQL-session loss")
			require.False(t, exists(old))
			if !withSuccessor {
				require.Equal(t, 1, metadata(next), "failed execution must not begin another file")
				require.True(t, exists(next))
				if tc.interrupt == "license" {
					require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
				}
				retry := start(New(th.Server).MakeWorker().(*Worker), make(chan struct{}))
				select {
				case outcome := <-retry:
					require.NoError(t, outcome.err)
				case <-ctx.Done():
					t.Fatal("fresh execution did not complete retry")
				}
				require.Zero(t, metadata(old))
				require.Zero(t, metadata(next))
			}
			for _, file := range []*model.FileInfo{current, bookmark} {
				require.Equal(t, 1, metadata(file))
				require.True(t, exists(file))
			}
		})
	}
}
