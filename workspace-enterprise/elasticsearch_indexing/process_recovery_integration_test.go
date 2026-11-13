package elasticsearch_indexing

import (
	"encoding/json"
	"fmt"
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

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/config"
	"github.com/stretchr/testify/require"
)

const indexingProcessEnv = "WORKSPACE_INDEXING_TEST_PROCESS"

type indexingProcessInput struct {
	Config            *model.Config
	JobID, ResultPath string
}

type indexingProcessResult struct {
	PID    int
	Status string
}

// Test binary entry only. The child uses the registered job worker and the
// real JobServer watcher; the parent never manually requeues the crashed Job.
func runIndexingTestProcess(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var input indexingProcessInput
	if err := json.Unmarshal(data, &input); err != nil {
		return err
	}
	if err := utils.TranslationsPreInit(); err != nil {
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
	if err := mem.Set(input.Config); err != nil {
		return err
	}
	configuration, err := config.NewStoreFromBacking(mem, nil, false)
	if err != nil {
		return err
	}
	defer configuration.Close()
	app.RegisterJobsElasticsearchIndexerInterface(New)
	jobs.DefaultWatcherPollingInterval = 20
	server, err := app.NewServer(app.ConfigStore(configuration), app.SetLogger(logger), app.DisableGlobalLogger())
	if err != nil {
		return err
	}
	defer server.Shutdown()
	if !server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)) {
		return fmt.Errorf("child rejected explicit indexing test license")
	}
	if err := server.Start(); err != nil {
		return err
	}
	if err := server.Jobs.StartWorkers(); err != nil {
		return err
	}
	defer server.Jobs.StopWorkers()
	deadline := time.Now().Add(15 * time.Second)
	for {
		job, err := server.Store().Job().Get(request.EmptyContext(logger), input.JobID)
		if err != nil {
			return err
		}
		if job.Status == model.JobStatusSuccess || job.Status == model.JobStatusError || job.Status == model.JobStatusCanceled {
			data, err := json.Marshal(indexingProcessResult{PID: os.Getpid(), Status: job.Status})
			if err != nil {
				return err
			}
			return os.WriteFile(input.ResultPath, data, 0600)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("watcher did not complete crashed indexing Job: status=%s", job.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestIndexingJobProcessCrashIsRedispatchedByWatcher(t *testing.T) {
	testIndexingProcessRecovery(t, false)
}

func TestIndexingCanceledJobProcessCrashIsNotRedispatched(t *testing.T) {
	testIndexingProcessRecovery(t, true)
}

func testIndexingProcessRecovery(t *testing.T, cancelBeforeCrash bool) {
	t.Helper()
	th, endpoint, prefix := setupIndexer(t)
	file, err := th.Store.FileInfo().Save(th.Context, &model.FileInfo{
		CreatorId: th.BasicUser.Id, PostId: th.BasicPost.Id, ChannelId: th.BasicChannel.Id,
		Name: "crash.txt", Path: "fixture/crash.txt", Extension: "txt", MimeType: "text/plain", Content: "ملف بعد استعادة المهمة", Size: 40,
	})
	require.NoError(t, err)
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var armed atomic.Bool
	armed.Store(true)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if armed.Load() && strings.HasSuffix(r.URL.Path, "/_bulk") {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	t.Cleanup(unblock)
	job, err := th.Store.Job().Save(&model.Job{Id: model.NewId(), Type: model.JobTypeElasticsearchPostIndexing,
		CreateAt: model.GetMillis(), Status: model.JobStatusPending, Data: model.StringMap{"start_time": "0"}})
	require.NoError(t, err)
	cfg := th.App.Config().Clone()
	cfg.ServiceSettings.ListenAddress = new("127.0.0.1:0")
	cfg.ServiceSettings.EnableLocalMode = new(false)
	cfg.JobSettings.RunJobs = new(false)
	cfg.JobSettings.RunScheduler = new(false)
	cfg.PluginSettings.Enable = new(false)
	cfg.MetricsSettings.Enable = new(false)
	cfg.LogSettings.EnableFile = new(false)
	cfg.LogSettings.EnableConsole = new(false)
	cfg.LogSettings.EnableDiagnostics = new(false)
	cfg.EmailSettings.SendEmailNotifications = new(false)
	cfg.SqlSettings.MaxOpenConns = new(10)
	cfg.SqlSettings.MaxIdleConns = new(5)
	cfg.ElasticsearchSettings.ConnectionURL = new(front.URL)
	cfg.ElasticsearchSettings.RequestTimeoutSeconds = new(30)
	type child struct {
		cmd                 *exec.Cmd
		done                chan error
		drained             bool
		resultPath, logPath string
	}
	start := func() *child {
		dir := t.TempDir()
		c := &child{done: make(chan error, 1), resultPath: filepath.Join(dir, "result.json"), logPath: filepath.Join(dir, "child.log")}
		data, err := json.Marshal(indexingProcessInput{Config: cfg, JobID: job.Id, ResultPath: c.resultPath})
		require.NoError(t, err)
		path := filepath.Join(dir, "input.json")
		require.NoError(t, os.WriteFile(path, data, 0600))
		log, err := os.OpenFile(c.logPath, os.O_CREATE|os.O_WRONLY, 0600)
		require.NoError(t, err)
		c.cmd = exec.Command(os.Args[0])
		c.cmd.Env = append(os.Environ(), indexingProcessEnv+"="+path)
		c.cmd.Stdout, c.cmd.Stderr = log, log
		require.NoError(t, c.cmd.Start())
		go func() { err := c.cmd.Wait(); log.Close(); c.done <- err }()
		t.Cleanup(func() {
			if !c.drained {
				_ = c.cmd.Process.Kill()
				select {
				case <-c.done:
				case <-time.After(10 * time.Second):
					t.Error("indexing child did not terminate")
				}
			}
			if t.Failed() {
				data, _ := os.ReadFile(c.logPath)
				t.Logf("indexing child log:\n%s", data)
			}
		})
		return c
	}
	first := start()
	select {
	case <-entered:
	case err := <-first.done:
		first.drained = true
		t.Fatalf("first child exited before Bulk: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("first child did not submit Bulk")
	}
	before, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusInProgress, before.Status)
	require.Equal(t, "0", before.Data["done_posts_count"])
	require.NotEmpty(t, before.Data[indexingAttemptKey])
	key := uint64(indexingLock(job.Id))
	var sqlPID int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT pid FROM pg_locks WHERE locktype='advisory'
        AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
        AND classid=$1::oid AND objid=$2::oid AND objsubid=1 AND granted`, key>>32, key&0xffffffff).Scan(&sqlPID))
	if cancelBeforeCrash {
		require.Nil(t, th.Server.Jobs.RequestCancellation(th.Context, job.Id))
	}
	require.NoError(t, first.cmd.Process.Kill())
	select {
	case err := <-first.done:
		first.drained = true
		require.Error(t, err, "SIGKILL must bypass deferred cleanup")
	case <-time.After(10 * time.Second):
		t.Fatal("killed child did not exit")
	}
	require.Eventually(t, func() bool {
		var n int
		return th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE pid=$1`, sqlPID).Scan(&n) == nil && n == 0
	}, 3*time.Second, 10*time.Millisecond)
	armed.Store(false)
	unblock()
	second := start()
	select {
	case err := <-second.done:
		second.drained = true
		require.NoError(t, err)
	case <-time.After(45 * time.Second):
		t.Fatal("successor did not finish")
	}
	data, err := os.ReadFile(second.resultPath)
	require.NoError(t, err)
	var result indexingProcessResult
	require.NoError(t, json.Unmarshal(data, &result))
	require.Equal(t, second.cmd.Process.Pid, result.PID)
	require.NotEqual(t, first.cmd.Process.Pid, result.PID)
	want := model.JobStatusSuccess
	if cancelBeforeCrash {
		want = model.JobStatusCanceled
	}
	require.Equal(t, want, result.Status)
	final, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	if cancelBeforeCrash {
		require.Equal(t, before.Data[indexingAttemptKey], final.Data[indexingAttemptKey], "canceled job must not start another attempt")
		t.Logf("actual watcher canceled same Job %s after SIGKILL: first_pid=%d sql_pid=%d successor_pid=%d status=%s", job.Id, first.cmd.Process.Pid, sqlPID, result.PID, result.Status)
		return
	}
	require.NotEqual(t, before.Data[indexingAttemptKey], final.Data[indexingAttemptKey])
	for _, entity := range []struct{ index, id string }{
		{"posts*", th.BasicPost.Id}, {"channels", th.BasicChannel.Id}, {"users", th.BasicUser.Id}, {"files", file.Id},
	} {
		assertPostIndexed(t, endpoint, prefix+entity.index, entity.id)
	}
	t.Logf("actual watcher recovered same Job %s after SIGKILL: first_pid=%d successor_pid=%d status=%s", job.Id, first.cmd.Process.Pid, result.PID, result.Status)
}
