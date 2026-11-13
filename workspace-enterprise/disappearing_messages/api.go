package disappearing_messages

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

func apiError(status int, message string) *model.AppError {
	err := model.NewAppError("ConversationExpiry", "ent.conversation_expiry.error", nil, "", status)
	err.Message, err.SkipTranslation = message, true
	return err
}

func (s *Service) registerAPI() {
	s.app.Srv().Router.Handle(model.APIURLSuffix+"/workspace/channels/{channel_id:[A-Za-z0-9]+}/disappearing", &web.Handler{
		Srv: s.app.Srv(), HandleFunc: s.channelPolicy, HandlerName: "ConversationExpiryPolicy", RequireSession: true, RequireMfa: true,
	}).Methods(http.MethodGet, http.MethodPut)
}

func (s *Service) authorize(rctx request.CTX, channelID, userID string) *model.AppError {
	if !s.available() {
		return apiError(http.StatusForbidden, "الرسائل المختفية تحتاج إلى ترخيص المؤسسة المفعّل.")
	}
	if !model.IsValidId(channelID) {
		return apiError(http.StatusBadRequest, "معرّف المحادثة غير صالح.")
	}
	channel, err := s.app.GetChannel(rctx, channelID)
	if err != nil || channel.Type != model.ChannelTypeDirect || channel.DeleteAt != 0 || model.SafeDereference(channel.Shared) {
		return apiError(http.StatusForbidden, "الرسائل المختفية متاحة داخل المحادثات الفردية فقط.")
	}
	if _, err := s.app.GetChannelMember(rctx, channelID, userID); err != nil {
		return apiError(http.StatusForbidden, "لا تملك صلاحية الوصول إلى المحادثة.")
	}
	return nil
}

func (s *Service) channelPolicy(c *web.Context, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	channelID, userID := mux.Vars(r)["channel_id"], c.AppContext.Session().UserId
	if c.Err = s.authorize(c.AppContext, channelID, userID); c.Err != nil {
		return
	}
	if r.Method == http.MethodPut {
		if !s.canManagePolicy(c.AppContext) {
			c.Err = apiError(http.StatusForbidden, "لا تملك صلاحية إدارة الرسائل ذاتية الاختفاء.")
			return
		}
		var body struct {
			DurationSeconds *int64 `json:"duration_seconds"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			c.SetInvalidParam("duration_seconds")
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF || body.DurationSeconds == nil || validateDuration(*body.DurationSeconds) != nil {
			c.Err = apiError(http.StatusBadRequest, "اختر إيقاف أو 24 ساعة أو 7 أيام أو 90 يومًا.")
			return
		}
		p := Policy{DurationSeconds: *body.DurationSeconds}
		audit := c.MakeAuditRecord("setConversationExpiry", model.AuditStatusFail)
		defer c.LogAuditRec(audit)
		audit.AddMeta("channel_id", channelID)
		audit.AddMeta("duration_seconds", p.DurationSeconds)
		if err := s.repo.setPolicy(r.Context(), channelID, userID, p); err != nil {
			if errors.Is(err, errForbidden) {
				c.Err = apiError(http.StatusForbidden, "الميزة متاحة بين مستخدمين بشريين نشطين فقط.")
			} else {
				c.Err = apiError(http.StatusServiceUnavailable, "تعذّر حفظ إعداد الرسائل المختفية.").Wrap(err)
			}
			return
		}
		audit.Success()
		s.announce(c.AppContext, channelID, userID, p)
	}
	p, err := s.repo.policy(r.Context(), channelID)
	if err != nil {
		c.Err = apiError(http.StatusServiceUnavailable, "تعذّرت قراءة إعداد الرسائل المختفية.").Wrap(err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p)
}

func (s *Service) announce(rctx request.CTX, channelID, userID string, p Policy) {
	message := "عُطّلت الرسائل المختفية للرسائل الجديدة. الرسائل السابقة تحتفظ بموعد اختفائها."
	if p.DurationSeconds > 0 {
		message = fmt.Sprintf("فُعّلت الرسائل المختفية: تختفي الرسائل الجديدة بعد %s من وقت إرسالها، سواء قُرئت أم لا، على جميع الأجهزة. الرسائل السابقة لا تتغير.", durationLabel(p.DurationSeconds))
	}
	if _, _, err := s.app.CreatePostMissingChannel(rctx, &model.Post{
		ChannelId: channelID, UserId: userID, Type: model.PostTypeSystemGeneric, Message: message,
		Props: model.StringInterface{"workspace_disappearing_duration_seconds": p.DurationSeconds},
	}, false, false); err != nil {
		rctx.Logger().Warn("Conversation expiry saved but notice failed", mlog.String("channel_id", channelID), mlog.Err(err))
	}
}
