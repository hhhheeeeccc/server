package content_extraction_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestRegisteredExtractionReplaysCacheAfterUnacknowledgedContent(t *testing.T) {
	for _, mode := range []string{"catchup", "canceled", "periodic"} {
		t.Run(mode, func(t *testing.T) {
			th, proxy := setupSQLProxy(t, "owner", 30)
			proxy.dropAt.Store(6) // acquire, BEGIN, claim, COMMIT, range, content plus durable cache intent
			proxy.responseOnly.Store(true)
			info := upload(t, th, "persisted attachment", "cached.txt", time.Now().Add(-time.Minute))
			post, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: th.BasicChannel.Id, FileIds: model.StringArray{info.Id}})
			require.NoError(t, err)
			files, err := th.App.Srv().Store().FileInfo().GetForPost(post.Id, true, false, true)
			require.NoError(t, err)
			require.Len(t, files, 1)
			require.Empty(t, files[0].Content)
			th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings.ExtractContent = new(true) })
			job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, model.StringMap{"catchup": "true"})
			require.Nil(t, appErr)
			select {
			case <-proxy.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the real content commit must lose its SQL response")
			}
			require.Eventually(t, func() bool { return get(t, th, info.Id).Content == "persisted attachment" }, time.Second, 10*time.Millisecond)
			if mode == "periodic" {
				require.Eventually(t, func() bool { return proxy.blocked() == 0 }, 5*time.Second, 10*time.Millisecond)
			} else {
				require.NoError(t, th.Server.Jobs.StopWorkers())
			}
			files, err = th.App.Srv().Store().FileInfo().GetForPost(post.Id, true, false, true)
			require.NoError(t, err)
			require.Empty(t, files[0].Content, "the commit happened before any native cache invalidation")
			if mode == "canceled" {
				_, err = th.SystemAdminClient.CancelJob(context.Background(), job.Id)
				require.NoError(t, err)
			}
			// A replay must use its durable intent, not reopen the already
			// committed attachment or depend on another extraction succeeding.
			require.Nil(t, th.App.RemoveFile(info.Path))
			proxy.unblock.Store(true)
			wait := 3 * time.Second
			if mode == "periodic" {
				wait = 40 * time.Second
			} else {
				require.NoError(t, th.Server.Jobs.StartWorkers())
			}
			require.Eventually(t, func() bool {
				files, err := th.App.Srv().Store().FileInfo().GetForPost(post.Id, true, false, true)
				return err == nil && len(files) == 1 && files[0].Content == "persisted attachment"
			}, wait, 10*time.Millisecond, "restart or periodic replay must invalidate the previously warmed cache even when catchup skips the committed file or its job is canceled")
			want := model.JobStatusSuccess
			if mode == "canceled" {
				want = model.JobStatusCanceled
			}
			require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == want }, 10*time.Second, 20*time.Millisecond)
			if mode != "canceled" {
				require.Equal(t, "1", currentJob(t, th, job.Id).Data["processed"], "the durable file outcome survives recovery")
			}
		})
	}
}

func TestRegisteredExtractionCacheIntentAndContentCommitTogether(t *testing.T) {
	th := setup(t)
	// Join startup maintenance before seeding a queue row used to stall the
	// next content statement. No test substitutes the real worker or store.
	run(t, th, nil, model.JobStatusSuccess)
	info := upload(t, th, "atomic attachment", "atomic.txt", time.Now().Add(-time.Minute))
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: th.BasicChannel.Id, FileIds: model.StringArray{info.Id}})
	require.NoError(t, err)
	version := model.NewId()
	db := th.Store.GetInternalMasterDB()
	_, err = db.Exec(`INSERT INTO WorkspaceExtractionCacheOutbox (PostId,Version,QueuedAt) VALUES ($1,$2,$3)`, post.Id, version, model.GetMillis())
	require.NoError(t, err)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	var id string
	require.NoError(t, tx.QueryRow(`SELECT PostId FROM WorkspaceExtractionCacheOutbox WHERE PostId=$1 FOR UPDATE`, post.Id).Scan(&id))
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, nil)
	require.Nil(t, appErr)
	pid := waitingContentBackend(t, th)
	stopped := make(chan error, 1)
	go func() { stopped <- th.Server.Jobs.StopWorkers() }()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		_ = tx.Rollback()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("Stop did not exit after releasing the outbox fixture")
		}
		t.Fatal("Stop did not cancel a content statement blocked on its durable cache intent")
	}
	require.Eventually(t, func() bool {
		var n int
		return db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&n) == nil && n == 0
	}, 4*time.Second, 10*time.Millisecond)
	require.NoError(t, tx.Rollback())
	require.Empty(t, get(t, th, info.Id).Content, "content must roll back when its cache intent cannot commit")
	require.Empty(t, currentJob(t, th, job.Id).Data["workspace_extraction_checkpoint_v1"], "a rolled-back file must not advance the durable cursor")
	require.Equal(t, "0", currentJob(t, th, job.Id).Data["processed"])
	var storedVersion string
	require.NoError(t, db.QueryRow(`SELECT Version FROM WorkspaceExtractionCacheOutbox WHERE PostId=$1`, post.Id).Scan(&storedVersion))
	require.Equal(t, version, storedVersion)
	require.NoError(t, th.Server.Jobs.StartWorkers())
	require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == model.JobStatusSuccess }, 10*time.Second, 20*time.Millisecond)
	require.Equal(t, "atomic attachment", get(t, th, info.Id).Content)
	require.ErrorIs(t, db.QueryRow(`SELECT Version FROM WorkspaceExtractionCacheOutbox WHERE PostId=$1`, post.Id).Scan(&storedVersion), sql.ErrNoRows)
}

func TestRegisteredExtractionStopInterruptsSilentCacheReplay(t *testing.T) {
	th, proxy := setupSQLProxy(t, "cache_replay", 30)
	select {
	case <-proxy.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("startup cache replay must reach the real SQL proxy")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- th.Server.Jobs.StopWorkers() }()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not interrupt cache replay before its SQL deadline")
	}
	require.Eventually(t, func() bool { return proxy.blocked() == 0 }, time.Second, 10*time.Millisecond)
	require.Positive(t, proxy.dropped.Load())
	proxy.unblock.Store(true)
	require.NoError(t, th.Server.Jobs.StartWorkers())
	info := upload(t, th, "after replay outage", "replay.txt", time.Now().Add(-time.Minute))
	run(t, th, nil, model.JobStatusSuccess)
	require.Equal(t, "after replay outage", get(t, th, info.Id).Content)
}
