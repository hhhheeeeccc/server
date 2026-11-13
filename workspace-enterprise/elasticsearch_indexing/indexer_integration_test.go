package elasticsearch_indexing

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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

const testElasticsearchURLEnv = "WORKSPACE_INDEXING_TEST_URL"
const testIndexingBackendEnv = "WORKSPACE_INDEXING_TEST_BACKEND"

func TestMain(m *testing.M) {
	if path := os.Getenv(indexingProcessEnv); path != "" {
		if err := runIndexingTestProcess(path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv(testElasticsearchURLEnv) == "" || os.Getenv("TEST_DATABASE_POSTGRESQL_DSN") == "" {
		os.Exit(m.Run())
	}
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

// This test uses one real worker for all three jobs. Creating a fresh worker for
// the recovery job would hide failures leaking from the preceding job.
func TestIndexingJobRecoversAfterBulkFailures(t *testing.T) {
	th, endpoint, prefix := setupIndexer(t)
	worker := New(th.Server).MakeWorker()
	require.NotNil(t, worker)
	th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, worker, nil)
	go worker.Run()
	t.Cleanup(worker.Stop)

	runIndexingJob(t, th, worker, model.JobStatusSuccess)
	assertPostIndexed(t, endpoint, prefix, th.BasicPost.Id)
	indexRequest(t, endpoint, http.MethodPut, "/"+prefix+"*/_settings", `{"index.blocks.write":true}`)
	runIndexingJob(t, th, worker, model.JobStatusError)
	indexRequest(t, endpoint, http.MethodPut, "/"+prefix+"*/_settings", `{"index.blocks.write":false}`)

	post, err := th.Store.Post().Save(th.Context, &model.Post{
		ChannelId: th.BasicChannel.Id, UserId: th.BasicUser.Id,
		Message: "فهرسة جديدة بعد تعافي محرك البحث",
	})
	require.NoError(t, err)
	runIndexingJob(t, th, worker, model.JobStatusSuccess)
	assertPostIndexed(t, endpoint, prefix, post.Id)
}

func TestIndexingMissingDocumentDeleteDoesNotPoisonNextJob(t *testing.T) {
	th, endpoint, prefix := setupIndexer(t)
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET deleteat=$1 WHERE id=$2`, model.GetMillis(), th.BasicPost.Id)
	require.NoError(t, err)
	worker := New(th.Server).MakeWorker()
	require.NotNil(t, worker)
	th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, worker, nil)
	go worker.Run()
	t.Cleanup(worker.Stop)
	runIndexingJob(t, th, worker, model.JobStatusSuccess)
	runIndexingJob(t, th, worker, model.JobStatusSuccess)

	response := indexRequest(t, endpoint, http.MethodPost, "/"+prefix+"*/_search", `{"query":{"ids":{"values":["`+th.BasicPost.Id+`"]}}}`)
	require.EqualValues(t, 0, response["hits"].(map[string]any)["total"].(map[string]any)["value"])
}

func TestIndexingJobRejectsIncompleteBulkAcknowledgements(t *testing.T) {
	th, endpoint, prefix := setupIndexer(t)
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var omitResults atomic.Bool
	omitResults.Store(true)
	fault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if omitResults.Load() && strings.HasSuffix(r.URL.Path, "/_bulk") {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			_, _ = io.WriteString(w, `{"errors":false,"items":[]}`)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(fault.Close)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.ConnectionURL = new(fault.URL) })
	worker := New(th.Server).MakeWorker()
	require.NotNil(t, worker)
	th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, worker, nil)
	go worker.Run()
	t.Cleanup(worker.Stop)
	runIndexingJob(t, th, worker, model.JobStatusError)
	omitResults.Store(false)
	runIndexingJob(t, th, worker, model.JobStatusSuccess)
	assertPostIndexed(t, endpoint, prefix, th.BasicPost.Id)
}

func TestIndexingJobAcknowledgesAllEntityTypes(t *testing.T) {
	th, endpoint, prefix := setupIndexer(t)
	file, err := th.Store.FileInfo().Save(th.Context, &model.FileInfo{
		CreatorId: th.BasicUser.Id, PostId: th.BasicPost.Id, ChannelId: th.BasicChannel.Id,
		Name: "indexing.txt", Path: "fixture/indexing.txt", Extension: "txt", MimeType: "text/plain",
		Content: "محتوى ملف مفهرس", Size: 32,
	})
	require.NoError(t, err)
	orphan, err := th.Store.Post().Save(th.Context, &model.Post{
		ChannelId: th.BasicChannel.Id, UserId: th.BasicUser.Id, Message: "قناة حذفت نهائيًا",
	})
	require.NoError(t, err)
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET channelid=$1 WHERE id=$2`, model.NewId(), orphan.Id)
	require.NoError(t, err)
	worker := New(th.Server).MakeWorker()
	require.NotNil(t, worker)
	th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, worker, nil)
	go worker.Run()
	t.Cleanup(worker.Stop)
	runIndexingJobWithData(t, th, worker, model.JobStatusSuccess, map[string]string{"start_time": "0"})
	for _, entity := range []struct{ index, id string }{
		{"posts*", th.BasicPost.Id}, {"channels", th.BasicChannel.Id}, {"users", th.BasicUser.Id}, {"files", file.Id},
	} {
		t.Run(entity.index, func(t *testing.T) { assertPostIndexed(t, endpoint, prefix+entity.index, entity.id) })
	}
	response := indexRequest(t, endpoint, http.MethodPost, "/"+prefix+"posts*/_search", `{"query":{"ids":{"values":["`+orphan.Id+`"]}}}`)
	require.Empty(t, response["hits"].(map[string]any)["hits"])
}

func TestIndexingWorkerRequiresLicenseAndConfiguration(t *testing.T) {
	th, _, _ := setupIndexer(t)
	worker := New(th.Server).MakeWorker()
	require.NotNil(t, worker)
	th.Server.SetLicense(nil)
	require.False(t, worker.IsEnabled(th.App.Config()))
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.Elasticsearch = new(false)
	require.True(t, th.Server.SetLicense(license))
	require.False(t, worker.IsEnabled(th.App.Config()))
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	require.True(t, worker.IsEnabled(th.App.Config()))
	config := th.App.Config().Clone()
	config.ElasticsearchSettings.EnableIndexing = new(false)
	require.False(t, worker.IsEnabled(config))
}

func setupIndexer(t *testing.T) (*api4.TestHelper, string, string) {
	t.Helper()
	endpoint := os.Getenv(testElasticsearchURLEnv)
	if endpoint == "" || os.Getenv("TEST_DATABASE_POSTGRESQL_DSN") == "" {
		t.Skip("requires an isolated PostgreSQL DSN and WORKSPACE_INDEXING_TEST_URL")
	}
	u, err := url.Parse(endpoint)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", u.Hostname(), "integration fixtures must be bound to loopback")
	require.Equal(t, "http", u.Scheme)
	backend := os.Getenv(testIndexingBackendEnv)
	if backend == "" {
		backend = model.ElasticsearchSettingsESBackend
	}
	require.Contains(t, []string{model.ElasticsearchSettingsESBackend, model.ElasticsearchSettingsOSBackend}, backend)
	prefix := "workspace_recovery_" + model.NewId() + "_"
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.EmailSettings.SendPushNotifications = new(false)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ElasticsearchSettings.Backend = new(backend)
		cfg.ElasticsearchSettings.ConnectionURL = new(endpoint)
		cfg.ElasticsearchSettings.IndexPrefix = new(prefix)
		cfg.ElasticsearchSettings.EnableIndexing = new(true)
		cfg.ElasticsearchSettings.Sniff = new(false)
		cfg.ElasticsearchSettings.BatchSize = new(20)
		cfg.ElasticsearchSettings.RequestTimeoutSeconds = new(2)
	})
	t.Cleanup(func() {
		indices := indexRequest(t, endpoint, http.MethodGet, "/"+prefix+"*", "")
		for name := range indices {
			require.True(t, strings.HasPrefix(name, prefix))
			indexRequest(t, endpoint, http.MethodDelete, "/"+name, "")
		}
	})
	return th, endpoint, prefix
}

func runIndexingJob(t *testing.T, th *api4.TestHelper, worker model.Worker, want string) *model.Job {
	t.Helper()
	return runIndexingJobWithData(t, th, worker, want, map[string]string{
		"index_posts": "true", "index_channels": "false", "index_users": "false", "index_files": "false", "start_time": "0",
	})
}

func runIndexingJobWithData(t *testing.T, th *api4.TestHelper, worker model.Worker, want string, data map[string]string) *model.Job {
	t.Helper()
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeElasticsearchPostIndexing, data)
	require.Nil(t, appErr)
	select {
	case worker.JobChannel() <- *job:
	case <-time.After(10 * time.Second):
		t.Fatal("indexer did not accept the job")
	}
	var stored *model.Job
	require.Eventually(t, func() bool {
		var err error
		stored, err = th.Store.Job().Get(th.Context, job.Id)
		require.NoError(t, err)
		return stored.Status == model.JobStatusSuccess || stored.Status == model.JobStatusError || stored.Status == model.JobStatusCanceled
	}, 30*time.Second, 25*time.Millisecond)
	require.Equal(t, want, stored.Status, "job %s: %v", stored.Id, stored.Data)
	return stored
}

func assertPostIndexed(t *testing.T, endpoint, prefix, postID string) {
	t.Helper()
	indexRequest(t, endpoint, http.MethodPost, "/"+prefix+"*/_refresh", "")
	response := indexRequest(t, endpoint, http.MethodPost, "/"+prefix+"*/_search", `{"query":{"ids":{"values":["`+postID+`"]}}}`)
	hits := response["hits"].(map[string]any)["hits"].([]any)
	require.Len(t, hits, 1)
	require.Equal(t, postID, hits[0].(map[string]any)["_id"])
}

func indexRequest(t *testing.T, endpoint, method, path, body string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(endpoint, "/")+path, bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	require.NoError(t, err)
	require.Less(t, res.StatusCode, 300, "%s %s: %s", method, path, data)
	var result map[string]any
	require.NoError(t, json.Unmarshal(data, &result))
	return result
}
