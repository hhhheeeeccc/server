package inactive_users_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattermost/enterprise"
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

func TestEnormousInactivityPeriodDoesNotDeactivateUsers(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense()))

	user := th.CreateUser(t)
	require.NoError(t, th.Store.Status().SaveOrUpdate(&model.Status{
		UserId: user.Id, Status: model.StatusOffline, LastActivityAt: 1,
	}))
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"days":1,"unexpected":true}`, http.StatusBadRequest},
		{`{"days":1} true`, http.StatusBadRequest},
		{`{"days":1}` + strings.Repeat(" ", 4096), http.StatusRequestEntityTooLarge},
	} {
		response, err := th.SystemAdminClient.DoAPIPost(th.Context.Context(), "/users/deactivate/inactive", tc.body)
		require.Error(t, err)
		require.Equal(t, tc.status, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
	var deleteAt int64
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRowContext(th.Context.Context(),
		`SELECT deleteat FROM users WHERE id = $1`, user.Id).Scan(&deleteAt))
	require.Zero(t, deleteAt)

	// On 64-bit systems, multiplying MaxInt by milliseconds per day used to
	// wrap to -86400000 and deactivate every non-admin user.
	maxDays := int(^uint(0) >> 1)
	response, err := th.SystemAdminClient.DoAPIPost(th.Context.Context(),
		"/users/deactivate/inactive", `{"days":`+strconv.Itoa(maxDays)+`}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var result struct {
		Count int64 `json:"count"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
	require.NoError(t, response.Body.Close())
	require.Zero(t, result.Count)

	require.NoError(t, th.Store.GetInternalMasterDB().QueryRowContext(th.Context.Context(),
		`SELECT deleteat FROM users WHERE id = $1`, user.Id).Scan(&deleteAt))
	require.Zero(t, deleteAt)
}
