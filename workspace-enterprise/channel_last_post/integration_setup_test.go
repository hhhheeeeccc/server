package channel_last_post_test

import (
	"testing"

	_ "github.com/mattermost/enterprise"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
)

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}
