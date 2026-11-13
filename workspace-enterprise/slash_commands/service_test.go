package slash_commands

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost/server/public/model"
)

func TestNormalizeCommandLine(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		root     string
		expected string
	}{
		{name: "secure connection", input: "/اتصال_آمن قبول --اسم الموقع --كلمة-المرور سري", root: "secure-connection", expected: "/secure-connection accept --name الموقع --password سري"},
		{name: "share yes", input: "/مشاركة_المجموعة دعوة --معرف-الاتصال abc --للقراءة-فقط نعم", root: "share-channel", expected: "/share-channel invite --connectionID abc --readonly yes"},
		{name: "legacy share", input: "/مشاركة-المجموعة إلغاء-الدعوة --معرف-الاتصال abc", root: "share-channel", expected: "/share-channel uninvite --connectionID abc"},
		{name: "simple", input: "/بالخارج", root: "away", expected: "/away"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.expected, normalizeCommandLine(test.input, test.root))
		})
	}
}

func TestNormalizeMessage(t *testing.T) {
	require.Equal(t, "clear", normalizeMessage("status", "مسح"))
	require.Equal(t, "latest", normalizeMessage("exportlink", "الأحدث"))
	require.Equal(t, "نص عربي", normalizeMessage("me", "نص عربي"))
}

func TestLocalizeAutocompleteDataDoesNotMutateSource(t *testing.T) {
	source := model.NewAutocompleteData("secure-connection", "[action]", "help")
	accept := model.NewAutocompleteData("accept", "", "accept help")
	accept.AddNamedTextArgument("password", "password help", "password", "", true)
	remove := model.NewAutocompleteData("remove", "", "remove help")
	remove.AddNamedDynamicListArgument("connectionID", "connection help", "builtin:secure-connection", true)
	source.AddCommand(accept)
	source.AddCommand(remove)

	clone := cloneAutocompleteData(source)
	localizeAutocompleteData(clone, "secure-connection", "اتصال_آمن", true)

	require.Equal(t, "secure-connection", source.Trigger)
	require.Equal(t, "password", source.SubCommands[0].Arguments[0].Name)
	require.Equal(t, "builtin:secure-connection", source.SubCommands[1].Arguments[0].Data.(*model.AutocompleteDynamicListArg).FetchURL)
	require.Equal(t, "اتصال_آمن", clone.Trigger)
	require.Equal(t, "قبول", clone.SubCommands[0].Trigger)
	require.Equal(t, "كلمة-المرور", clone.SubCommands[0].Arguments[0].Name)
	require.Equal(t, "builtin:اتصال_آمن", clone.SubCommands[1].Arguments[0].Data.(*model.AutocompleteDynamicListArg).FetchURL)
	require.Equal(t, "[إجراء]", clone.Hint)

	share := model.NewAutocompleteData("share-channel", "[action]", "help")
	invite := model.NewAutocompleteData("invite", "", "invite help")
	invite.AddNamedTextArgument("readonly", "readonly help", "readonly", "Y|N|y|n", false)
	share.AddCommand(invite)
	localizeAutocompleteData(share, "share-channel", "مشاركة_المجموعة", true)
	require.Equal(t, "[إجراء]", share.Hint)
	require.Equal(t, "للقراءة-فقط", share.SubCommands[0].Arguments[0].Name)
	require.Equal(t, "Y|N|y|n|نعم|لا", share.SubCommands[0].Arguments[0].Data.(*model.AutocompleteTextArg).Pattern)
}
