package elasticsearch_indexing

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/jobs/extract_content"
	"github.com/mattermost/mattermost/server/v8/channels/store/searchlayer"
	coreES "github.com/mattermost/mattermost/server/v8/enterprise/elasticsearch/elasticsearch"
	coreOS "github.com/mattermost/mattermost/server/v8/enterprise/elasticsearch/opensearch"
	"github.com/mattermost/mattermost/server/v8/platform/services/searchengine"
	"github.com/stretchr/testify/require"
)

// Exercise bytes -> native extraction -> saved Content -> Enterprise worker ->
// native search engine -> authenticated API. No fixture supplies Content or
// writes documents directly to the engine. Cover both explicit extraction
// and the upload handler's asynchronous extraction queue.
func TestIndexingUploadedFileContentSearchAndPermissions(t *testing.T) {
	for _, mode := range []string{"explicit", "upload-queue"} {
		t.Run(mode, func(t *testing.T) { testIndexingUploadedFileContent(t, mode) })
	}
}

func TestIndexingExtractionQueueOverflowCatchupSearch(t *testing.T) {
	testIndexingUploadedFileContent(t, "queue-catchup")
}

func testIndexingUploadedFileContent(t *testing.T, mode string) {
	t.Helper()
	th, endpoint, prefix := setupIndexer(t)
	inline := mode != "explicit"
	initialConfig := th.App.Config()
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.FileSettings.ExtractContent = new(inline)
		cfg.ElasticsearchSettings.EnableFileIndexing = new(true)
		cfg.ElasticsearchSettings.EnableSearching = new(true)
		cfg.ElasticsearchSettings.RequestTimeoutSeconds = new(5)
		cfg.ElasticsearchSettings.PostIndexReplicas = new(0)
		cfg.SqlSettings.DisableDatabaseSearch = new(true)
	})
	var releaseExtraction func()
	if mode == "queue-catchup" {
		releaseExtraction = saturateExtractionQueue(t, th.Server)
	}
	channel := th.CreatePrivateChannel(t)
	const arabic = "استخراجالمستندات"
	type uploadedFile struct {
		info   *model.FileInfo
		marker string
		data   []byte
	}
	var files []uploadedFile
	for _, extension := range []string{"txt", "docx"} {
		marker := "contenttoken" + model.NewId()
		content := marker + " " + arabic
		data := []byte(content)
		if extension == "docx" {
			data = indexingDOCX(t, content)
		}
		response, _, err := th.Client.UploadFile(context.Background(), data, channel.Id, "payload."+extension)
		require.NoError(t, err)
		require.Len(t, response.FileInfos, 1)
		info := response.FileInfos[0]
		require.NotContains(t, info.Name, marker, "the search term must exist only in the file's bytes")
		_, _, err = th.Client.CreatePost(context.Background(), &model.Post{
			ChannelId: channel.Id, Message: "مرفق لاختبار استخراج المحتوى", FileIds: model.StringArray{info.Id},
		})
		require.NoError(t, err)
		stored, err := th.Store.FileInfo().Get(info.Id)
		require.NoError(t, err)
		if mode == "queue-catchup" {
			require.Empty(t, stored.Content, "an upload rejected by the full queue must stay unextracted")
		} else if inline {
			require.Eventually(t, func() bool {
				stored, err = th.Store.FileInfo().Get(info.Id)
				require.NoError(t, err)
				return strings.Contains(stored.Content, marker) && strings.Contains(stored.Content, arabic)
			}, 10*time.Second, 25*time.Millisecond, "the upload extraction queue must persist content without an explicit extraction call")
		} else {
			require.Empty(t, stored.Content, "upload must not silently pre-populate the extraction fixture")
			require.NoError(t, th.App.ExtractContentFromFileInfo(th.Context, stored))
		}
		stored, err = th.Store.FileInfo().Get(info.Id)
		require.NoError(t, err)
		if mode != "queue-catchup" {
			require.Contains(t, stored.Content, marker)
			require.Contains(t, stored.Content, arabic)
		}
		files = append(files, uploadedFile{info: stored, marker: marker, data: data})
	}
	if mode == "queue-catchup" {
		releaseExtraction()
		for _, file := range files {
			stored, err := th.Store.FileInfo().Get(file.info.Id)
			require.NoError(t, err)
			require.Empty(t, stored.Content, "draining the queue must not secretly run a rejected upload task")
		}
		scheduler := extract_content.MakeScheduler(th.Server.Jobs)
		require.True(t, scheduler.Enabled(th.App.Config()))
		disabled := th.App.Config().Clone()
		disabled.FileSettings.ExtractContent = new(false)
		require.False(t, scheduler.Enabled(disabled))
		extraction := extract_content.MakeWorker(th.Server.Jobs, th.App, th.Store)
		go extraction.Run()
		t.Cleanup(extraction.Stop)
		var previous *model.Job
		for _, processed := range []string{"2", "0"} {
			job, appErr := scheduler.ScheduleJob(th.Context, th.App.Config(), false, previous)
			require.Nil(t, appErr)
			require.Equal(t, "true", job.Data["catchup"])
			select {
			case extraction.JobChannel() <- *job:
			case <-time.After(5 * time.Second):
				t.Fatal("native extraction worker did not accept catch-up job")
			}
			require.Eventually(t, func() bool {
				var err error
				previous, err = th.Store.Job().Get(th.Context, job.Id)
				require.NoError(t, err)
				return previous.Status == model.JobStatusSuccess || previous.Status == model.JobStatusError
			}, 10*time.Second, 20*time.Millisecond)
			require.Equal(t, model.JobStatusSuccess, previous.Status)
			require.Equal(t, processed, previous.Data["processed"])
			require.Equal(t, "0", previous.Data["errors"])
		}
		for _, file := range files {
			stored, err := th.Store.FileInfo().Get(file.info.Id)
			require.NoError(t, err)
			require.Contains(t, stored.Content, file.marker)
			require.Contains(t, stored.Content, arabic)
		}
	}

	// Start the same engine implementation selected by the central native
	// registration only after extraction, so live indexing cannot mask a
	// worker that omits extracted content.
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var searches atomic.Int64
	forward := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_search") {
			searches.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(forward.Close)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ElasticsearchSettings.ConnectionURL = new(forward.URL) })
	// The core test helper supplies a prebuilt SearchStore. Synchronize its
	// config as production's config listener does, including the SQL fallback
	// prohibition; otherwise the helper retains its initial default config.
	searchStore, ok := th.Store.(*searchlayer.SearchStore)
	require.True(t, ok)
	searchStore.UpdateConfig(th.App.Config())
	var engine searchengine.SearchEngineInterface
	if *th.App.Config().ElasticsearchSettings.Backend == model.ElasticsearchSettingsOSBackend {
		engine = &coreOS.OpensearchInterfaceImpl{Platform: th.Server.Platform()}
	} else {
		engine = &coreES.ElasticsearchInterfaceImpl{Platform: th.Server.Platform()}
	}
	previousEngine := th.App.SearchEngine().ElasticsearchEngine
	th.App.SearchEngine().RegisterElasticsearchEngine(engine)
	t.Cleanup(func() {
		err := engine.Stop()
		th.App.SearchEngine().RegisterElasticsearchEngine(previousEngine)
		searchStore.UpdateConfig(initialConfig)
		require.Nil(t, err)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.Nil(t, engine.Start(ctx))
	require.Equal(t, engine.GetName(), th.App.ActiveSearchBackend())

	params := &model.SearchParameter{Terms: new(arabic), IsOrSearch: new(false)}
	before, _, err := th.Client.SearchFilesWithParams(ctx, th.BasicTeam.Id, params)
	require.NoError(t, err)
	require.Empty(t, before.Order, "neither live indexing nor SQL may supply a pre-worker result")
	require.Positive(t, searches.Load(), "the authenticated API must reach the configured search engine")

	worker := New(th.Server).MakeWorker()
	require.NotNil(t, worker)
	th.Server.Jobs.RegisterJobType(model.JobTypeElasticsearchPostIndexing, worker, nil)
	go worker.Run()
	t.Cleanup(worker.Stop)
	job := runIndexingJobWithData(t, th, worker, model.JobStatusSuccess, model.StringMap{
		"start_time": "0", "index_posts": "false", "index_channels": "false", "index_users": "false", "index_files": "true",
	})
	cp, err := loadIndexingCheckpoint(job)
	require.NoError(t, err)
	require.True(t, cp.Progress.DoneFiles)
	require.EqualValues(t, len(files), cp.Progress.DoneFilesCount)
	indexRequest(t, endpoint, http.MethodPost, "/"+prefix+"files/_refresh", "")

	for _, file := range files {
		t.Run(file.info.Extension, func(t *testing.T) {
			count := searches.Load()
			found, _, err := th.Client.SearchFilesWithParams(ctx, th.BasicTeam.Id, &model.SearchParameter{
				Terms: new(file.marker), IsOrSearch: new(false),
			})
			require.NoError(t, err)
			require.Equal(t, []string{file.info.Id}, found.Order)
			require.Greater(t, searches.Load(), count, "a SQL fallback must not stand in for a real engine query")
			data, _, err := th.Client.GetFile(ctx, file.info.Id)
			require.NoError(t, err)
			require.Equal(t, file.data, data)
		})
	}
	found, _, err := th.Client.SearchFilesWithParams(ctx, th.BasicTeam.Id, params)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{files[0].info.Id, files[1].info.Id}, found.Order)

	th.LoginBasic2(t)
	assertHidden := func() {
		t.Helper()
		found, _, err := th.Client.SearchFilesWithParams(ctx, th.BasicTeam.Id, params)
		require.NoError(t, err)
		require.Empty(t, found.Order)
		require.Empty(t, found.FileInfos)
		for _, file := range files {
			_, response, err := th.Client.GetFile(ctx, file.info.Id)
			require.Error(t, err)
			require.Equal(t, http.StatusForbidden, response.StatusCode)
		}
	}
	assertHidden()
	th.AddUserToChannel(t, th.BasicUser2, channel)
	found, _, err = th.Client.SearchFilesWithParams(ctx, th.BasicTeam.Id, params)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{files[0].info.Id, files[1].info.Id}, found.Order)
	for _, file := range files {
		data, _, err := th.Client.GetFile(ctx, file.info.Id)
		require.NoError(t, err)
		require.Equal(t, file.data, data)
	}
	_, err = th.SystemAdminClient.RemoveUserFromChannel(ctx, channel.Id, th.BasicUser2.Id)
	require.NoError(t, err)
	assertHidden()
	// Revocation must filter real indexed documents, not merely delete them.
	for _, file := range files {
		assertPostIndexed(t, endpoint, prefix+"files", file.info.Id)
	}
}

func indexingDOCX(t *testing.T, content string) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	for _, part := range []struct{ name, content string }{
		{"[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`},
		{"word/document.xml", `<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` + content + `</w:t></w:r></w:p></w:body></w:document>`},
	} {
		partWriter, err := writer.Create(part.name)
		require.NoError(t, err)
		_, err = io.WriteString(partWriter, part.content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return data.Bytes()
}
