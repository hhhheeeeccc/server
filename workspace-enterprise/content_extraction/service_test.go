package content_extraction_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/mattermost/enterprise"
	"github.com/mattermost/enterprise/content_extraction"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if path := os.Getenv(extractionProcessEnv); path != "" {
		if err := runExtractionProcess(path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	dsn := os.Getenv("TEST_DATABASE_POSTGRESQL_DSN")
	if dsn == "" {
		os.Exit(m.Run())
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Hostname() != "127.0.0.1" {
		fmt.Fprintln(os.Stderr, "content extraction tests require an isolated loopback PostgreSQL DSN")
		os.Exit(2)
	}
	// Same test-only watcher acceleration used by the native jobs suite.
	jobs.DefaultWatcherPollingInterval = 100
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func setup(t *testing.T) *api4.TestHelper {
	return setupWithConfig(t, nil)
}

func setupWithConfig(t *testing.T, update func(*model.Config)) *api4.TestHelper {
	t.Helper()
	if os.Getenv("TEST_DATABASE_POSTGRESQL_DSN") == "" {
		t.Skip("requires isolated PostgreSQL; use the audit runner")
	}
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.EmailSettings.SendPushNotifications = new(false)
		cfg.FileSettings.ExtractContent = new(false)
		cfg.JobSettings.RunJobs = new(false)
		cfg.JobSettings.RunScheduler = new(false)
		if update != nil {
			update(cfg)
		}
	}).InitBasic(t)
	require.NotNil(t, th.Server.IPFiltering, "the post-jobs registration constructor must retain IP filtering")
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	require.NoError(t, th.Server.Jobs.StartWorkers())
	t.Cleanup(func() {
		err := th.Server.Jobs.StopWorkers()
		if !errors.Is(err, jobs.ErrWorkersNotRunning) {
			require.NoError(t, err)
		}
	})
	return th
}

func upload(t *testing.T, th *api4.TestHelper, content, name string, created time.Time) *model.FileInfo {
	t.Helper()
	info, appErr := th.App.UploadFileX(th.Context, th.BasicChannel.Id, name, strings.NewReader(content),
		app.UploadFileSetUserId(th.BasicUser.Id), app.UploadFileSetTeamId(th.BasicTeam.Id),
		app.UploadFileSetTimestamp(created), app.UploadFileSetContentLength(int64(len(content))))
	require.Nil(t, appErr)
	require.Equal(t, created.UnixMilli(), info.CreateAt)
	require.Empty(t, info.Content)
	return info
}

// Only the central root registration installs the integration worker. These
// fixtures do not call Register, MakeWorker, Run or JobChannel: the real watcher
// dispatches their jobs. A separate unit test covers idle lifecycle ordering.
func run(t *testing.T, th *api4.TestHelper, data model.StringMap, status string) *model.Job {
	t.Helper()
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, data)
	require.Nil(t, appErr)
	var stored *model.Job
	require.Eventually(t, func() bool {
		var err error
		stored, err = th.Store.Job().Get(th.Context, job.Id)
		require.NoError(t, err)
		return stored.Status == model.JobStatusSuccess || stored.Status == model.JobStatusError
	}, 60*time.Second, 50*time.Millisecond)
	require.Equal(t, status, stored.Status, "job data: %v", stored.Data)
	return stored
}

func get(t *testing.T, th *api4.TestHelper, id string) *model.FileInfo {
	t.Helper()
	info, err := th.Store.FileInfo().GetFromMaster(id)
	require.NoError(t, err)
	return info
}

func TestRegisteredExtractionPreservesTiedBatchBoundaries(t *testing.T) {
	for _, mode := range []string{"catchup-tied", "range-tied", "catchup-distinct"} {
		t.Run(mode, func(t *testing.T) {
			th := setup(t)
			const total = 1002
			created := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
			ids := make([]string, 0, total)
			for i := range total {
				stamp := created
				if mode == "catchup-distinct" {
					stamp = created.Add(time.Duration(i) * time.Millisecond)
				}
				info := upload(t, th, fmt.Sprintf("boundarytoken%04d", i), "boundary.txt", stamp)
				ids = append(ids, info.Id)
			}
			data := model.StringMap{"catchup": "true"}
			if mode == "range-tied" {
				data = model.StringMap{"from": fmt.Sprint(created.Add(-time.Second).Unix()), "to": fmt.Sprint(time.Now().Unix())}
			} else {
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings.ExtractContent = new(true) })
			}
			job := run(t, th, data, model.JobStatusSuccess)
			require.Equal(t, fmt.Sprint(total), job.Data["processed"])
			require.Equal(t, "0", job.Data["errors"])
			for i, id := range ids {
				require.Contains(t, get(t, th, id).Content, fmt.Sprintf("boundarytoken%04d", i))
			}
			if mode != "range-tied" {
				again := run(t, th, model.StringMap{"catchup": "true"}, model.JobStatusSuccess)
				require.Equal(t, "0", again.Data["processed"])
			}
		})
	}
}

func TestRegisteredExtractionRangeBoundsDeletionAndIgnoredFiles(t *testing.T) {
	th := setup(t)
	created := time.Now().Add(-time.Minute).Truncate(time.Second)
	before := upload(t, th, "before", "before.txt", created.Add(-time.Second))
	inside := upload(t, th, "inside", "inside.txt", created)
	after := upload(t, th, "after", "after.txt", created.Add(time.Second))
	deleted := upload(t, th, "deleted", "deleted.txt", created)
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE fileinfo SET deleteat=$1 WHERE id=$2`, model.GetMillis(), deleted.Id)
	require.NoError(t, err)
	ignored := upload(t, th, "ignored", "ignored.mp4", created)
	job := run(t, th, model.StringMap{"from": fmt.Sprint(created.Unix()), "to": fmt.Sprint(created.Unix())}, model.JobStatusSuccess)
	require.Equal(t, "1", job.Data["processed"])
	require.Equal(t, "inside", get(t, th, inside.Id).Content)
	for _, info := range []*model.FileInfo{before, after, ignored} {
		require.Empty(t, get(t, th, info.Id).Content)
	}
	var deletedContent string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT content FROM fileinfo WHERE id=$1`, deleted.Id).Scan(&deletedContent))
	require.Empty(t, deletedContent)
}

func TestRegisteredExtractionHonorsLicenseAndRuntimeGrant(t *testing.T) {
	th := setup(t)
	info := upload(t, th, "licensed contents", "license.txt", time.Now())
	th.Server.SetLicense(nil)
	job := run(t, th, nil, model.JobStatusError)
	require.Equal(t, "0", job.Data["processed"])
	require.Empty(t, get(t, th, info.Id).Content)
	scheduler := content_extraction.New(th.App).MakeScheduler()
	require.False(t, scheduler.Enabled(th.App.Config()))
	_, appErr := scheduler.ScheduleJob(th.Context, th.App.Config(), false, nil)
	require.NotNil(t, appErr)
	require.Equal(t, 403, appErr.StatusCode)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	job = run(t, th, nil, model.JobStatusSuccess)
	require.Equal(t, "1", job.Data["processed"])
	require.Equal(t, "licensed contents", get(t, th, info.Id).Content)
}

func TestRegisteredExtractionStorageFailureIsNotReportedAsSuccess(t *testing.T) {
	th := setup(t)
	good := upload(t, th, "available", "good.txt", time.Now())
	missing := upload(t, th, "missing", "missing.txt", time.Now())
	require.Nil(t, th.App.RemoveFile(missing.Path))
	job := run(t, th, nil, model.JobStatusError)
	require.Equal(t, "2", job.Data["processed"])
	require.Equal(t, "1", job.Data["errors"])
	require.Equal(t, "available", get(t, th, good.Id).Content)
	require.Empty(t, get(t, th, missing.Id).Content)
}

func TestRegisteredExtractionValidatesRangeAndAutomaticEnablement(t *testing.T) {
	th := setup(t)
	info := upload(t, th, "not yet", "validation.txt", time.Now())
	for _, data := range []model.StringMap{
		{"from": "invalid"}, {"from": "2", "to": "1"}, {"to": "-1"}, {"from": "9223372036854775807"}, {"catchup": "true"},
	} {
		job := run(t, th, data, model.JobStatusError)
		require.Equal(t, "0", job.Data["processed"])
		require.Empty(t, get(t, th, info.Id).Content)
	}
	old := upload(t, th, "older than catchup", "old.txt", time.Now().Add(-25*time.Hour))
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings.ExtractContent = new(true) })
	scheduler := content_extraction.New(th.App).MakeScheduler()
	require.True(t, scheduler.Enabled(th.App.Config()))
	job := run(t, th, model.StringMap{"catchup": "true"}, model.JobStatusSuccess)
	require.Equal(t, "1", job.Data["processed"])
	require.Equal(t, "not yet", get(t, th, info.Id).Content)
	require.Empty(t, get(t, th, old.Id).Content)
	_, _, err := th.Client.GetFile(context.Background(), info.Id)
	require.NoError(t, err)
}
