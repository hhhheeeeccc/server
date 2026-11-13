package global_relay_export

import (
	"html/template"
	"testing"

	"github.com/mattermost/enterprise/message_export/shared"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/platform/shared/templates"
	"github.com/stretchr/testify/require"
)

func TestChannelExportPreservesEqualTimeMessageOrder(t *testing.T) {
	views := templates.NewFromTemplate(template.Must(template.New("relay").Parse(`
{{define "globalrelay_compliance_export_message"}}{{.Props.PostId}};{{end}}
{{define "globalrelay_compliance_export"}}{{.Props.Messages}}{{end}}`)))
	for _, test := range []struct {
		name     string
		messages []Message
		expected string
	}{
		{
			name:     "ordinary ties retain source order",
			messages: []Message{{Id: "first", SentTime: 10, Message: "First"}, {Id: "second", SentTime: 10, Message: "Second"}, {Id: "third", SentTime: 10, Message: "Third"}},
			expected: "first;second;third;",
		},
		{
			name:     "ordinary messages precede same-time file markers and edits",
			messages: []Message{{Id: "upload", SentTime: 10, Message: "Uploaded file file.txt"}, {Id: "first", SentTime: 10, Message: "First"}, {Id: "edit", SentTime: 10, Message: "Edited", UpdateType: shared.EditedNewMsg}, {Id: "second", SentTime: 10, Message: "Second"}, {Id: "delete", SentTime: 10, Message: "Deleted file file.txt"}},
			expected: "first;second;upload;edit;delete;",
		},
		{
			name:     "timestamp takes priority over message category",
			messages: []Message{{Id: "new", SentTime: 20, Message: "New"}, {Id: "old-file", SentTime: 1, Message: "Uploaded file old.txt"}, {Id: "old", SentTime: 1, Message: "Old"}},
			expected: "old;old-file;new;",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			channel := &ChannelExport{Messages: test.messages}
			for range 2 {
				rendered, err := channelExportToHTML(request.TestContext(t), channel, views)
				require.NoError(t, err)
				require.Equal(t, test.expected, rendered)
			}
		})
	}
}
