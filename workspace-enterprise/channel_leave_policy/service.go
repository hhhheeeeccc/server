package channel_leave_policy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

type application interface {
	Log() *mlog.Logger
	GetSession(string) (*model.Session, *model.AppError)
	GetUser(request.CTX, string) (*model.User, *model.AppError)
	GetChannel(request.CTX, string) (*model.Channel, *model.AppError)
	GetTeamMember(request.CTX, string, string) (*model.TeamMember, *model.AppError)
}

type Service struct{ app application }

// Register adds a guard to the existing HTTP routes without replacing core
// authentication, CSRF checks, membership removal, audit, or websocket handling.
// The access restriction is enforced without a license as requested; losing a
// license must not restore ordinary members' ability to leave a channel.
func Register(a *app.App) {
	s := &Service{app: a}
	a.Srv().Router.Use(s.guard)
}

func (s *Service) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		channelID, targetID, matched := leaveRequest(r)
		commandRequest := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, model.APIURLSuffix+"/commands/execute")
		if !matched && !commandRequest {
			next.ServeHTTP(w, r)
			return
		}
		token, _ := app.ParseAuthTokenFromRequest(r)
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		session, err := s.app.GetSession(token)
		if err != nil || session == nil || !model.IsValidId(session.UserId) {
			// The original route owns the authentication error response.
			next.ServeHTTP(w, r)
			return
		}
		if matched && targetID != "me" && targetID != session.UserId {
			// Removing another member retains the core permission checks.
			next.ServeHTTP(w, r)
			return
		}
		rctx := request.EmptyContext(s.app.Log()).WithContext(r.Context()).
			With(app.RequestContextWithMaster).WithSession(session)
		var policyErr *model.AppError
		if commandRequest {
			args, ok := readCommand(r)
			if ok {
				trigger, message := splitCommand(args.Command)
				policyErr = checkInvocation(s.app, rctx, session.UserId, args.ChannelId, trigger, message)
			}
		} else {
			policyErr = check(s.app, rctx, session.UserId, channelID)
		}
		if err := policyErr; err != nil {
			err.RequestId = model.NewId()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set(model.HeaderRequestId, err.RequestId)
			w.WriteHeader(err.StatusCode)
			_, _ = w.Write([]byte(err.ToJSON()))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Inspect a bounded copy while retaining the exact body and its original Close
// for the core handler. Oversized or invalid payloads keep the core validation;
// the provider guard also protects direct and oversized command executions.
func readCommand(r *http.Request) (*model.CommandArgs, bool) {
	if r.Body == nil {
		return nil, false
	}
	original := r.Body
	const limit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(original, limit+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), original), original}
	if err != nil || len(body) > limit {
		return nil, false
	}
	var args model.CommandArgs
	if json.NewDecoder(bytes.NewReader(body)).Decode(&args) != nil || !model.IsValidId(args.ChannelId) {
		return nil, false
	}
	return &args, true
}

func leaveRequest(r *http.Request) (string, string, bool) {
	if r.Method != http.MethodDelete {
		return "", "", false
	}
	parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")
	if len(parts) < 7 {
		return "", "", false
	}
	// Match the suffix so installations under a SiteURL subpath are covered.
	p := parts[len(parts)-6:]
	if p[0] != "api" || p[1] != "v4" || p[2] != "channels" || p[4] != "members" ||
		!model.IsValidId(p[3]) || (p[5] != "me" && !model.IsValidId(p[5])) {
		return "", "", false
	}
	return p[3], p[5], true
}

func check(a application, rctx request.CTX, userID, channelID string) *model.AppError {
	user, err := a.GetUser(rctx, userID)
	if err != nil {
		return err
	}
	if user.IsSystemAdmin() {
		return nil
	}
	channel, err := a.GetChannel(rctx, channelID)
	if err != nil {
		return err
	}
	if channel.Type != model.ChannelTypeOpen && channel.Type != model.ChannelTypePrivate {
		// Closing a DM/GM sidebar entry is not leaving a team channel.
		return nil
	}
	member, err := a.GetTeamMember(rctx, channel.TeamId, userID)
	if err != nil {
		if err.StatusCode == http.StatusNotFound {
			return denied()
		}
		return err
	}
	if member.DeleteAt == 0 && (member.SchemeAdmin || model.IsInRole(member.Roles, model.TeamAdminRoleId)) {
		return nil
	}
	return denied()
}

func denied() *model.AppError {
	return model.NewAppError("WorkspaceEnterprise.ChannelLeavePolicy", "api.context.permissions.app_error", nil,
		"Only system or team administrators may leave channels", http.StatusForbidden)
}
