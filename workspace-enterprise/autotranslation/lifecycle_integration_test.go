package autotranslation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func setupTranslationLifecycle(t *testing.T) *api4.TestHelper {
	t.Helper()
	return api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.EmailSettings.SendPushNotifications = new(false)
	}).InitBasic(t)
}

func seedLifecycleTranslations(t *testing.T, th *api4.TestHelper, post *model.Post) {
	t.Helper()
	digest := sha256.Sum256([]byte(post.Message))
	for i, state := range []model.TranslationState{model.TranslationStateReady, model.TranslationStateSkipped,
		model.TranslationStateProcessing, model.TranslationStateUnavailable} {
		require.NoError(t, th.Store.AutoTranslation().Save(&model.Translation{
			ObjectType: model.TranslationObjectTypePost, ObjectID: post.Id, ChannelID: post.ChannelId,
			Lang: []string{"ar", "fr", "es", "de"}[i], Provider: providerName,
			Type: model.TranslationTypeString, Text: "derived private text",
			NormHash: hex.EncodeToString(digest[:]), State: state,
		}))
	}
	// The table is shared with other object types; the same ID is not a post.
	require.NoError(t, th.Store.AutoTranslation().Save(&model.Translation{
		ObjectType: "other_object", ObjectID: post.Id, Lang: "ar", Provider: providerName,
		Type: model.TranslationTypeString, Text: "unrelated text", State: model.TranslationStateReady,
	}))
}

func requireLifecycleCounts(t *testing.T, th *api4.TestHelper, postID string, want int) {
	t.Helper()
	var posts, others int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT
		count(*) FILTER (WHERE objecttype='post'), count(*) FILTER (WHERE objecttype='other_object')
		FROM translations WHERE objectid=$1`, postID).Scan(&posts, &others))
	require.Equal(t, want, posts, "post translations must follow their source's deletion")
	require.Equal(t, 1, others, "unrelated object types must be preserved")
}

func TestTranslationLifecycleDeletionPaths(t *testing.T) {
	for _, path := range []string{"REST soft delete", "permanent post", "permanent channel", "permanent user", "retention batch", "legacy batch"} {
		t.Run(path, func(t *testing.T) {
			th := setupTranslationLifecycle(t)
			root := th.BasicPost
			reply, err := th.Store.Post().Save(th.Context, &model.Post{ChannelId: root.ChannelId,
				UserId: th.BasicUser2.Id, RootId: root.Id, Message: "A reply with derived text"})
			require.NoError(t, err)
			seedLifecycleTranslations(t, th, root)
			seedLifecycleTranslations(t, th, reply)
			current := th.CreatePost(t)
			seedLifecycleTranslations(t, th, current)
			// Deletion hygiene must still apply with translation disabled and no license.
			require.Nil(t, th.Server.License())
			switch path {
			case "REST soft delete":
				_, err = th.SystemAdminClient.DeletePost(th.Context.Context(), root.Id)
			case "permanent post":
				err = th.Store.Post().PermanentDelete(th.Context, root.Id)
			case "permanent channel":
				err = th.Store.Post().PermanentDeleteByChannel(th.Context, root.ChannelId)
			case "permanent user":
				err = th.Store.Post().PermanentDeleteByUser(th.Context, root.UserId)
			case "retention batch", "legacy batch":
				_, err = th.Store.GetInternalMasterDB().Exec(`UPDATE posts SET createat=$1 WHERE id IN ($2,$3)`,
					time.Now().Add(-48*time.Hour).UnixMilli(), root.Id, reply.Id)
				require.NoError(t, err)
				if path == "retention batch" {
					_, _, err = th.Store.Post().PermanentDeleteBatchForRetentionPolicies(model.RetentionPolicyBatchConfigs{
						Now: model.GetMillis(), GlobalPolicyEndTime: time.Now().Add(-time.Hour).UnixMilli(), Limit: 1000,
					}, model.RetentionPolicyCursor{})
				} else {
					_, err = th.Store.Post().PermanentDeleteBatch(time.Now().Add(-time.Hour).UnixMilli(), 1000)
				}
			}
			require.NoError(t, err)
			requireLifecycleCounts(t, th, root.Id, 0)
			requireLifecycleCounts(t, th, reply.Id, 0)
			if path != "permanent channel" && path != "permanent user" {
				requireLifecycleCounts(t, th, current.Id, 4)
			}
		})
	}
}

func TestTranslationLifecycleDeletionRollbackPreservesTranslations(t *testing.T) {
	th := setupTranslationLifecycle(t)
	seedLifecycleTranslations(t, th, th.BasicPost)
	tx, err := th.Store.GetInternalMasterDB().BeginTx(th.Context.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`DELETE FROM posts WHERE id=$1`, th.BasicPost.Id)
	require.NoError(t, err)
	var count int
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM translations WHERE objecttype='post' AND objectid=$1`, th.BasicPost.Id).Scan(&count))
	require.Zero(t, count, "derived rows are removed in the source transaction")
	require.NoError(t, tx.Rollback())
	requireLifecycleCounts(t, th, th.BasicPost.Id, 4)
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM posts WHERE id=$1`, th.BasicPost.Id).Scan(&count))
	require.Equal(t, 1, count)
}

func TestTranslationLifecycleDeleteAndInsertSerialize(t *testing.T) {
	for _, soft := range []bool{false, true} {
		for _, insertFirst := range []bool{false, true} {
			name := "hard"
			if soft {
				name = "soft"
			}
			if insertFirst {
				name += "/insert first"
			} else {
				name += "/delete first"
			}
			t.Run(name, func(t *testing.T) {
				th := setupTranslationLifecycle(t)
				db := th.Store.GetInternalMasterDB()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				first, err := db.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer first.Rollback()
				second, err := db.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer second.Rollback()
				insert := `INSERT INTO translations (objectid,objecttype,dstlang,providerid,normhash,text,state,updateat)
					VALUES ($1,'post','ar','libretranslate','','translated text','ready',1)`
				deletion := `DELETE FROM posts WHERE id=$1`
				if soft {
					deletion = `UPDATE posts SET deleteat=1 WHERE id=$1`
				}
				firstSQL, secondSQL := deletion, insert
				if insertFirst {
					firstSQL, secondSQL = insert, deletion
				}
				_, err = first.ExecContext(ctx, firstSQL, th.BasicPost.Id)
				require.NoError(t, err)
				var pid int
				require.NoError(t, second.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
				finished := make(chan error, 1)
				go func() {
					_, execErr := second.ExecContext(ctx, secondSQL, th.BasicPost.Id)
					finished <- execErr
				}()
				// Verify a real PostgreSQL lock wait before releasing the winner.
				require.Eventually(t, func() bool {
					var blocked bool
					return db.QueryRowContext(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked) == nil && blocked
				}, 3*time.Second, 10*time.Millisecond)
				require.NoError(t, first.Commit())
				select {
				case err = <-finished:
				case <-ctx.Done():
					t.Fatal("deletion/translation lock wait did not resolve")
				}
				if insertFirst {
					require.NoError(t, err)
					require.NoError(t, second.Commit())
				} else {
					var pgErr *pq.Error
					require.ErrorAs(t, err, &pgErr)
					require.Equal(t, pq.ErrorCode("23503"), pgErr.Code)
					require.NoError(t, second.Rollback())
				}
				var count int
				require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM translations WHERE objectid=$1`, th.BasicPost.Id).Scan(&count))
				require.Zero(t, count, "a committed deletion must leave no derived text")
			})
		}
	}
}

func TestTranslationLifecyclePayloadUpdateDoesNotDeadlockDeletion(t *testing.T) {
	th := setupTranslationLifecycle(t)
	seedLifecycleTranslations(t, th, th.BasicPost)
	db := th.Store.GetInternalMasterDB()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	update, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer update.Rollback()
	deletion, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer deletion.Rollback()
	_, err = update.ExecContext(ctx, `UPDATE translations SET state='processing' WHERE objecttype='post' AND objectid=$1`, th.BasicPost.Id)
	require.NoError(t, err)
	var pid int
	require.NoError(t, deletion.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	finished := make(chan error, 1)
	go func() {
		_, execErr := deletion.ExecContext(ctx, `DELETE FROM posts WHERE id=$1`, th.BasicPost.Id)
		finished <- execErr
	}()
	require.Eventually(t, func() bool {
		var blocked bool
		return db.QueryRowContext(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked) == nil && blocked
	}, 3*time.Second, 10*time.Millisecond)
	_, err = update.ExecContext(ctx, `UPDATE translations SET text='late recovery result',state='ready'
		WHERE objecttype='post' AND objectid=$1`, th.BasicPost.Id)
	require.NoError(t, err)
	require.NoError(t, update.Commit())
	select {
	case err = <-finished:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("deletion deadlocked with a translation payload update")
	}
	require.NoError(t, deletion.Commit())
	requireLifecycleCounts(t, th, th.BasicPost.Id, 0)
}

func TestTranslationLifecycleDiscardsProviderReplyAfterDeletion(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		name := "foreground"
		if recovery {
			name = "recovery"
		}
		t.Run(name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			releaseProvider := sync.OnceFunc(func() { close(release) })
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/detect" {
					_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
					return
				}
				select {
				case <-started:
				default:
					close(started)
				}
				<-release
				_, _ = w.Write([]byte(`{"translatedText":"late private translation"}`))
			}))
			t.Cleanup(provider.Close)
			t.Cleanup(releaseProvider)
			th := setupRecovery(t, provider.URL)
			post := seedRecoveryPost(t, th, "Delete while the translation request is running.", model.TranslationStateUnavailable)
			finished := make(chan *model.AppError, 1)
			service := New(th.Server)
			t.Cleanup(func() { require.NoError(t, service.Shutdown()) })
			if recovery {
				go func() {
					err := service.recoverTranslations(th.Context.Context(), th.Context.Logger())
					if err != nil {
						finished <- providerError("test", 500, err)
						return
					}
					finished <- nil
				}()
			} else {
				go func() {
					_, appErr := service.Translate(th.Context.Context(), model.TranslationObjectTypePost, post.Id, post.ChannelId, post.UserId, post.Message)
					finished <- appErr
				}()
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("translation request did not reach the provider")
			}
			_, err := th.SystemAdminClient.DeletePost(th.Context.Context(), post.Id)
			require.NoError(t, err)
			releaseProvider()
			select {
			case appErr := <-finished:
				if recovery {
					require.Nil(t, appErr)
				} else {
					require.Nil(t, appErr, "a superseded foreground attempt is discarded without saving")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("translation did not finish after deletion")
			}
			var count int
			require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM translations WHERE objecttype='post' AND objectid=$1`, post.Id).Scan(&count))
			require.Zero(t, count, "a late provider response must not recreate deleted text")
		})
	}
}
