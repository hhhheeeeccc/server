package autotranslation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func TestRecoveryWorkerRetriesPersistedFailures(t *testing.T) {
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
			return
		}
		_, _ = w.Write([]byte(`{"translatedText":"سيبدأ اجتماع المشروع غدًا."}`))
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	post := seedRecoveryPost(t, th, "The project meeting starts tomorrow.", model.TranslationStateUnavailable)
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET meta='{}' WHERE objectid=$1`, post.Id)
	require.NoError(t, err)

	// A new service has no memory of the failed request. The database is the queue.
	runRecoveryWorker(t, th, New(th.Server))
	_, valid := recoveryIndexIdentity(t, th.Store.GetInternalMasterDB())
	require.True(t, valid, "the actual worker must install the recovery index")
	translation, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, model.TranslationStateReady, translation.State)
	require.Equal(t, model.TranslationTypeString, translation.Type, "recovery must restore missing legacy type metadata")
	require.Equal(t, "سيبدأ اجتماع المشروع غدًا.", translation.Text)
	require.Positive(t, requests.Load())
}

func TestRecoveryBackoffPersistsAcrossServices(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	post := seedRecoveryPost(t, th, "The office reopens tomorrow.", model.TranslationStateProcessing)
	runRecoveryWorker(t, th, New(th.Server))
	var attempts int
	var nextAt int64
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT (meta#>>'{workspace_retry,attempts}')::int,
		(meta#>>'{workspace_retry,next_at}')::bigint FROM translations WHERE objectid=$1`, post.Id).Scan(&attempts, &nextAt))
	require.Equal(t, 1, attempts)
	require.WithinDuration(t, time.Now().Add(10*time.Minute), time.UnixMilli(nextAt), 2*time.Second)
	require.EqualValues(t, 1, calls.Load(), "failed detection must not generate another translation request")
	// Aging UpdateAt cannot defeat the persisted retry delay after a restart.
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET updateat=$1 WHERE objectid=$2`, time.Now().Add(-time.Hour).UnixMilli(), post.Id)
	require.NoError(t, err)
	runRecoveryWorker(t, th, New(th.Server))
	require.EqualValues(t, 1, calls.Load())
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET meta=jsonb_set(meta,'{workspace_retry,next_at}','0') WHERE objectid=$1`, post.Id)
	require.NoError(t, err)
	runRecoveryWorker(t, th, New(th.Server))
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT (meta#>>'{workspace_retry,attempts}')::int,
		(meta#>>'{workspace_retry,next_at}')::bigint FROM translations WHERE objectid=$1`, post.Id).Scan(&attempts, &nextAt))
	require.Equal(t, 2, attempts)
	require.WithinDuration(t, time.Now().Add(20*time.Minute), time.UnixMilli(nextAt), 2*time.Second)
	require.EqualValues(t, 2, calls.Load())
}

func TestRecoveryConcurrentWorkersAndExpiredLease(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	releaseProvider := sync.OnceFunc(func() { close(release) })
	defer releaseProvider()
	var translations atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
			return
		}
		if translations.Add(1) == 1 {
			close(started)
			<-release
			_, _ = w.Write([]byte(`{"translatedText":"old delayed response"}`))
			return
		}
		_, _ = w.Write([]byte(`{"translatedText":"fresh lease response"}`))
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	post := seedRecoveryPost(t, th, "The meeting starts tomorrow.", model.TranslationStateUnavailable)
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeAutoTranslationRecovery, nil)
	require.Nil(t, appErr)
	done := make(chan struct{})
	go func() {
		defer close(done)
		New(th.Server).MakeWorker().(interface{ DoJob(*model.Job) }).DoJob(job)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		releaseProvider()
		t.Fatal("first worker did not reach the provider")
	}
	// A second service shares the primary queue but cannot take a live lease.
	runRecoveryWorker(t, th, New(th.Server))
	require.EqualValues(t, 1, translations.Load())
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET updateat=$1 WHERE objectid=$2`, time.Now().Add(-time.Hour).UnixMilli(), post.Id)
	require.NoError(t, err)
	runRecoveryWorker(t, th, New(th.Server))
	require.EqualValues(t, 2, translations.Load())
	releaseProvider()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old worker did not finish")
	}
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, model.TranslationStateReady, stored.State)
	require.Equal(t, "fresh lease response", stored.Text, "an expired lease must not overwrite the new result")
}

func TestRecoveryDiscardsResultAfterSourceChanges(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	releaseProvider := sync.OnceFunc(func() { close(release) })
	defer releaseProvider()
	var calls atomic.Int32
	var receivedText atomic.Value
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
			return
		}
		var body translateRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		receivedText.Store(body.Text)
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		_, _ = w.Write([]byte(`{"translatedText":"translated current message"}`))
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	post := seedRecoveryPost(t, th, "Original meeting tomorrow.", model.TranslationStateUnavailable)
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeAutoTranslationRecovery, nil)
	require.Nil(t, appErr)
	done := make(chan struct{})
	go func() { defer close(done); New(th.Server).MakeWorker().(interface{ DoJob(*model.Job) }).DoJob(job) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		releaseProvider()
		t.Fatal("provider not reached")
	}
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET message='Updated meeting next week.' WHERE id=$1`, post.Id)
	require.NoError(t, err)
	releaseProvider()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker not finished")
	}
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.NotEqual(t, model.TranslationStateReady, stored.State, "late translation for the old text must be discarded")
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET updateat=$1 WHERE objectid=$2`, time.Now().Add(-time.Hour).UnixMilli(), post.Id)
	require.NoError(t, err)
	runRecoveryWorker(t, th, New(th.Server))
	require.Equal(t, "Updated meeting next week.", receivedText.Load())
	stored, err = th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, model.TranslationStateReady, stored.State)
	digest := sha256.Sum256([]byte("Updated meeting next week."))
	require.Equal(t, hex.EncodeToString(digest[:]), stored.NormHash)
}

func TestRecoveryHonorsCurrentEligibility(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	post := seedRecoveryPost(t, th, "Private synthetic message.", model.TranslationStateUnavailable)
	db := th.Store.GetInternalMasterDB()
	for _, test := range []struct {
		name, disable, restore string
		id                     string
	}{
		{"deleted_post", `UPDATE posts SET deleteat=1 WHERE id=$1`, `UPDATE posts SET deleteat=0 WHERE id=$1`, post.Id},
		{"archived_channel", `UPDATE channels SET deleteat=1 WHERE id=$1`, `UPDATE channels SET deleteat=0 WHERE id=$1`, post.ChannelId},
		{"disabled_channel", `UPDATE channels SET autotranslation=false WHERE id=$1`, `UPDATE channels SET autotranslation=true WHERE id=$1`, post.ChannelId},
		{"opted_out", `UPDATE channelmembers SET autotranslationdisabled=true WHERE userid=$1`, `UPDATE channelmembers SET autotranslationdisabled=false WHERE userid=$1`, th.BasicUser.Id},
		{"inactive_reader", `UPDATE users SET deleteat=1 WHERE id=$1`, `UPDATE users SET deleteat=0 WHERE id=$1`, th.BasicUser.Id},
		{"locale_changed", `UPDATE users SET locale='en' WHERE id=$1`, `UPDATE users SET locale='ar' WHERE id=$1`, th.BasicUser.Id},
		{"unsupported_post", `UPDATE posts SET type='system_join_channel' WHERE id=$1`, `UPDATE posts SET type='' WHERE id=$1`, post.Id},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := db.Exec(test.disable, test.id)
			require.NoError(t, err)
			defer func() { _, err := db.Exec(test.restore, test.id); require.NoError(t, err) }()
			runRecoveryWorker(t, th, New(th.Server))
			require.Zero(t, calls.Load())
		})
	}
	t.Run("license_removed", func(t *testing.T) {
		license := th.Server.License()
		th.Server.SetLicense(nil)
		runRecoveryWorker(t, th, New(th.Server))
		require.Zero(t, calls.Load())
		require.True(t, th.Server.SetLicense(license))
	})
	t.Run("configuration_disabled", func(t *testing.T) {
		th.App.UpdateConfig(func(cfg *model.Config) { cfg.AutoTranslationSettings.Enable = new(false) })
		defer th.App.UpdateConfig(func(cfg *model.Config) { cfg.AutoTranslationSettings.Enable = new(true) })
		runRecoveryWorker(t, th, New(th.Server))
		require.Zero(t, calls.Load())
	})
	t.Run("feature_flag_disabled", func(t *testing.T) {
		th.App.UpdateConfig(func(cfg *model.Config) { cfg.FeatureFlags.AutoTranslation = false })
		defer th.App.UpdateConfig(func(cfg *model.Config) { cfg.FeatureFlags.AutoTranslation = true })
		runRecoveryWorker(t, th, New(th.Server))
		require.Zero(t, calls.Load())
	})
	t.Run("member_removed", func(t *testing.T) {
		_, err := db.Exec(`DELETE FROM channelmembers WHERE channelid=$1 AND userid=$2`, post.ChannelId, th.BasicUser.Id)
		require.NoError(t, err)
		runRecoveryWorker(t, th, New(th.Server))
		require.Zero(t, calls.Load())
	})
}

func TestRecoveryOptOutDuringDetectionStopsTranslation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	releaseProvider := sync.OnceFunc(func() { close(release) })
	defer releaseProvider()
	var translations atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/detect" {
			close(started)
			<-release
			_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
			return
		}
		translations.Add(1)
		_, _ = w.Write([]byte(`{"translatedText":"must not be requested"}`))
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	seedRecoveryPost(t, th, "Synthetic opt-out message.", model.TranslationStateUnavailable)
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeAutoTranslationRecovery, nil)
	require.Nil(t, appErr)
	done := make(chan struct{})
	go func() { defer close(done); New(th.Server).MakeWorker().(interface{ DoJob(*model.Job) }).DoJob(job) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		releaseProvider()
		t.Fatal("detector not reached")
	}
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE channelmembers SET autotranslationdisabled=true WHERE userid=$1`, th.BasicUser.Id)
	require.NoError(t, err)
	releaseProvider()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker not finished")
	}
	require.Zero(t, translations.Load())
}

func TestRecoveryRevocationAndStopDiscardPendingResults(t *testing.T) {
	for _, action := range []string{"revoke_license", "stop_worker", "shutdown_service"} {
		t.Run(action, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			releaseProvider := sync.OnceFunc(func() { close(release) })
			defer releaseProvider()
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/detect" {
					_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
					return
				}
				close(started)
				select {
				case <-release:
					_, _ = w.Write([]byte(`{"translatedText":"late reply"}`))
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(provider.Close)
			th := setupRecovery(t, provider.URL)
			post := seedRecoveryPost(t, th, "Pending synthetic message.", model.TranslationStateUnavailable)
			s := New(th.Server)
			worker := s.MakeWorker().(*recoveryWorker)
			stopWorker := sync.OnceFunc(worker.Stop)
			go worker.Run()
			defer func() { releaseProvider(); stopWorker() }()
			job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeAutoTranslationRecovery, nil)
			require.Nil(t, appErr)
			worker.JobChannel() <- *job
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("provider not reached")
			}
			if action == "revoke_license" {
				th.Server.SetLicense(nil)
				releaseProvider()
				// Let inference finish before stopping the worker, so cancellation
				// cannot hide a missing license check in the final write.
				require.Eventually(t, func() bool {
					stored, err := th.Store.Job().Get(th.Context, job.Id)
					return err == nil && stored.Status == model.JobStatusSuccess
				}, 3*time.Second, 10*time.Millisecond)
			} else if action == "shutdown_service" {
				require.NoError(t, s.Shutdown())
				require.Eventually(t, func() bool {
					stored, err := th.Store.Job().Get(th.Context, job.Id)
					return err == nil && stored.Status == model.JobStatusError
				}, 3*time.Second, 10*time.Millisecond)
			}
			done := make(chan struct{})
			go func() { defer close(done); stopWorker() }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				releaseProvider()
				t.Fatal("worker stop did not cancel inference")
			}
			stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
			require.NoError(t, err)
			require.NotEqual(t, model.TranslationStateReady, stored.State)
		})
	}
}

func TestRecoveryBackoffIsBounded(t *testing.T) {
	require.Equal(t, 10*time.Minute, recoveryDelay(1))
	require.Equal(t, 20*time.Minute, recoveryDelay(2))
	require.Equal(t, 6*time.Hour, recoveryDelay(1000000))
}

func TestRecoveryWorkerConfigurationAndRestart(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/detect" {
			_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
			return
		}
		_, _ = w.Write([]byte(`{"translatedText":"translated after restart"}`))
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	t.Run("uses_supplied_configuration", func(t *testing.T) {
		worker := New(th.Server).MakeWorker()
		enabled := th.Server.Config().Clone()
		disabled := enabled.Clone()
		disabled.AutoTranslationSettings.Enable = new(false)
		require.True(t, worker.IsEnabled(enabled))
		require.False(t, worker.IsEnabled(disabled), "the job manager compares old and new configuration")
	})
	t.Run("same_worker_runs_again", func(t *testing.T) {
		worker := New(th.Server).MakeWorker()
		running := false
		defer func() {
			if running {
				worker.Stop()
			}
		}()
		for range 2 {
			post := seedRecoveryPost(t, th, "Synthetic restart message.", model.TranslationStateUnavailable)
			go worker.Run()
			running = true
			job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeAutoTranslationRecovery, nil)
			require.Nil(t, appErr)
			worker.JobChannel() <- *job
			require.Eventually(t, func() bool {
				stored, err := th.Store.Job().Get(th.Context, job.Id)
				return err == nil && (stored.Status == model.JobStatusSuccess || stored.Status == model.JobStatusError)
			}, 3*time.Second, 10*time.Millisecond)
			storedJob, err := th.Store.Job().Get(th.Context, job.Id)
			require.NoError(t, err)
			require.Equal(t, model.JobStatusSuccess, storedJob.Status, "%v", storedJob.Data)
			stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
			require.NoError(t, err)
			require.Equal(t, model.TranslationStateReady, stored.State)
			worker.Stop()
			running = false
		}
	})
}

func setupRecovery(t *testing.T, providerURL string) *api4.TestHelper {
	t.Helper()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.EmailSettings.SendPushNotifications = new(false)
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.AutoTranslation = new(true)
	require.True(t, th.Server.SetLicense(license))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.FeatureFlags.AutoTranslation = true
		cfg.AutoTranslationSettings.Enable = new(true)
		cfg.AutoTranslationSettings.Provider = new(providerName)
		cfg.AutoTranslationSettings.LibreTranslate.URL = new(providerURL)
	})
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE users SET locale='ar' WHERE id=$1`, th.BasicUser.Id)
	require.NoError(t, err)
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE channels SET autotranslation=true WHERE id=$1`, th.BasicChannel.Id)
	require.NoError(t, err)
	return th
}

func seedRecoveryPost(t *testing.T, th *api4.TestHelper, text string, state model.TranslationState) *model.Post {
	t.Helper()
	post, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: th.BasicChannel.Id, UserId: th.BasicUser.Id, Message: text})
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(text))
	require.NoError(t, th.Store.AutoTranslation().Save(&model.Translation{
		ObjectType: model.TranslationObjectTypePost, ObjectID: post.Id, ChannelID: post.ChannelId,
		Lang: "ar", Provider: providerName, Type: model.TranslationTypeString,
		NormHash: hex.EncodeToString(digest[:]), State: state,
	}))
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET updateat=$1 WHERE objectid=$2`, time.Now().Add(-time.Hour).UnixMilli(), post.Id)
	require.NoError(t, err)
	return post
}

func runRecoveryWorker(t *testing.T, th *api4.TestHelper, service *Service) {
	t.Helper()
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeAutoTranslationRecovery, nil)
	require.Nil(t, appErr)
	worker := service.MakeWorker().(interface{ DoJob(*model.Job) })
	worker.DoJob(job)
	stored, err := th.Store.Job().Get(th.Context, job.Id)
	require.NoError(t, err)
	require.Equal(t, model.JobStatusSuccess, stored.Status)
}
