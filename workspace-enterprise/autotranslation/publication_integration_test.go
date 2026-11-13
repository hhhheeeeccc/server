package autotranslation

import (
	"context"
	"sync"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func markPublicationPending(t *testing.T, th *api4.TestHelper, postID string, state model.TranslationState) {
	t.Helper()
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET text='Synthetic durable translation.',state=$3,
		meta=jsonb_set(COALESCE(meta,'{}'),'{workspace_publish_v1}',jsonb_build_object('id',$2::text),true)
		WHERE objecttype='post' AND objectid=$1 AND dstlang='ar'`, postID, model.NewId(), state)
	require.NoError(t, err)
}

func TestPublicationClaimsExcludeConcurrentDispatchers(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := seedRecoveryPost(t, th, "Synthetic queued translation.", model.TranslationStateUnavailable)
	markPublicationPending(t, th, post.Id, model.TranslationStateReady)
	results := make(chan *publicationClaim, 8)
	errors := make(chan error, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			claim, err := New(th.Server).claimPublication(th.Context.Context(), post.Id, "ar")
			results <- claim
			errors <- err
		})
	}
	workers.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	winners := 0
	for claim := range results {
		if claim != nil {
			winners++
		}
	}
	require.Equal(t, 1, winners, "a live database lease must have only one owner")
}

func TestPublicationAcknowledgementFencesReplacementsAndPreservesVersion(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	db, service := th.Store.GetInternalMasterDB(), New(th.Server)
	for _, replacement := range []string{"none", "revision", "lease", "hash", "canceled", "license", "source"} {
		t.Run(replacement, func(t *testing.T) {
			post := seedRecoveryPost(t, th, "Synthetic source for acknowledgement.", model.TranslationStateUnavailable)
			markPublicationPending(t, th, post.Id, model.TranslationStateReady)
			var before int64
			require.NoError(t, db.QueryRow(`SELECT updateat FROM translations WHERE objectid=$1`, post.Id).Scan(&before))
			claim, err := service.claimPublication(th.Context.Context(), post.Id, "ar")
			require.NoError(t, err)
			require.NotNil(t, claim)
			ctx, cancel := context.WithCancel(th.Context.Context())
			defer cancel()
			switch replacement {
			case "revision":
				_, err = db.Exec(`UPDATE translations SET meta=jsonb_set(meta,'{workspace_publish_v1,id}',to_jsonb($2::text)) WHERE objectid=$1`, post.Id, model.NewId())
			case "lease":
				_, err = db.Exec(`UPDATE translations SET meta=jsonb_set(meta,'{workspace_publish_v1,lease}',to_jsonb($2::text)) WHERE objectid=$1`, post.Id, model.NewId())
			case "hash":
				_, err = db.Exec(`UPDATE translations SET normhash=repeat('a',64) WHERE objectid=$1`, post.Id)
			case "canceled":
				cancel()
			case "license":
				license := th.Server.License()
				th.Server.SetLicense(nil)
				defer func() { require.True(t, th.Server.SetLicense(license)) }()
			case "source":
				_, err = db.Exec(`UPDATE posts SET message='Edited source.' WHERE id=$1`, post.Id)
			}
			require.NoError(t, err)
			if replacement != "none" {
				sent, emitErr := service.emitRecoveredTranslation(ctx, claim)
				require.False(t, sent, "a superseded claim must not publish")
				if replacement == "canceled" {
					require.ErrorIs(t, emitErr, context.Canceled)
				} else {
					require.NoError(t, emitErr)
				}
			}
			err = service.acknowledgePublication(ctx, claim)
			if replacement == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}
			var pending bool
			var after int64
			require.NoError(t, db.QueryRow(`SELECT meta ? 'workspace_publish_v1',updateat FROM translations WHERE objectid=$1`, post.Id).Scan(&pending, &after))
			require.Equal(t, replacement != "none" && replacement != "source", pending)
			if replacement != "source" {
				require.Equal(t, before, after, "claim and acknowledgement must not change the public translation version")
			}
		})
	}
}

func TestPublicationFailedAcknowledgementRetainsRetryAndCanDuplicateDispatch(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := seedRecoveryPost(t, th, "Synthetic source for failed acknowledgement.", model.TranslationStateSkipped)
	markPublicationPending(t, th, post.Id, model.TranslationStateSkipped)
	socket := th.CreateConnectedWebSocketClientWithClient(t, th.Client)
	service, db := New(th.Server), th.Store.GetInternalMasterDB()
	claim, err := service.claimPublication(th.Context.Context(), post.Id, "ar")
	require.NoError(t, err)
	require.NotNil(t, claim)
	_, err = db.Exec(`CREATE FUNCTION workspace_test_reject_publication_ack() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic acknowledgement failure'; END $$;
		CREATE TRIGGER workspace_test_reject_publication_ack BEFORE UPDATE ON translations
		FOR EACH ROW WHEN (OLD.meta ? 'workspace_publish_v1' AND NOT (NEW.meta ? 'workspace_publish_v1'))
		EXECUTE FUNCTION workspace_test_reject_publication_ack()`)
	require.NoError(t, err)
	drop := func() {
		_, err := db.Exec(`DROP TRIGGER IF EXISTS workspace_test_reject_publication_ack ON translations;
			DROP FUNCTION IF EXISTS workspace_test_reject_publication_ack()`)
		require.NoError(t, err)
	}
	t.Cleanup(drop)
	require.ErrorContains(t, service.dispatchPublication(th.Context.Context(), claim), "synthetic acknowledgement failure")
	first := nextRecoveredTranslation(t, socket, post.Id)
	var pending bool
	require.NoError(t, db.QueryRow(`SELECT meta ? 'workspace_publish_v1' FROM translations WHERE objectid=$1`, post.Id).Scan(&pending))
	require.True(t, pending)
	drop()
	require.NoError(t, service.dispatchPublication(th.Context.Context(), claim))
	second := nextRecoveredTranslation(t, socket, post.Id)
	require.Equal(t, first, second, "duplicate dispatch must carry the same version for client deduplication")
	require.NoError(t, db.QueryRow(`SELECT meta ? 'workspace_publish_v1' FROM translations WHERE objectid=$1`, post.Id).Scan(&pending))
	require.False(t, pending)
}
