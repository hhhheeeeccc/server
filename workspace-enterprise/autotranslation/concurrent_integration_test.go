package autotranslation

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

// The first edit has committed before its provider request is released. A second
// native HTTP edit and its websocket event must finish while it is still held.
func TestConcurrentEditDelayedEventRetainsEarlierVersion(t *testing.T) {
	for _, fixedClock := range []bool{false, true} {
		t.Run(strconv.FormatBool(fixedClock), func(t *testing.T) {
			testConcurrentEditDelivery(t, fixedClock)
		})
	}
}

func testConcurrentEditDelivery(t *testing.T, fixedClock bool) {
	started, release := make(chan struct{}), make(chan struct{})
	releaseOld := sync.OnceFunc(func() { close(release) })
	const oldText = "First edit is delayed at the translation provider."
	const currentText = "Second edit completes before the first provider response."
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
			return
		}
		var body translateRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "invalid test provider request", http.StatusBadRequest)
			return
		}
		if body.Text == oldText {
			close(started)
			<-release
			_, _ = w.Write([]byte(`{"translatedText":"OLD delayed translation"}`))
			return
		}
		_, _ = w.Write([]byte(`{"translatedText":"CURRENT translation"}`))
	}))
	t.Cleanup(provider.Close)
	t.Cleanup(releaseOld)
	th := setupRecovery(t, provider.URL)
	post, _, err := th.Client.CreatePost(th.Context.Context(), &model.Post{
		ChannelId: th.BasicChannel.Id, Message: "Initial source before concurrent edits.",
	})
	require.NoError(t, err)
	if fixedClock {
		installFixedPostClock(t, th.Store.GetInternalMasterDB())
	}
	socket := th.CreateConnectedWebSocketClientWithClient(t, th.Client)
	type result struct {
		post *model.Post
		err  error
	}
	finished := make(chan result, 1)
	go func() {
		edited, _, editErr := th.Client.PatchPost(th.Context.Context(), post.Id, &model.PostPatch{Message: new(oldText)})
		finished <- result{edited, editErr}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first edit never reached its held provider request")
	}
	current, _, err := th.Client.PatchPost(th.Context.Context(), post.Id, &model.PostPatch{Message: new(currentText)})
	require.NoError(t, err)
	currentEvent := nextLivePost(t, socket, model.WebsocketEventPostEdited, post.Id)
	require.Equal(t, currentText, currentEvent.Message)
	require.Equal(t, "CURRENT translation", currentEvent.Metadata.Translations["ar"].Text)
	releaseOld()
	select {
	case response := <-finished:
		require.NoError(t, response.err)
		require.Equal(t, oldText, response.post.Message)
	case <-time.After(5 * time.Second):
		t.Fatal("first edit did not complete after releasing the provider")
	}
	oldEvent := nextLivePost(t, socket, model.WebsocketEventPostEdited, post.Id)
	require.Equal(t, oldText, oldEvent.Message)
	require.Empty(t, oldEvent.Metadata.Translations, "the superseded provider result must not be broadcast")
	require.Less(t, oldEvent.UpdateAt, currentEvent.UpdateAt,
		"the existing webapp reducer can reject a delayed event only if its version is older")
	require.Equal(t, current.UpdateAt, currentEvent.UpdateAt)
	stored, err := th.Store.Post().GetSingle(th.Context, post.Id, false)
	require.NoError(t, err)
	require.Equal(t, currentText, stored.Message)
	translation, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, "CURRENT translation", translation.Text)
	t.Logf("arrival order: current(update_at=%d), delayed(update_at=%d); source and translation preserved", currentEvent.UpdateAt, oldEvent.UpdateAt)
}

// A fixed database clock makes the millisecond boundary deterministic. It runs
// before the production lifecycle trigger, rather than relying on fast requests.
func TestConcurrentEditClockCollisionUsesCommittedTimestamp(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := saveForegroundPost(t, th, "Initial clock collision source.")
	db := th.Store.GetInternalMasterDB()
	stamp := installFixedPostClock(t, db)
	first := post.Clone()
	first.Message = "First source in the same database millisecond."
	first, err := th.Server.Store().Post().Update(th.Context, first, post.Clone())
	require.NoError(t, err)
	firstStored, err := th.Store.Post().GetSingle(th.Context, post.Id, false)
	require.NoError(t, err)
	second := first.Clone()
	second.Message = "Second source in the same database millisecond."
	second, err = th.Server.Store().Post().Update(th.Context, second, first.Clone())
	require.NoError(t, err)
	stored, err := th.Store.Post().GetSingle(th.Context, post.Id, false)
	require.NoError(t, err)
	require.Greater(t, stored.UpdateAt, firstStored.UpdateAt, "the database must strictly order colliding source edits")
	require.Greater(t, second.UpdateAt, first.UpdateAt, "committed edits need a strict order even when the clock collides")
	require.Equal(t, stored.UpdateAt, second.UpdateAt, "native websocket objects must carry the committed timestamp")
	require.Equal(t, stamp, first.UpdateAt)
	require.Equal(t, stamp+1, second.UpdateAt)
}

func installFixedPostClock(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	stamp := time.Now().Add(time.Minute).UnixMilli()
	name := pq.QuoteIdentifier("aaa_workspace_test_fixed_post_clock_" + model.NewId())
	_, err := db.Exec(`CREATE FUNCTION ` + name + `() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN NEW.updateat=TG_ARGV[0]::bigint; RETURN NEW; END $$`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON posts; DROP FUNCTION ` + name + `()`)
		require.NoError(t, err)
	})
	_, err = db.Exec(`CREATE TRIGGER ` + name + ` BEFORE UPDATE OF message ON posts
		FOR EACH ROW EXECUTE FUNCTION ` + name + `('` + strconv.FormatInt(stamp, 10) + `')`)
	require.NoError(t, err)
	return stamp
}
