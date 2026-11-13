package disappearing_messages

import (
	"fmt"
	"strings"
	"sync"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/i18n"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

var registerCommands sync.Once

func RegisterCommands() {
	registerCommands.Do(func() {
		for _, trigger := range []string{"اختفاء", "disappear"} {
			app.RegisterCommandProvider(&command{trigger})
		}
	})
}

type command struct{ trigger string }

func (p *command) GetTrigger() string { return p.trigger }

func (p *command) GetCommand(a *app.App, _ i18n.TranslateFunc) *model.Command {
	value, ok := services.Load(a.Srv())
	return &model.Command{Trigger: p.trigger, DisplayName: "الرسائل المختفية", AutoComplete: ok && value.(*Service).available(), AutoCompleteDesc: "اختفاء الرسائل الجديدة من وقت الإرسال", AutoCompleteHint: "[24h | 7d | 90d | إيقاف | حالة]"}
}

func (p *command) DoCommand(a *app.App, rctx request.CTX, args *model.CommandArgs, text string) *model.CommandResponse {
	reply := func(text string) *model.CommandResponse {
		return &model.CommandResponse{ResponseType: model.CommandResponseTypeEphemeral, Text: text}
	}
	value, ok := services.Load(a.Srv())
	if !ok {
		return reply("لم تُجهّز خدمة الرسائل المختفية على السيرفر. راجع سجل بدء التشغيل؛ لن تُرسل رسالتك كأمر عادي.")
	}
	if args == nil || rctx.Session() == nil || args.UserId != rctx.Session().UserId || args.RootId != "" {
		return reply("استخدم الأمر داخل المحادثة الفردية الأساسية بجلسة دخول صالحة.")
	}
	s := value.(*Service)
	if err := s.authorize(rctx, args.ChannelId, args.UserId); err != nil {
		return reply(err.Message)
	}
	text = strings.TrimSpace(text)
	if text == "" || text == "حالة" || text == "status" {
		policy, err := s.repo.policy(rctx.Context(), args.ChannelId)
		if err != nil {
			return reply("تعذّرت قراءة إعداد المحادثة.")
		}
		return reply(fmt.Sprintf("إعداد الرسائل المختفية: %s. يبدأ الوقت من الإرسال، وليس القراءة. للتغيير: `/اختفاء 24h` أو `/اختفاء 7d` أو `/اختفاء 90d` أو `/اختفاء إيقاف`.", durationLabel(policy.DurationSeconds)))
	}
	if !s.canManagePolicy(rctx) {
		return reply("لا تملك صلاحية إدارة الرسائل ذاتية الاختفاء. اطلب من مدير النظام منحك الصلاحية.")
	}
	seconds, err := parseDuration(text)
	if err != nil {
		return reply("اختر `/اختفاء 24h` أو `/اختفاء 7d` أو `/اختفاء 90d` أو `/اختفاء إيقاف`. المدة تبدأ من الإرسال، وليست عدد ثوانٍ بعد القراءة.")
	}
	policy := Policy{seconds}
	if err := s.repo.setPolicy(rctx.Context(), args.ChannelId, args.UserId, policy); err != nil {
		return reply("تعذّر حفظ الإعداد. الميزة متاحة بين مستخدمين بشريين نشطين فقط.")
	}
	s.announce(rctx, args.ChannelId, args.UserId, policy)
	return reply("حُفظ إعداد الرسائل المختفية: " + durationLabel(seconds) + ". يسري على الرسائل الجديدة فقط؛ الرسائل السابقة تحتفظ بموعدها.")
}
