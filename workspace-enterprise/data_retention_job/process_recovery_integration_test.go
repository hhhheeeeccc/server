package data_retention_job

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/enterprise/internal/objectstoretest"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/config"
	"github.com/stretchr/testify/require"
)

const retentionProcessEnv = "WORKSPACE_RETENTION_TEST_PROCESS"

type retentionProcessInput struct {
	Config      *model.Config
	ResultPath  string
	WorkerJobID string
}
type retentionProcessResult struct {
	PID       int
	Reason    stopReason
	Error     string
	JobStatus string
}

// Test-binary entry only; no product startup, validator or license file changes.
func runRetentionTestProcess(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var input retentionProcessInput
	if err = json.Unmarshal(data, &input); err != nil {
		return err
	}
	if err = utils.TranslationsPreInit(); err != nil {
		return err
	}
	logger, err := mlog.NewLogger()
	if err != nil {
		return err
	}
	defer logger.Shutdown()
	mem, err := config.NewMemoryStoreWithOptions(&config.MemoryStoreOptions{IgnoreEnvironmentOverrides: true})
	if err != nil {
		return err
	}
	if err = mem.Set(input.Config); err != nil {
		return err
	}
	store, err := config.NewStoreFromBacking(mem, nil, false)
	if err != nil {
		return err
	}
	defer store.Close()
	if input.WorkerJobID != "" {
		app.RegisterJobsDataRetentionJobInterface(New)
		jobs.DefaultWatcherPollingInterval = 20
	}
	server, err := app.NewServer(app.ConfigStore(store), app.SetLogger(logger), app.DisableGlobalLogger())
	if err != nil {
		return err
	}
	defer server.Shutdown()
	if !server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)) {
		return fmt.Errorf("child rejected explicit retention test license")
	}
	if err = server.Start(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if input.WorkerJobID != "" {
		if err := server.Jobs.StartWorkers(); err != nil {
			return err
		}
		defer server.Jobs.StopWorkers()
		result := retentionProcessResult{PID: os.Getpid()}
		deadline := time.Now().Add(8 * time.Second)
		for {
			job, err := server.Store().Job().Get(request.EmptyContext(logger), input.WorkerJobID)
			if err != nil {
				return err
			}
			result.JobStatus = job.Status
			if job.Status == model.JobStatusSuccess {
				break
			}
			if time.Now().After(deadline) {
				result.Error = "actual job watcher did not redispatch the crashed job: " + job.Status
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return os.WriteFile(input.ResultPath, data, 0600)
	}
	rctx := request.EmptyContext(logger).WithContext(ctx)
	reason, executionErr := New(server).MakeWorker().(*Worker).execute(rctx, logger, make(chan struct{}))
	result := retentionProcessResult{PID: os.Getpid(), Reason: reason}
	if executionErr != nil {
		result.Error = executionErr.Error()
	}
	data, err = json.Marshal(result)
	if err != nil {
		return err
	}
	return os.WriteFile(input.ResultPath, data, 0600)
}

func TestRetentionSeparateProcessCrashResumesRemainingFilePaths(t *testing.T) {
	testRetentionProcessCrash(t, false)
}

func TestRetentionJobProcessCrashIsRedispatchedByWatcher(t *testing.T) {
	testRetentionProcessCrash(t, true)
}

func testRetentionProcessCrash(t *testing.T, throughWatcher bool) {
	for _, emulator := range objectstoretest.Cases() {
		t.Run(emulator.Name, func(t *testing.T) {
			endpoint := os.Getenv(emulator.EndpointEnv)
			if endpoint == "" {
				t.Skipf("set %s for retention process recovery", emulator.EndpointEnv)
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
			var targetPath string
			var firstStatus atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce, enteredOnce sync.Once
			drain := func() { releaseOnce.Do(func() { close(release) }) }
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !armed.Load() || r.Method != http.MethodDelete || !strings.HasSuffix(r.URL.Path, "/"+targetPath) {
					proxy.ServeHTTP(w, r)
					return
				}
				buffered := httptest.NewRecorder()
				proxy.ServeHTTP(buffered, r)
				firstStatus.CompareAndSwap(0, int32(buffered.Code))
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				for key, values := range buffered.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(buffered.Code)
				_, _ = io.Copy(w, buffered.Body)
			}))
			defer front.Close()
			defer drain()
			proxyEndpoint := front.URL + endpointPath
			if emulator.Name == "s3" {
				proxyEndpoint = strings.TrimPrefix(proxyEndpoint, "http://")
			}
			settings := emulator.Setup(t, proxyEndpoint)
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
			workerJobID := ""
			if throughWatcher {
				job := &model.Job{Id: model.NewId(), Type: model.JobTypeDataRetention,
					CreateAt: model.GetMillis(), Status: model.JobStatusPending, Data: model.StringMap{"fixture": "actual-watcher"}}
				_, err := th.Store.Job().Save(job)
				require.NoError(t, err)
				workerJobID = job.Id
			}
			cfg := th.App.Config().Clone()
			cfg.ServiceSettings.ListenAddress = new("127.0.0.1:0")
			cfg.ServiceSettings.EnableLocalMode = new(false)
			cfg.JobSettings.RunJobs = new(false)
			cfg.JobSettings.RunScheduler = new(false)
			cfg.PluginSettings.Enable = new(false)
			cfg.LogSettings.EnableFile = new(false)
			cfg.LogSettings.EnableConsole = new(false)
			cfg.LogSettings.EnableDiagnostics = new(false)
			cfg.MetricsSettings.Enable = new(false)
			cfg.EmailSettings.SendEmailNotifications = new(false)
			cfg.SqlSettings.MaxOpenConns = new(10)
			cfg.SqlSettings.MaxIdleConns = new(5)
			type child struct {
				cmd                 *exec.Cmd
				done                chan error
				resultPath, logPath string
				drained             bool
			}
			start := func() *child {
				dir := t.TempDir()
				c := &child{done: make(chan error, 1), resultPath: filepath.Join(dir, "result.json"), logPath: filepath.Join(dir, "child.log")}
				data, err := json.Marshal(retentionProcessInput{Config: cfg, ResultPath: c.resultPath, WorkerJobID: workerJobID})
				require.NoError(t, err)
				path := filepath.Join(dir, "input.json")
				require.NoError(t, os.WriteFile(path, data, 0600))
				log, err := os.OpenFile(c.logPath, os.O_CREATE|os.O_WRONLY, 0600)
				require.NoError(t, err)
				c.cmd = exec.Command(os.Args[0])
				c.cmd.Env = append(os.Environ(), retentionProcessEnv+"="+path)
				c.cmd.Stdout, c.cmd.Stderr = log, log
				require.NoError(t, c.cmd.Start())
				go func() {
					err := c.cmd.Wait()
					_ = log.Close()
					c.done <- err
				}()
				t.Cleanup(func() {
					if !c.drained {
						_ = c.cmd.Process.Kill()
						select {
						case <-c.done:
						case <-time.After(10 * time.Second):
							t.Error("retention child did not terminate")
						}
					}
					if t.Failed() {
						data, _ := os.ReadFile(c.logPath)
						t.Logf("retention child log:\n%s", data)
					}
				})
				return c
			}
			targetPath = old.Path
			armed.Store(true)
			first := start()
			select {
			case <-entered:
			case err := <-first.done:
				first.drained = true
				t.Fatalf("child exited before actual deletion: %v", err)
			case <-time.After(30 * time.Second):
				t.Fatal("child did not reach object DELETE")
			}
			wantStatus := http.StatusNoContent
			if emulator.Name == "azure" {
				wantStatus = http.StatusAccepted
			}
			require.EqualValues(t, wantStatus, firstStatus.Load())
			_, err = os.Stat(first.resultPath)
			require.True(t, os.IsNotExist(err), "child must still be executing when it is killed")
			var sqlPID int
			require.NoError(t, db.QueryRow(`SELECT pid FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
            AND classid=$1::oid AND objid=$2::oid AND objsubid=1 AND granted`, retentionExecutionLock>>32, retentionExecutionLock&0xffffffff).Scan(&sqlPID))
			require.NoError(t, first.cmd.Process.Kill())
			select {
			case err := <-first.done:
				first.drained = true
				require.Error(t, err, "SIGKILL must bypass completion and graceful shutdown")
			case <-time.After(10 * time.Second):
				t.Fatal("killed child did not exit")
			}
			require.Eventually(t, func() bool {
				var count int
				return db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE pid=$1`, sqlPID).Scan(&count) == nil && count == 0
			}, 2*time.Second, 10*time.Millisecond)
			armed.Store(false)
			drain()
			count := func(file *model.FileInfo) int {
				var n int
				require.NoError(t, db.QueryRow(`SELECT count(*) FROM FileInfo WHERE Id=$1`, file.Id).Scan(&n))
				return n
			}
			exists := func(path string) bool {
				found, appErr := th.App.FileExists(path)
				require.Nil(t, appErr)
				return found
			}
			require.Equal(t, 1, count(old))
			require.False(t, exists(old.Path))
			require.True(t, exists(old.PreviewPath))
			require.True(t, exists(old.ThumbnailPath))
			require.Equal(t, 1, count(next))
			require.True(t, exists(next.Path))
			if throughWatcher {
				job, err := th.Store.Job().Get(th.Context, workerJobID)
				require.NoError(t, err)
				require.Equal(t, model.JobStatusInProgress, job.Status)
				t.Logf("job crash before redispatch: id=%s status=%s", job.Id, job.Status)
			}
			second := start()
			select {
			case err := <-second.done:
				second.drained = true
				require.NoError(t, err)
			case <-time.After(45 * time.Second):
				t.Fatal("successor child did not finish")
			}
			data, err := os.ReadFile(second.resultPath)
			require.NoError(t, err)
			var result retentionProcessResult
			require.NoError(t, json.Unmarshal(data, &result))
			require.Equal(t, second.cmd.Process.Pid, result.PID)
			require.NotEqual(t, first.cmd.Process.Pid, result.PID)
			t.Logf("actual-watcher=%t second_pid=%d job_status=%s child_error=%s", throughWatcher, result.PID, result.JobStatus, result.Error)
			require.Empty(t, result.Error)
			if throughWatcher {
				require.Equal(t, model.JobStatusSuccess, result.JobStatus)
			}
			require.Equal(t, continueWork, result.Reason)
			require.Zero(t, count(old))
			require.Zero(t, count(next))
			for _, path := range []string{old.Path, old.PreviewPath, old.ThumbnailPath, next.Path} {
				require.False(t, exists(path))
			}
			for _, file := range []*model.FileInfo{current, bookmark} {
				require.Equal(t, 1, count(file))
				require.True(t, exists(file.Path))
			}
			t.Logf("process crash recovery: killed_os_pid=%d killed_sql_pid=%d successor_os_pid=%d old_metadata_before=1 old_body_before=false remaining_previews_before=2 old_metadata_after=0 all_old_paths_after=false next_file_deleted=true protected_files_kept=true", first.cmd.Process.Pid, sqlPID, result.PID)
		})
	}
}
