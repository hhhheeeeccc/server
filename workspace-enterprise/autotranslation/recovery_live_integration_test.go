package autotranslation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func nextRecoveredTranslation(t *testing.T, socket *model.WebSocketClient, postID string) map[string]any {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, open := <-socket.EventChannel:
			require.True(t, open, "websocket closed before recovered translation arrived")
			if event.EventType() == model.WebsocketEventPostTranslationUpdated && event.GetData()["object_id"] == postID {
				return event.GetData()
			}
		case <-timer.C:
			t.Fatal("recovered translation did not reach an authenticated websocket")
			return nil
		}
	}
}

func TestRecoveryPublicationRechecksCurrentEligibility(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	socket := th.CreateConnectedWebSocketClientWithClient(t, th.Client)
	service := New(th.Server)
	for _, gate := range []string{"source_changed", "deleted", "channel_disabled", "opted_out", "license_removed", "configuration_disabled", "canceled", "different_hash"} {
		t.Run(gate, func(t *testing.T) {
			post := seedRecoveryPost(t, th, "Source before publication.", model.TranslationStateUnavailable)
			translation, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
			require.NoError(t, err)
			translation.State, translation.Text = model.TranslationStateReady, "النص المستعاد"
			translation.Provider, translation.ChannelID = providerName, post.ChannelId
			require.NoError(t, th.Store.AutoTranslation().Save(translation))
			markPublicationPending(t, th, post.Id, model.TranslationStateReady)
			claim, err := service.claimPublication(th.Context.Context(), post.Id, "ar")
			require.NoError(t, err)
			require.NotNil(t, claim, "claim the result before withdrawing eligibility")
			ctx, cancel := context.WithCancel(th.Context.Context())
			defer cancel()
			db := th.Store.GetInternalMasterDB()
			switch gate {
			case "source_changed":
				_, err = db.Exec(`UPDATE posts SET message='Newer source.' WHERE id=$1`, post.Id)
			case "deleted":
				_, err = db.Exec(`UPDATE posts SET deleteat=1 WHERE id=$1`, post.Id)
			case "channel_disabled":
				_, err = db.Exec(`UPDATE channels SET autotranslation=false WHERE id=$1`, post.ChannelId)
				defer func() {
					_, e := db.Exec(`UPDATE channels SET autotranslation=true WHERE id=$1`, post.ChannelId)
					require.NoError(t, e)
				}()
			case "opted_out":
				_, err = db.Exec(`UPDATE channelmembers SET autotranslationdisabled=true WHERE channelid=$1`, post.ChannelId)
				defer func() {
					_, e := db.Exec(`UPDATE channelmembers SET autotranslationdisabled=false WHERE channelid=$1`, post.ChannelId)
					require.NoError(t, e)
				}()
			case "license_removed":
				license := th.Server.License()
				th.Server.SetLicense(nil)
				defer func() { require.True(t, th.Server.SetLicense(license)) }()
			case "configuration_disabled":
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.AutoTranslationSettings.Enable = new(false) })
				defer th.App.UpdateConfig(func(cfg *model.Config) { cfg.AutoTranslationSettings.Enable = new(true) })
			case "canceled":
				cancel()
			case "different_hash":
				claim.normHash = "superseded-hash"
			}
			require.NoError(t, err)
			sent, err := service.emitRecoveredTranslation(ctx, claim)
			require.False(t, sent)
			if gate == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}
			barrier := model.NewWebSocketEvent(model.WebsocketEventType("workspace_recovery_test_barrier"), "", "", th.BasicUser.Id, nil, "")
			th.Server.Platform().Publish(barrier)
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			for {
				select {
				case event, open := <-socket.EventChannel:
					require.True(t, open)
					if event.EventType() == model.WebsocketEventPostTranslationUpdated {
						require.NotEqual(t, post.Id, event.GetData()["object_id"], "an ineligible result was broadcast")
					}
					if event.EventType() == barrier.EventType() {
						return
					}
				case <-timer.C:
					t.Fatal("websocket barrier did not arrive")
				}
			}
		})
	}
}

func TestRecoveryPublishesCurrentTranslationToChannel(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
			return
		}
		_, _ = w.Write([]byte(`{"translatedText":"سيبدأ اجتماع المشروع غدًا."}`))
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	private := th.CreateChannelWithClient(t, th.SystemAdminClient, model.ChannelTypePrivate)
	th.AddUserToChannel(t, th.BasicUser, private)
	_, _, err := th.SystemAdminClient.PatchChannel(th.Context.Context(), private.Id, &model.ChannelPatch{AutoTranslation: new(true)})
	require.NoError(t, err)
	th.BasicChannel = private
	post := seedRecoveryPost(t, th, "The project meeting starts tomorrow.", model.TranslationStateUnavailable)
	author := th.CreateConnectedWebSocketClientWithClient(t, th.Client)
	outsiderClient := th.CreateClient()
	th.LoginBasic2WithClient(t, outsiderClient)
	outsider := th.CreateConnectedWebSocketClientWithClient(t, outsiderClient)

	runRecoveryWorker(t, th, New(th.Server))
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, model.TranslationStateReady, stored.State)
	data := nextRecoveredTranslation(t, author, post.Id)
	require.Equal(t, post.ChannelId, data["channel_id"])
	require.Equal(t, post.Message, data["source_text"])
	encoded, err := json.Marshal(data["translations"])
	require.NoError(t, err)
	// Decode explicitly to check the public wire contract rather than Go field names.
	var wire map[string]map[string]any
	require.NoError(t, json.Unmarshal(encoded, &wire))
	require.Equal(t, "ready", wire["ar"]["state"])
	require.Equal(t, stored.Text, wire["ar"]["translation"])
	require.Equal(t, "string", wire["ar"]["translation_type"])
	require.Equal(t, "en", wire["ar"]["src_lang"])
	var updatedAt int64
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT updateat FROM translations WHERE objectid=$1 AND dstlang='ar'`, post.Id).Scan(&updatedAt))
	require.EqualValues(t, updatedAt, wire["ar"]["update_at"])
	require.NotContains(t, string(encoded), "workspace_retry")
	require.NotContains(t, string(encoded), "lease")

	// A targeted barrier after the channel event proves the non-member's live
	// socket was serviced without relying on a sleep or an HTTP history read.
	barrierKind := model.WebsocketEventType("workspace_recovery_test_barrier")
	barrier := model.NewWebSocketEvent(barrierKind, "", "", th.BasicUser2.Id, nil, "")
	th.Server.Platform().Publish(barrier)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, open := <-outsider.EventChannel:
			require.True(t, open)
			require.NotEqual(t, model.WebsocketEventPostTranslationUpdated, event.EventType(), "private-channel translation leaked to a non-member")
			if event.EventType() == barrierKind {
				current, storeErr := th.Store.Post().GetSingle(th.Context, post.Id, false)
				require.NoError(t, storeErr)
				require.Equal(t, post.Message, current.Message)
				require.Equal(t, post.UpdateAt, current.UpdateAt, "translation recovery must not edit the post")
				return
			}
		case <-timer.C:
			t.Fatal("non-member websocket did not receive the barrier")
		}
	}
}
