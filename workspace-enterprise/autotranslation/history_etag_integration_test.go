package autotranslation

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func getConditionalHistory(t *testing.T, th *api4.TestHelper, query, etag string) (*http.Response, *model.PostList) {
	t.Helper()
	response, err := th.Client.DoAPIGet(th.Context.Context(), "/channels/"+th.BasicChannel.Id+"/posts"+query, etag)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	if response.StatusCode == http.StatusNotModified {
		require.Empty(t, body)
		return response, nil
	}
	require.Equal(t, http.StatusOK, response.StatusCode)
	var posts model.PostList
	require.NoError(t, json.Unmarshal(body, &posts))
	return response, &posts
}

func TestHistoryETagTranslationChangesWithUnchangedTimestamp(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := saveForegroundPost(t, th, "Canonical source for conditional history.")
	saveReadTranslation(t, th, post, post.Message, "first translated payload")
	first, posts := getConditionalHistory(t, th, "?page=0&per_page=50", "")
	require.NotNil(t, posts.Posts[post.Id].Metadata.Translations["ar"])
	require.Equal(t, "first translated payload", posts.Posts[post.Id].Metadata.Translations["ar"].Text)
	require.NotEmpty(t, first.Header.Get(model.HeaderEtagServer))
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET text='second translated payload' WHERE objectid=$1`, post.Id)
	require.NoError(t, err)
	th.Store.AutoTranslation().InvalidatePostTranslationEtag(post.ChannelId)
	second, updated := getConditionalHistory(t, th, "?page=0&per_page=50", first.Header.Get(model.HeaderEtagServer))
	require.Equal(t, http.StatusOK, second.StatusCode, "an unchanged timestamp must not conceal a changed translation")
	require.NotNil(t, updated)
	require.Equal(t, "second translated payload", updated.Posts[post.Id].Metadata.Translations["ar"].Text)
}

func TestHistoryETagUserPreferenceChangesRepresentation(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := saveForegroundPost(t, th, "Canonical source after opting out.")
	saveReadTranslation(t, th, post, post.Message, "translated payload before opting out")
	first, posts := getConditionalHistory(t, th, "?page=0&per_page=50", "")
	require.NotEmpty(t, posts.Posts[post.Id].Metadata.Translations)
	_, err := th.Client.UpdateChannelMemberAutotranslation(th.Context.Context(), post.ChannelId, th.BasicUser.Id, true)
	require.NoError(t, err)
	second, updated := getConditionalHistory(t, th, "?page=0&per_page=50", first.Header.Get(model.HeaderEtagServer))
	require.Equal(t, http.StatusOK, second.StatusCode, "a shared channel timestamp must not retain another preference's representation")
	require.NotNil(t, updated)
	require.Empty(t, updated.Posts[post.Id].Metadata.Translations)
}

func TestHistoryETagStableResponsesAndPagination(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	var posts []*model.Post
	for range 4 {
		post := saveForegroundPost(t, th, "Synthetic conditional pagination source.")
		saveReadTranslation(t, th, post, post.Message, "synthetic pagination translation")
		posts = append(posts, post)
	}
	for _, query := range []string{
		"?page=0&per_page=50", "?page=1&per_page=1",
		"?after=" + posts[1].Id, "?before=" + posts[2].Id,
	} {
		t.Run(query, func(t *testing.T) {
			first, list := getConditionalHistory(t, th, query, "")
			etag := first.Header.Get(model.HeaderEtagServer)
			require.True(t, strings.HasPrefix(etag, historyETagPrefix))
			unchanged, cached := getConditionalHistory(t, th, query, `"another-validator", W/`+etag)
			require.Equal(t, http.StatusNotModified, unchanged.StatusCode)
			require.Nil(t, cached)
			require.Equal(t, etag, unchanged.Header.Get(model.HeaderEtagServer))
			require.Equal(t, "private, no-cache", unchanged.Header.Get("Cache-Control"))
			var translatedID string
			for id, post := range list.Posts {
				if post.Metadata != nil && post.Metadata.Translations["ar"] != nil {
					translatedID = id
					break
				}
			}
			require.NotEmpty(t, translatedID)
			_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET text=text||' changed' WHERE objectid=$1`, translatedID)
			require.NoError(t, err)
			updated, body := getConditionalHistory(t, th, query, etag)
			require.Equal(t, http.StatusOK, updated.StatusCode)
			require.NotNil(t, body)
			require.NotEqual(t, etag, updated.Header.Get(model.HeaderEtagServer))
		})
	}
}

func TestHistoryETagReconcilesHistoricalRowsBeforeConditionalMatch(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := saveForegroundPost(t, th, "Canonical history requiring reconciliation.")
	saveReadTranslation(t, th, post, post.Message, "initial current payload")
	first, _ := getConditionalHistory(t, th, "", "")
	saveReadTranslation(t, th, post, "Obsolete historical source.", "private obsolete historical payload")
	second, updated := getConditionalHistory(t, th, "", first.Header.Get(model.HeaderEtagServer))
	require.Equal(t, http.StatusOK, second.StatusCode)
	translation := updated.Posts[post.Id].Metadata.Translations["ar"]
	require.Empty(t, translation.Text)
	require.Equal(t, string(model.TranslationStateProcessing), translation.State)
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Empty(t, stored.Text)
	third, cached := getConditionalHistory(t, th, "", second.Header.Get(model.HeaderEtagServer))
	require.Equal(t, http.StatusNotModified, third.StatusCode)
	require.Nil(t, cached)
}

func TestHistoryETagRevocationAndLocale(t *testing.T) {
	for _, change := range []string{"license", "configuration", "locale"} {
		t.Run(change, func(t *testing.T) {
			th := setupRecovery(t, "http://127.0.0.1:1")
			post := saveForegroundPost(t, th, "Canonical source after a reader state change.")
			saveReadTranslation(t, th, post, post.Message, "translated payload before state change")
			first, posts := getConditionalHistory(t, th, "", "")
			require.NotEmpty(t, posts.Posts[post.Id].Metadata.Translations)
			switch change {
			case "license":
				th.Server.RemoveLicense()
			case "configuration":
				th.App.UpdateConfig(func(cfg *model.Config) { cfg.AutoTranslationSettings.Enable = new(false) })
			case "locale":
				_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE users SET locale='en' WHERE id=$1`, th.BasicUser.Id)
				require.NoError(t, err)
				th.Server.Store().AutoTranslation().InvalidateUserLocaleCache(th.BasicUser.Id)
			}
			second, updated := getConditionalHistory(t, th, "", first.Header.Get(model.HeaderEtagServer))
			require.Equal(t, http.StatusOK, second.StatusCode)
			require.NotNil(t, updated)
			require.Empty(t, updated.Posts[post.Id].Metadata.Translations)
		})
	}
}

func TestHistoryETagDisabledFeatureRetainsNativeCaching(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.AutoTranslationSettings.Enable = new(false) })
	first, _ := getConditionalHistory(t, th, "", "")
	etag := first.Header.Get(model.HeaderEtagServer)
	require.NotEmpty(t, etag)
	require.NotContains(t, etag, historyETagPrefix)
	second, cached := getConditionalHistory(t, th, "", etag)
	require.Equal(t, http.StatusNotModified, second.StatusCode)
	require.Nil(t, cached)
}

func TestHistoryETagConditionalRequestsStillRequireAuthenticationAndPermission(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	th.BasicChannel = th.CreatePrivateChannel(t)
	channel := th.BasicChannel.DeepCopy()
	channel.AutoTranslation = true
	_, appErr := th.App.UpdateChannel(th.Context, channel)
	require.Nil(t, appErr)
	post := saveForegroundPost(t, th, "Private conditional history source.")
	saveReadTranslation(t, th, post, post.Message, "private translation")
	first, _ := getConditionalHistory(t, th, "", "")
	other := th.CreateClient()
	th.LoginBasic2WithClient(t, other)
	anonymous := model.NewAPIv4Client(th.Client.URL)
	for name, client := range map[string]*model.Client4{"nonmember": other, "anonymous": anonymous} {
		t.Run(name, func(t *testing.T) {
			response, err := client.DoAPIGet(th.Context.Context(), "/channels/"+post.ChannelId+"/posts", first.Header.Get(model.HeaderEtagServer))
			require.Error(t, err)
			require.NotNil(t, response)
			defer response.Body.Close()
			require.Contains(t, []int{http.StatusForbidden, http.StatusUnauthorized}, response.StatusCode)
			// Client4 consumes and closes error bodies while decoding AppError.
			require.NotContains(t, err.Error(), "private translation")
		})
	}
}
