package autotranslation

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestHistorySinceNonpositiveValuesUseConditionalPageProtection(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	post := saveForegroundPost(t, th, "Canonical source for since boundaries.")
	saveReadTranslation(t, th, post, post.Message, "initial since-boundary translation")
	for i, query := range []string{
		"?since=0", "?since=-1", "?since=-9223372036854775808",
		"?since=", "?since", "?since=%2B0", "?since=0000",
		"?since=0&since=10", "?since=&since=10",
	} {
		t.Run(query, func(t *testing.T) {
			first, before := getConditionalHistory(t, th, query, "")
			require.NotEmpty(t, before.Posts[post.Id].Metadata.Translations["ar"].Text)
			changed := fmt.Sprintf("fresh boundary translation %d", i)
			_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE translations SET text=$1 WHERE objectid=$2`, changed, post.Id)
			require.NoError(t, err)
			second, after := getConditionalHistory(t, th, query, first.Header.Get(model.HeaderEtagServer))
			require.Equal(t, http.StatusOK, second.StatusCode, "the native since<=0 page branch must not retain old translated text")
			require.NotNil(t, after)
			require.Equal(t, changed, after.Posts[post.Id].Metadata.Translations["ar"].Text)
			third, cached := getConditionalHistory(t, th, query, second.Header.Get(model.HeaderEtagServer))
			require.Equal(t, http.StatusNotModified, third.StatusCode)
			require.Nil(t, cached)
		})
	}
}

func TestHistorySincePositiveAndInvalidValuesKeepNativeSemantics(t *testing.T) {
	th := setupRecovery(t, "http://127.0.0.1:1")
	for _, query := range []string{"?since=1", "?since=9223372036854775807", "?since=1&since=0"} {
		t.Run(query, func(t *testing.T) {
			response, _ := getConditionalHistory(t, th, query, "*")
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.Empty(t, response.Header.Get(model.HeaderEtagServer))
		})
	}
	for _, value := range []string{"broken", "9223372036854775808", "-9223372036854775809", "%20", "1.5"} {
		t.Run(value, func(t *testing.T) {
			response, err := th.Client.DoAPIGet(th.Context.Context(), "/channels/"+th.BasicChannel.Id+"/posts?since="+value, "*")
			require.Error(t, err)
			require.NotNil(t, response)
			defer response.Body.Close()
			require.Equal(t, http.StatusBadRequest, response.StatusCode)
		})
	}
}
