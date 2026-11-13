package slash_commands

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/i18n"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"

	"github.com/mattermost/enterprise/localization"
)

type commandDefinition struct {
	english string
	arabic  string
	visible bool
}

var canonicalCommands = []commandDefinition{
	{english: "away", arabic: "بالخارج", visible: true},
	{english: "collapse", arabic: "طي", visible: true},
	{english: "code", arabic: "كود", visible: true},
	{english: "dnd", arabic: "عدم_الإزعاج", visible: true},
	{english: "echo", arabic: "ترديد", visible: true},
	{english: "expand", arabic: "توسيع", visible: true},
	{english: "exportlink", arabic: "رابط_تصدير", visible: true},
	{english: "groupmsg", arabic: "رسالة_جماعية", visible: true},
	{english: "header", arabic: "ترويسة", visible: true},
	{english: "help", arabic: "مساعدة", visible: true},
	{english: "invite", arabic: "دعوة", visible: true},
	{english: "invite_people", arabic: "دعوة_أشخاص", visible: true},
	{english: "join", arabic: "انضمام", visible: true},
	{english: "kick", arabic: "طرد", visible: true},
	{english: "leave", arabic: "مغادرة", visible: true},
	{english: "logout", arabic: "تسجيل_الخروج", visible: true},
	{english: "marketplace", arabic: "سوق", visible: true},
	{english: "me", arabic: "أنا", visible: true},
	{english: "msg", arabic: "رسالة", visible: true},
	{english: "mute", arabic: "كتم_الصوت", visible: true},
	{english: "offline", arabic: "غير_متصل", visible: true},
	{english: "online", arabic: "متصل", visible: true},
	{english: "open", arabic: "فتح", visible: true},
	{english: "purpose", arabic: "غرض", visible: true},
	{english: "remove", arabic: "إزالة", visible: true},
	{english: "rename", arabic: "إعادة_تسمية", visible: true},
	{english: "search", arabic: "بحث", visible: true},
	{english: "secure-connection", arabic: "اتصال_آمن", visible: true},
	{english: "settings", arabic: "إعدادات", visible: true},
	{english: "share-channel", arabic: "مشاركة_المجموعة", visible: true},
	{english: "shortcuts", arabic: "اختصارات", visible: true},
	{english: "shrug", arabic: "تكتاف", visible: true},
	{english: "status", arabic: "حالة", visible: true},
}

var legacyCommands = []commandDefinition{
	{english: "away", arabic: "غائب"},
	{english: "code", arabic: "رمز"},
	{english: "dnd", arabic: "عدم-الإزعاج"},
	{english: "echo", arabic: "صدى"},
	{english: "exportlink", arabic: "رابط-التصدير"},
	{english: "groupmsg", arabic: "رسالة-جماعية"},
	{english: "invite_people", arabic: "دعوة-أشخاص"},
	{english: "logout", arabic: "خروج"},
	{english: "marketplace", arabic: "السوق"},
	{english: "mute", arabic: "كتم"},
	{english: "offline", arabic: "غير-متصل"},
	{english: "purpose", arabic: "الغرض"},
	{english: "rename", arabic: "إعادة-تسمية"},
	{english: "secure-connection", arabic: "اتصال-آمن"},
	{english: "share-channel", arabic: "مشاركة-المجموعة"},
	{english: "shrug", arabic: "لامبالاة"},
}

var subcommandAliases = map[string]map[string]string{
	"secure-connection": {
		"إنشاء": "create",
		"قبول":  "accept",
		"إزالة": "remove",
		"حالة":  "status",
	},
	"share-channel": {
		"مشاركة":         "share",
		"دعوة":           "invite",
		"إلغاء-الدعوة":   "uninvite",
		"إلغاء-المشاركة": "unshare",
		"حالة":           "status",
	},
}

var arabicSubcommands = map[string]map[string]string{
	"secure-connection": {
		"create": "إنشاء",
		"accept": "قبول",
		"remove": "إزالة",
		"status": "حالة",
	},
	"share-channel": {
		"share":    "مشاركة",
		"invite":   "دعوة",
		"uninvite": "إلغاء-الدعوة",
		"unshare":  "إلغاء-المشاركة",
		"status":   "حالة",
	},
}

var argumentAliases = map[string]string{
	"اسم":           "name",
	"الاسم-المعروض": "displayname",
	"كلمة-المرور":   "password",
	"الدعوة":        "invite",
	"معرف-الاتصال":  "connectionID",
	"للقراءة-فقط":   "readonly",
}

var arabicArguments = map[string]string{
	"name":         "اسم",
	"displayname":  "الاسم-المعروض",
	"password":     "كلمة-المرور",
	"invite":       "الدعوة",
	"connectionID": "معرف-الاتصال",
	"readonly":     "للقراءة-فقط",
}

var registerOnce sync.Once

// Register adds the Arabic built-in aliases from enterprise/main.go after the
// original Mattermost command providers have been registered.
func Register() {
	registerOnce.Do(func() {
		for _, definition := range canonicalCommands {
			app.RegisterCommandProvider(&provider{definition: definition})
		}
		for _, definition := range legacyCommands {
			app.RegisterCommandProvider(&provider{definition: definition})
		}
	})
}

type provider struct {
	definition commandDefinition
}

var _ app.CommandProvider = (*provider)(nil)
var _ app.AutocompleteDynamicArgProvider = (*provider)(nil)

func (p *provider) GetTrigger() string {
	return p.definition.arabic
}

func (p *provider) GetCommand(a *app.App, T i18n.TranslateFunc) *model.Command {
	if a == nil || a.License() == nil {
		return nil
	}

	delegate := app.GetCommandProvider(p.definition.english)
	if delegate == nil {
		return nil
	}
	translate := localization.ArabicTranslateFunc(T)
	command := delegate.GetCommand(a, translate)
	if command == nil {
		return nil
	}

	command = cloneCommand(command)
	command.Trigger = p.definition.arabic
	command.AutoComplete = p.definition.visible
	localizeAutocompleteData(command.AutocompleteData, p.definition.english, p.definition.arabic, true)
	return command
}

func (p *provider) DoCommand(a *app.App, rctx request.CTX, args *model.CommandArgs, message string) *model.CommandResponse {
	if a == nil || a.License() == nil || args == nil {
		return nil
	}

	delegate := app.GetCommandProvider(p.definition.english)
	if delegate == nil {
		return nil
	}

	normalizedArgs := *args
	normalizedArgs.Command = normalizeCommandLine(args.Command, p.definition.english)
	normalizedArgs.T = localization.ArabicTranslateFunc(args.T)
	return delegate.DoCommand(a, rctx, &normalizedArgs, normalizeMessage(p.definition.english, message))
}

func (p *provider) GetAutoCompleteListItems(rctx request.CTX, a *app.App, commandArgs *model.CommandArgs, arg *model.AutocompleteArg, parsed, toBeParsed string) ([]model.AutocompleteListItem, error) {
	if a == nil || a.License() == nil {
		return nil, fmt.Errorf("enterprise license is required")
	}
	if commandArgs == nil || arg == nil {
		return nil, fmt.Errorf("command autocomplete arguments are required")
	}

	delegate, ok := app.GetCommandProvider(p.definition.english).(app.AutocompleteDynamicArgProvider)
	if !ok {
		return nil, fmt.Errorf("auto-completion not available for built-in command %s", p.definition.english)
	}

	normalizedArgs := *commandArgs
	normalizedArgs.Command = normalizeCommandLine(commandArgs.Command, p.definition.english)
	normalizedArgs.T = localization.ArabicTranslateFunc(commandArgs.T)
	normalizedArg := cloneAutocompleteArg(arg)
	if english, ok := argumentAliases[normalizedArg.Name]; ok {
		normalizedArg.Name = english
	}
	if dynamic, ok := normalizedArg.Data.(*model.AutocompleteDynamicListArg); ok {
		dynamic.FetchURL = "builtin:" + p.definition.english
	}

	items, err := delegate.GetAutoCompleteListItems(
		rctx,
		a,
		&normalizedArgs,
		normalizedArg,
		normalizeCommandLine(parsed, p.definition.english),
		normalizeNamedArguments(toBeParsed),
	)
	if err != nil && p.definition.english == "secure-connection" && strings.Contains(err.Error(), "manage_secure_connections") {
		return nil, errors.New("تحتاج إلى إذن إدارة الاتصالات الآمنة")
	}
	return items, err
}

func normalizeCommandLine(command, englishRoot string) string {
	normalized := replaceToken(command, 0, englishRoot)
	if aliases := subcommandAliases[englishRoot]; len(aliases) != 0 {
		start, end := tokenBounds(normalized, 1)
		if start >= 0 {
			if english, ok := aliases[normalized[start:end]]; ok {
				normalized = normalized[:start] + english + normalized[end:]
			}
		}
	}
	normalized = normalizeNamedArguments(normalized)
	if englishRoot == "share-channel" {
		normalized = normalizeReadonlyValue(normalized)
	}
	return normalized
}

func normalizeNamedArguments(value string) string {
	for arabic, english := range argumentAliases {
		value = replaceNamedArgument(value, arabic, english)
	}
	return value
}

func replaceNamedArgument(value, arabic, english string) string {
	needle := "--" + arabic
	for offset := 0; offset < len(value); {
		index := strings.Index(value[offset:], needle)
		if index < 0 {
			break
		}
		index += offset
		end := index + len(needle)
		if end == len(value) || unicode.IsSpace(rune(value[end])) || value[end] == '=' {
			value = value[:index] + "--" + english + value[end:]
			offset = index + len(english) + 2
			continue
		}
		offset = end
	}
	return value
}

func normalizeReadonlyValue(value string) string {
	fields := strings.Fields(value)
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] != "--readonly" {
			continue
		}
		switch fields[index+1] {
		case "نعم":
			fields[index+1] = "yes"
		case "لا":
			fields[index+1] = "no"
		}
		return strings.Join(fields, " ")
	}
	return value
}

func normalizeMessage(englishRoot, message string) string {
	trimmed := strings.TrimSpace(message)
	switch englishRoot {
	case "status":
		if trimmed == "مسح" {
			return "clear"
		}
	case "exportlink":
		if trimmed == "الأحدث" {
			return "latest"
		}
	}
	return message
}

func replaceToken(value string, token int, replacement string) string {
	start, end := tokenBounds(value, token)
	if start < 0 {
		return value
	}
	prefix := ""
	if token == 0 && value[start] == '/' {
		prefix = "/"
	}
	return value[:start] + prefix + replacement + value[end:]
}

func tokenBounds(value string, wanted int) (int, int) {
	current := 0
	inToken := false
	start := -1
	for index, char := range value {
		if unicode.IsSpace(char) {
			if inToken {
				if current == wanted {
					return start, index
				}
				current++
				inToken = false
			}
			continue
		}
		if !inToken {
			start = index
			inToken = true
		}
	}
	if inToken && current == wanted {
		return start, len(value)
	}
	return -1, -1
}

func cloneCommand(command *model.Command) *model.Command {
	clone := *command
	clone.AutocompleteData = cloneAutocompleteData(command.AutocompleteData)
	return &clone
}

func cloneAutocompleteData(data *model.AutocompleteData) *model.AutocompleteData {
	if data == nil {
		return nil
	}
	clone := *data
	clone.Arguments = make([]*model.AutocompleteArg, len(data.Arguments))
	for index, argument := range data.Arguments {
		clone.Arguments[index] = cloneAutocompleteArg(argument)
	}
	clone.SubCommands = make([]*model.AutocompleteData, len(data.SubCommands))
	for index, subcommand := range data.SubCommands {
		clone.SubCommands[index] = cloneAutocompleteData(subcommand)
	}
	return &clone
}

func cloneAutocompleteArg(argument *model.AutocompleteArg) *model.AutocompleteArg {
	if argument == nil {
		return nil
	}
	clone := *argument
	switch data := argument.Data.(type) {
	case *model.AutocompleteTextArg:
		dataClone := *data
		clone.Data = &dataClone
	case *model.AutocompleteStaticListArg:
		dataClone := *data
		dataClone.PossibleArguments = append([]model.AutocompleteListItem(nil), data.PossibleArguments...)
		clone.Data = &dataClone
	case *model.AutocompleteDynamicListArg:
		dataClone := *data
		clone.Data = &dataClone
	}
	return &clone
}

func localizeAutocompleteData(data *model.AutocompleteData, englishRoot, arabicRoot string, root bool) {
	if data == nil {
		return
	}
	if root {
		data.Trigger = arabicRoot
		if englishRoot == "secure-connection" || englishRoot == "share-channel" {
			data.Hint = "[إجراء]"
		}
	} else if arabic, ok := arabicSubcommands[englishRoot][data.Trigger]; ok {
		data.Trigger = arabic
	}
	for _, argument := range data.Arguments {
		englishName := argument.Name
		if arabic, ok := arabicArguments[argument.Name]; ok {
			argument.Name = arabic
		}
		if englishRoot == "share-channel" && englishName == "readonly" {
			if text, ok := argument.Data.(*model.AutocompleteTextArg); ok {
				text.Pattern = "Y|N|y|n|نعم|لا"
			}
		}
		if dynamic, ok := argument.Data.(*model.AutocompleteDynamicListArg); ok && strings.HasPrefix(dynamic.FetchURL, "builtin:") {
			dynamic.FetchURL = "builtin:" + arabicRoot
		}
	}
	for _, subcommand := range data.SubCommands {
		localizeAutocompleteData(subcommand, englishRoot, arabicRoot, false)
	}
}
