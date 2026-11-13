package global_relay_export

import (
	"io"
	"testing"

	"github.com/mattermost/enterprise/message_export/shared"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

func TestGetParticipantEmailsSkipsEmptyAddresses(t *testing.T) {
	channel := &ChannelExport{Participants: []ParticipantRow{
		{JoinExport: shared.JoinExport{UserEmail: ""}},
		{JoinExport: shared.JoinExport{UserEmail: "first@example.com"}},
		{JoinExport: shared.JoinExport{UserEmail: ""}},
		{JoinExport: shared.JoinExport{UserEmail: "second@example.com"}},
	}}

	require.Equal(t, []string{"first@example.com", "second@example.com"}, getParticipantEmails(channel))
}

func TestGenerateEmailWithoutParticipantEmailReturnsError(t *testing.T) {
	channel := &ChannelExport{Participants: []ParticipantRow{
		{JoinExport: shared.JoinExport{UserEmail: ""}},
	}}

	_, err := generateEmail(request.TestContext(t), nil, channel, nil, io.Discard)
	require.EqualError(t, err, "cannot generate Global Relay email without a participant email address")
}

func TestAddPostToChannelExportHandlesMissingAndInvalidProps(t *testing.T) {
	userID := "user-id"
	username := "original"
	channel := &ChannelExport{numUserMessages: map[string]int{}}
	post := shared.PostExport{MessageExport: model.MessageExport{UserId: &userID, Username: &username}}

	require.NotPanics(t, func() {
		addPostToChannelExport(request.TestContext(t), channel, post)
	})
	require.Equal(t, "original", channel.Messages[0].PostUsername)

	invalidProps := `{"override_username":42,"webhook_display_name":true}`
	post.PostProps = &invalidProps
	require.NotPanics(t, func() {
		addPostToChannelExport(request.TestContext(t), channel, post)
	})
	require.Equal(t, "original", channel.Messages[1].PostUsername)
}
