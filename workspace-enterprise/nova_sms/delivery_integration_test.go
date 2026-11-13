//go:build integration

package nova_sms

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

type novaFixture struct {
	th             *api4.TestHelper
	s              *Service
	cfg            deliverySettings
	channel        *model.Channel
	requests       atomic.Int32
	providerStatus atomic.Int32
	transactions   sync.Map
	afterRequest   func()
}

func setupNova(t *testing.T) *novaFixture {
	t.Helper()
	f := &novaFixture{}
	f.providerStatus.Store(http.StatusCreated)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.URL.Path != "/v1/events/trigger" || r.Header.Get("Authorization") != "ApiKey temporary-test-key" {
			t.Error("incorrect Nova request path or authorization")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var input struct {
			To struct {
				SubscriberID string `json:"subscriberId"`
				Phone        string `json:"phone"`
			} `json:"to"`
			Payload       map[string]string `json:"payload"`
			TransactionID string            `json:"transactionId"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.To.SubscriberID != f.th.BasicUser2.Id ||
			input.To.Phone != "+967737328812" || input.Payload["message"] != "رسالة عاجلة للاختبار" {
			t.Error("incorrect Nova recipient or message")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.transactions.Store(input.TransactionID, true)
		if f.afterRequest != nil {
			f.afterRequest()
		}
		w.WriteHeader(int(f.providerStatus.Load()))
	}))
	t.Cleanup(provider.Close)
	f.th = api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.PostPriority = new(true)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.EmailSettings.SendPushNotifications = new(false)
	}).InitBasic(t)
	require.True(t, f.th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	f.s = &Service{app: f.th.App, db: f.th.Store.GetInternalMasterDB()}
	require.NoError(t, f.s.initSettings(context.Background()))
	require.NoError(t, f.s.initDelivery(context.Background()))
	_, err := f.s.saveSettings(context.Background(), settingsUpdate{
		Endpoint: provider.URL, ApplicationIdentifier: "test-app", WorkflowIdentifier: "test-urgent", SecretKey: "temporary-test-key", Enabled: true,
	})
	require.NoError(t, err)
	f.cfg = deliverySettings{endpoint: provider.URL, workflow: "test-urgent", secret: "temporary-test-key", enabledAt: time.Now().Add(-time.Minute).UnixMilli()}
	_, err = f.s.db.Exec(`UPDATE WorkspaceNovaSettings SET EnabledAt=$1 WHERE Id=1`, f.cfg.enabledAt)
	require.NoError(t, err)
	f.channel, _, err = f.th.Client.CreateDirectChannel(context.Background(), f.th.BasicUser.Id, f.th.BasicUser2.Id)
	require.NoError(t, err)
	group, err := f.th.Store.PropertyGroup().Register(&model.PropertyGroup{Name: "access_control"})
	require.NoError(t, err)
	field, err := f.th.Store.PropertyField().Create(&model.PropertyField{
		GroupID: group.ID, Name: "Nova test phone", Type: model.PropertyFieldTypeText, ObjectType: "user", TargetType: "system",
		Attrs: model.StringInterface{"value_type": "phone"},
	})
	require.NoError(t, err)
	_, err = f.th.Store.PropertyValue().Create(&model.PropertyValue{
		GroupID: group.ID, FieldID: field.ID, TargetID: f.th.BasicUser2.Id, TargetType: "user", Value: json.RawMessage(`"737328812"`),
	})
	require.NoError(t, err)
	f.th.App.SetStatusOffline(f.th.BasicUser2.Id, true, true)
	return f
}

func (f *novaFixture) post(t *testing.T, urgent bool, at int64) *model.Post {
	t.Helper()
	input := &model.Post{ChannelId: f.channel.Id, Message: "رسالة عاجلة للاختبار"}
	if urgent {
		input.Metadata = &model.PostMetadata{Priority: &model.PostPriority{Priority: new(model.PostPriorityUrgent)}}
	}
	post, _, err := f.th.Client.CreatePost(context.Background(), input)
	require.NoError(t, err)
	_, err = f.s.db.Exec(`UPDATE Posts SET CreateAt=$1 WHERE Id=$2`, at, post.Id)
	require.NoError(t, err)
	return post
}

func TestNovaQueueFindsLateCommittedUrgentPost(t *testing.T) {
	f := setupNova(t)
	ctx := context.Background()
	older := f.post(t, false, time.Now().Add(-20*time.Second).UnixMilli())
	tx, err := f.s.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO PostsPriority (PostId, ChannelId, Priority, RequestedAck, PersistentNotifications) VALUES ($1,$2,'urgent',false,false)
		ON CONFLICT (PostId) DO UPDATE SET Priority='urgent'`, older.Id, f.channel.Id)
	require.NoError(t, err)
	newer := f.post(t, true, time.Now().Add(-10*time.Second).UnixMilli())
	require.NoError(t, f.s.enqueueUrgentDirectPosts(ctx, f.cfg.enabledAt))
	var queued []string
	rows, err := f.s.db.Query(`SELECT PostId FROM WorkspaceNovaDelivery ORDER BY PostId`)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		queued = append(queued, id)
	}
	require.NoError(t, rows.Close())
	require.Equal(t, []string{newer.Id}, queued, "the older urgent priority is still uncommitted")
	require.NoError(t, tx.Commit())
	restarted := &Service{app: f.th.App, db: f.s.db}
	require.NoError(t, restarted.initDelivery(ctx))
	require.NoError(t, restarted.enqueueUrgentDirectPosts(ctx, f.cfg.enabledAt))
	var count int
	require.NoError(t, f.s.db.QueryRow(`SELECT count(*) FROM WorkspaceNovaDelivery WHERE PostId=$1 AND UserId=$2`, older.Id, f.th.BasicUser2.Id).Scan(&count))
	require.Equal(t, 1, count, "a late commit must remain discoverable after a newer post and a restart")
	require.NoError(t, restarted.enqueueUrgentDirectPosts(ctx, f.cfg.enabledAt))
	require.NoError(t, f.s.db.QueryRow(`SELECT count(*) FROM WorkspaceNovaDelivery`).Scan(&count))
	require.Equal(t, 2, count, "repeated scanning must not duplicate either recipient")
}

func TestNovaAPIUrgentPostDeliveryAndRetry(t *testing.T) {
	f := setupNova(t)
	ctx := context.Background()
	f.post(t, true, time.Now().Add(-10*time.Second).UnixMilli())
	f.providerStatus.Store(http.StatusServiceUnavailable)
	require.NoError(t, f.s.deliveryPass(ctx))
	require.EqualValues(t, 1, f.requests.Load())
	var attempts int
	var done bool
	var retryAt int64
	require.NoError(t, f.s.db.QueryRow(`SELECT Attempts,Done,RetryAt FROM WorkspaceNovaDelivery`).Scan(&attempts, &done, &retryAt))
	require.Equal(t, 1, attempts)
	require.False(t, done)
	require.Greater(t, retryAt, time.Now().UnixMilli())
	_, err := f.s.db.Exec(`UPDATE WorkspaceNovaDelivery SET RetryAt=0`)
	require.NoError(t, err)
	f.providerStatus.Store(http.StatusCreated)
	require.NoError(t, f.s.deliveryPass(ctx))
	require.EqualValues(t, 2, f.requests.Load())
	transactionCount := 0
	f.transactions.Range(func(_, _ any) bool { transactionCount++; return true })
	require.Equal(t, 1, transactionCount, "retry must retain the provider idempotency identifier")
	require.NoError(t, f.s.deliveryPass(ctx))
	require.EqualValues(t, 2, f.requests.Load(), "completed delivery must not send again")
}

func TestNovaSendRechecksQueuedRecipientAndPolicy(t *testing.T) {
	f := setupNova(t)
	ctx := context.Background()
	for _, name := range []string{"online", "deleted_post", "removed_priority", "disabled_recipient", "removed_membership", "archived_channel", "disabled_policy", "superseded_policy", "removed_license"} {
		t.Run(name, func(t *testing.T) {
			post := f.post(t, true, time.Now().Add(-10*time.Second).UnixMilli())
			require.NoError(t, f.s.enqueueUrgentDirectPosts(ctx, f.cfg.enabledAt))
			switch name {
			case "online":
				f.th.App.SetStatusOnline(f.th.BasicUser2.Id, true)
				t.Cleanup(func() { f.th.App.SetStatusOffline(f.th.BasicUser2.Id, true, true) })
			case "deleted_post":
				_, err := f.s.db.Exec(`UPDATE Posts SET DeleteAt=1 WHERE Id=$1`, post.Id)
				require.NoError(t, err)
			case "removed_priority":
				_, err := f.s.db.Exec(`UPDATE PostsPriority SET Priority='important' WHERE PostId=$1`, post.Id)
				require.NoError(t, err)
			case "disabled_recipient":
				_, err := f.s.db.Exec(`UPDATE Users SET DeleteAt=1 WHERE Id=$1`, f.th.BasicUser2.Id)
				require.NoError(t, err)
				t.Cleanup(func() {
					_, err := f.s.db.Exec(`UPDATE Users SET DeleteAt=0 WHERE Id=$1`, f.th.BasicUser2.Id)
					require.NoError(t, err)
				})
			case "archived_channel":
				_, err := f.s.db.Exec(`UPDATE Channels SET DeleteAt=1 WHERE Id=$1`, f.channel.Id)
				require.NoError(t, err)
				t.Cleanup(func() {
					_, err := f.s.db.Exec(`UPDATE Channels SET DeleteAt=0 WHERE Id=$1`, f.channel.Id)
					require.NoError(t, err)
				})
			case "removed_membership":
				member, err := f.th.Store.Channel().GetMember(f.th.Context, f.channel.Id, f.th.BasicUser2.Id)
				require.NoError(t, err)
				_, err = f.s.db.Exec(`DELETE FROM ChannelMembers WHERE ChannelId=$1 AND UserId=$2`, f.channel.Id, f.th.BasicUser2.Id)
				require.NoError(t, err)
				t.Cleanup(func() { _, err := f.th.Store.Channel().SaveMember(f.th.Context, member); require.NoError(t, err) })
			case "disabled_policy":
				_, err := f.s.db.Exec(`UPDATE WorkspaceNovaSettings SET Enabled=false WHERE Id=1`)
				require.NoError(t, err)
				t.Cleanup(func() {
					_, err := f.s.db.Exec(`UPDATE WorkspaceNovaSettings SET Enabled=true WHERE Id=1`)
					require.NoError(t, err)
				})
			case "superseded_policy":
				_, err := f.s.db.Exec(`UPDATE WorkspaceNovaSettings SET EnabledAt=EnabledAt+1 WHERE Id=1`)
				require.NoError(t, err)
				t.Cleanup(func() {
					_, err := f.s.db.Exec(`UPDATE WorkspaceNovaSettings SET EnabledAt=$1 WHERE Id=1`, f.cfg.enabledAt)
					require.NoError(t, err)
				})
			case "removed_license":
				f.th.Server.SetLicense(nil)
				t.Cleanup(func() {
					require.True(t, f.th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
				})
			}
			before := f.requests.Load()
			err := f.s.sendDelivery(ctx, f.cfg, delivery{postID: post.Id, userID: f.th.BasicUser2.Id, enabledAt: f.cfg.enabledAt, attempts: 1})
			if name == "removed_license" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, before, f.requests.Load(), "a queued message must not bypass the current recipient or policy state")
		})
	}
}

func TestNovaLicenseRemovalStopsTheNextQueuedDelivery(t *testing.T) {
	f := setupNova(t)
	for i := 0; i < 2; i++ {
		f.post(t, true, time.Now().Add(-10*time.Second).UnixMilli())
	}
	f.afterRequest = func() { f.th.Server.SetLicense(nil) }
	require.NoError(t, f.s.deliveryPass(context.Background()))
	require.EqualValues(t, 1, f.requests.Load(), "removing the license during the first request must stop the next delivery")
	var pending int
	require.NoError(t, f.s.db.QueryRow(`SELECT count(*) FROM WorkspaceNovaDelivery WHERE NOT Done AND Attempts=0`).Scan(&pending))
	require.Equal(t, 1, pending, "the next job must remain available after the license is restored")
}

func TestNovaConcurrentScannersAndClaims(t *testing.T) {
	f := setupNova(t)
	ctx := context.Background()
	const postCount = 103
	for i := 0; i < postCount; i++ {
		f.post(t, true, time.Now().Add(-10*time.Second).UnixMilli())
	}
	errors := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errors <- f.s.enqueueUrgentDirectPosts(ctx, f.cfg.enabledAt) }()
	}
	require.NoError(t, <-errors)
	require.NoError(t, <-errors)
	var count int
	require.NoError(t, f.s.db.QueryRow(`SELECT count(*) FROM WorkspaceNovaDelivery`).Scan(&count))
	require.Equal(t, postCount, count)
	var claimed sync.Map
	for i := 0; i < 2; i++ {
		go func() {
			for {
				job, err := f.s.claimDelivery(ctx, f.cfg.enabledAt)
				if err == sql.ErrNoRows {
					errors <- nil
					return
				}
				if err != nil {
					errors <- err
					return
				}
				if _, loaded := claimed.LoadOrStore(job.postID, true); loaded {
					errors <- sql.ErrTxDone
					return
				}
			}
		}()
	}
	require.NoError(t, <-errors)
	require.NoError(t, <-errors)
	count = 0
	claimed.Range(func(_, _ any) bool { count++; return true })
	require.Equal(t, postCount, count)
}
