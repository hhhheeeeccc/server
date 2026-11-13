package cluster

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/config"
	"github.com/stretchr/testify/require"
)

// Model a full transport queue deterministically. The real public Cluster
// methods, native store handlers and cache providers remain in use. There is
// no network sender consuming this queue, so the assertion cannot pass just
// because a fast loopback peer happened to free a slot.
func fullOutgoingCacheQueue(t *testing.T, s *Service) (chan envelope, chan envelope, <-chan struct{}) {
	t.Helper()
	stop := make(chan struct{})
	queue := make(chan envelope, 1)
	queue <- envelope{Method: "occupied"}
	incoming := make(chan envelope, 8)
	s.mu.Lock()
	previousList, previousStop, previousOutgoing, previousIncoming := s.list, s.stop, s.outgoing, s.incoming
	if previousList != nil {
		s.mu.Unlock()
		t.Fatal("fixture must not replace a live transport")
	}
	s.list, s.stop, s.outgoing, s.incoming = &memberlist.Memberlist{}, stop, queue, incoming
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); s.receiveLoop(stop, incoming) }()
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() { close(stop) })
		<-done
		s.mu.Lock()
		s.list, s.stop, s.outgoing, s.incoming = previousList, previousStop, previousOutgoing, previousIncoming
		s.mu.Unlock()
	})
	return queue, incoming, done
}

func TestNativeFileCacheInvalidationDoesNotWaitForOutgoingClusterCapacity(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.EmailSettings.SendPushNotifications = new(false)
		cfg.JobSettings.RunJobs = new(false)
		cfg.JobSettings.RunScheduler = new(false)
		cfg.FileSettings.ExtractContent = new(false)
	}).InitBasic(t)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	require.True(t, th.Server.SetLicense(license))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ClusterSettings.Enable = new(true)
		cfg.ClusterSettings.ClusterName = new(model.NewId())
	})
	cfg := th.App.Config().Clone()
	cfg.ServiceSettings.ListenAddress = new("localhost:0")
	mem, err := config.NewMemoryStoreWithOptions(&config.MemoryStoreOptions{IgnoreEnvironmentOverrides: true})
	require.NoError(t, err)
	require.NoError(t, mem.Set(cfg))
	cs, err := config.NewStoreFromBacking(mem, nil, false)
	require.NoError(t, err)
	second, err := app.NewServer(app.ConfigStore(cs))
	require.NoError(t, err)
	t.Cleanup(func() { second.Shutdown(); _ = cs.Close() })
	require.True(t, second.SetLicense(license))
	sender := th.Server.Platform().Cluster().(*Service)
	receiver := second.Platform().Cluster().(*Service)
	info, appErr := th.App.UploadFileX(th.Context, th.BasicChannel.Id, "cache-pressure.txt", strings.NewReader("attachment"),
		app.UploadFileSetUserId(th.BasicUser.Id), app.UploadFileSetTeamId(th.BasicTeam.Id),
		app.UploadFileSetTimestamp(time.Now().Add(-time.Minute)), app.UploadFileSetContentLength(10))
	require.Nil(t, appErr)
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: th.BasicChannel.Id, FileIds: model.StringArray{info.Id}})
	require.NoError(t, err)
	for _, store := range []interface {
		GetForPost(string, bool, bool, bool) ([]*model.FileInfo, error)
	}{th.App.Srv().Store().FileInfo(), second.Store().FileInfo()} {
		files, err := store.GetForPost(post.Id, true, false, true)
		require.NoError(t, err)
		require.Len(t, files, 1)
		require.Empty(t, files[0].Content)
	}
	_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE FileInfo SET Content='new metadata' WHERE Id=$1`, info.Id)
	require.NoError(t, err)
	queue, _, _ := fullOutgoingCacheQueue(t, sender)
	fullOutgoingCacheQueue(t, receiver)
	called := make(chan struct{})
	go func() {
		th.App.Srv().Store().FileInfo().InvalidateFileInfosForPostCache(post.Id, false)
		close(called)
	}()
	returnedWhileFull := false
	select {
	case <-called:
		returnedWhileFull = true
	case <-time.After(250 * time.Millisecond):
		// Release only after observing the failure, so baseline cleanup is
		// bounded and cannot leave a native call hanging in a test goroutine.
		<-queue
		select {
		case <-called:
		case <-time.After(3 * time.Second):
			t.Fatal("native invalidation did not return after a queue slot was freed")
		}
	}
	require.True(t, returnedWhileFull, "the native FileInfo invalidation must return while Cluster's outgoing queue is still full")
	require.Len(t, queue, 1)
	require.Equal(t, "occupied", (<-queue).Method)
	files, err := th.App.Srv().Store().FileInfo().GetForPost(post.Id, true, false, true)
	require.NoError(t, err)
	require.Equal(t, "new metadata", files[0].Content, "the native local invalidation still runs")
	files, err = second.Store().FileInfo().GetForPost(post.Id, true, false, true)
	require.NoError(t, err)
	require.Empty(t, files[0].Content, "the independent receiver cache must still need the lost invalidation")
	state := sender.LocalState(false)
	require.NotContains(t, string(state), post.Id)
	var clocks cacheState
	require.NoError(t, json.Unmarshal(state, &clocks))
	version := clocks.Versions[model.ClusterEventInvalidateCacheForFileInfos]
	require.Positive(t, version)
	// Exercise the public state merge and its real serialized receiver. The
	// production LocalState/MergeRemoteState callbacks normally travel over
	// memberlist push/pull; no lost key or payload is manually replayed here.
	receiver.MergeRemoteState(state, false)
	require.Eventually(t, func() bool {
		files, err := second.Store().FileInfo().GetForPost(post.Id, true, false, true)
		return err == nil && len(files) == 1 && files[0].Content == "new metadata"
	}, time.Second, time.Millisecond)
	require.Equal(t, version, receiver.receivedCacheVersion(sender.id, model.ClusterEventInvalidateCacheForFileInfos))

	// Capacity permits precise key delivery, with a snapshot of buffers the
	// caller is allowed to reuse after this public method returns.
	message := &model.ClusterMessage{Event: model.ClusterEventInvalidateCacheForFileInfos, SendType: model.ClusterSendBestEffort, Data: []byte("snapshot")}
	sender.SendClusterMessage(message)
	message.Data[0] = 'X'
	enqueued := <-queue
	require.Equal(t, "snapshot", string(enqueued.Message.Data))
	require.Equal(t, version+1, enqueued.CacheVersion)
	for _, mode := range []struct {
		name, sendType string
		event          model.ClusterEvent
	}{
		{"reliable cache", model.ClusterSendReliable, model.ClusterEventInvalidateCacheForFileInfos},
		{"application best effort", model.ClusterSendBestEffort, model.ClusterEventPluginEvent},
	} {
		t.Run(mode.name, func(t *testing.T) {
			queue <- envelope{Method: "occupied"}
			started, returned := make(chan struct{}), make(chan struct{})
			go func() {
				close(started)
				sender.SendClusterMessage(&model.ClusterMessage{Event: mode.event, SendType: mode.sendType})
				close(returned)
			}()
			<-started
			select {
			case <-returned:
				<-queue
				t.Fatal("nonrecoverable or reliable messages must retain their original queue contract")
			case <-time.After(30 * time.Millisecond):
			}
			require.Equal(t, "occupied", (<-queue).Method)
			select {
			case <-returned:
			case <-time.After(time.Second):
				t.Fatal("sender did not return after capacity became available")
			}
			message := <-queue
			require.Equal(t, mode.event, message.Message.Event)
			require.Equal(t, mode.sendType, message.Message.SendType)
		})
	}
}
