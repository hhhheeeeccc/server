package disappearing_messages

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/stretchr/testify/require"
)

func TestUnavailableCommandIsRecognized(t *testing.T) {
	a := app.New(app.ServerConnector(&app.Channels{}))
	for _, trigger := range []string{"اختفاء", "disappear"} {
		p := &command{trigger}
		cmd := p.GetCommand(a, nil)
		require.NotNil(t, cmd)
		require.Equal(t, trigger, cmd.Trigger)
		require.False(t, cmd.AutoComplete)
		reply := p.DoCommand(a, request.EmptyContext(nil), &model.CommandArgs{}, "حالة")
		require.Equal(t, model.CommandResponseTypeEphemeral, reply.ResponseType)
		require.Contains(t, reply.Text, "لم تُجهّز")
	}
}
