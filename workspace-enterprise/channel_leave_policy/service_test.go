package channel_leave_policy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/i18n"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/stretchr/testify/require"
)

type fakeApplication struct {
	licensed  bool
	user      *model.User
	channel   *model.Channel
	member    *model.TeamMember
	session   *model.Session
	lookupErr *model.AppError
	memberErr *model.AppError
	teamRead  string
}

func (f *fakeApplication) License() *model.License {
	if f.licensed {
		return &model.License{}
	}
	return nil
}
func (*fakeApplication) Log() *mlog.Logger                                     { return nil }
func (f *fakeApplication) GetSession(string) (*model.Session, *model.AppError) { return f.session, nil }
func (f *fakeApplication) GetUser(request.CTX, string) (*model.User, *model.AppError) {
	return f.user, f.lookupErr
}
func (f *fakeApplication) GetChannel(request.CTX, string) (*model.Channel, *model.AppError) {
	return f.channel, nil
}
func (f *fakeApplication) GetTeamMember(_ request.CTX, team, _ string) (*model.TeamMember, *model.AppError) {
	f.teamRead = team
	return f.member, f.memberErr
}

func TestRESTLeavePolicy(t *testing.T) {
	userID, channelID, teamID := model.NewId(), model.NewId(), model.NewId()
	for _, tc := range []struct {
		name    string
		change  func(*fakeApplication, *http.Request)
		blocked bool
	}{
		{name: "ordinary member blocked", blocked: true},
		{name: "me alias blocked", blocked: true, change: func(_ *fakeApplication, r *http.Request) {
			r.URL.Path = model.APIURLSuffix + "/channels/" + channelID + "/members/me"
		}},
		{name: "subpath blocked", blocked: true, change: func(_ *fakeApplication, r *http.Request) { r.URL.Path = "/workspace" + r.URL.Path }},
		{name: "channel admin still blocked", blocked: true, change: func(f *fakeApplication, _ *http.Request) { f.user.Roles = "system_user channel_admin" }},
		{name: "team admin allowed", change: func(f *fakeApplication, _ *http.Request) { f.member.SchemeAdmin = true }},
		{name: "legacy team admin allowed", change: func(f *fakeApplication, _ *http.Request) { f.member.Roles = "team_user team_admin" }},
		{name: "deleted team admin blocked", blocked: true, change: func(f *fakeApplication, _ *http.Request) { f.member.SchemeAdmin = true; f.member.DeleteAt = 1 }},
		{name: "system admin allowed", change: func(f *fakeApplication, _ *http.Request) { f.user.Roles = "system_user system_admin" }},
		{name: "unlicensed ordinary member blocked", blocked: true, change: func(f *fakeApplication, _ *http.Request) { f.licensed = false }},
		{name: "unlicensed system admin allowed", change: func(f *fakeApplication, _ *http.Request) { f.licensed = false; f.user.Roles = "system_admin" }},
		{name: "unlicensed team admin allowed", change: func(f *fakeApplication, _ *http.Request) { f.licensed = false; f.member.SchemeAdmin = true }},
		{name: "guest blocked", blocked: true, change: func(f *fakeApplication, _ *http.Request) { f.user.Roles = "system_guest" }},
		{name: "not a member of channel team", blocked: true, change: func(f *fakeApplication, _ *http.Request) {
			f.memberErr = model.NewAppError("test", "not_found", nil, "", 404)
		}},
		{name: "missing auth handled by core", change: func(_ *fakeApplication, r *http.Request) { r.Header.Del("Authorization") }},
		{name: "invalid session handled by core", change: func(f *fakeApplication, _ *http.Request) { f.session = nil }},
		{name: "remove another member handled by core", change: func(_ *fakeApplication, r *http.Request) {
			r.URL.Path = model.APIURLSuffix + "/channels/" + channelID + "/members/" + model.NewId()
		}},
		{name: "GET handled by core", change: func(_ *fakeApplication, r *http.Request) { r.Method = http.MethodGet }},
		{name: "other endpoint handled by core", change: func(_ *fakeApplication, r *http.Request) { r.URL.Path += "/notify_props" }},
		{name: "DM native behavior", change: func(f *fakeApplication, _ *http.Request) { f.channel.Type = model.ChannelTypeDirect }},
		{name: "lookup failure stops removal", blocked: true, change: func(f *fakeApplication, _ *http.Request) {
			f.lookupErr = model.NewAppError("test", "api.context.permissions.app_error", nil, "", 403)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeApplication{licensed: true, user: &model.User{Id: userID, Roles: "system_user"},
				channel: &model.Channel{Id: channelID, TeamId: teamID, Type: model.ChannelTypeOpen},
				member:  &model.TeamMember{TeamId: teamID, UserId: userID}, session: &model.Session{UserId: userID}}
			r := httptest.NewRequest(http.MethodDelete, model.APIURLSuffix+"/channels/"+channelID+"/members/"+userID, nil)
			r.Header.Set("Authorization", "Bearer test-token")
			if tc.change != nil {
				tc.change(f, r)
			}
			nextCalled := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { nextCalled = true; w.WriteHeader(http.StatusNoContent) })
			response := httptest.NewRecorder()
			(&Service{app: f}).guard(next).ServeHTTP(response, r)
			require.Equal(t, !tc.blocked, nextCalled)
			if tc.blocked {
				require.Equal(t, 403, response.Code)
				require.Contains(t, response.Body.String(), "api.context.permissions.app_error")
			} else {
				require.Equal(t, 204, response.Code)
			}
			if f.teamRead != "" {
				require.Equal(t, teamID, f.teamRead, "must check the channel's team")
			}
		})
	}
}

type fakeProvider struct{ called bool }

func (*fakeProvider) GetTrigger() string { return "leave" }
func (*fakeProvider) GetCommand(*app.App, i18n.TranslateFunc) *model.Command {
	return &model.Command{Trigger: "leave"}
}
func (p *fakeProvider) DoCommand(*app.App, request.CTX, *model.CommandArgs, string) *model.CommandResponse {
	p.called = true
	return &model.CommandResponse{GotoLocation: "original-location"}
}

func TestLeaveCommandGuard(t *testing.T) {
	for _, blocked := range []bool{true, false} {
		original := &fakeProvider{}
		provider := &leaveProvider{original: original, check: func(_ *app.App, _ request.CTX, args *model.CommandArgs, trigger, message string) *model.AppError {
			require.Equal(t, "target-channel", args.ChannelId)
			require.Equal(t, "leave", trigger)
			if blocked {
				return model.NewAppError("test", "api.context.permissions.app_error", nil, "", 403)
			}
			return nil
		}}
		response := provider.DoCommand(nil, request.EmptyContext(nil), &model.CommandArgs{ChannelId: "target-channel", T: func(id string, _ ...any) string { return id }}, "")
		require.Equal(t, !blocked, original.called)
		if blocked {
			require.Equal(t, model.CommandResponseTypeEphemeral, response.ResponseType)
			require.Empty(t, response.GotoLocation)
		} else {
			require.Equal(t, "original-location", response.GotoLocation)
		}
	}
}

func TestRegisterCommandsWrapsBuiltin(t *testing.T) {
	RegisterCommands()
	for _, trigger := range []string{"leave", "remove", "kick"} {
		provider := app.GetCommandProvider(trigger)
		require.IsType(t, &leaveProvider{}, provider)
		require.Equal(t, trigger, provider.GetTrigger())
		RegisterCommands()
		require.Same(t, provider, app.GetCommandProvider(trigger))
	}
}

func TestCommandHTTPPolicy(t *testing.T) {
	for _, licensed := range []bool{true, false} {
		for _, teamAdmin := range []bool{true, false} {
			for _, tc := range []struct {
				command string
				self    bool
			}{
				{"/leave", true}, {"/LEAVE", true}, {"/مغادرة", true},
				{"/remove @alice", true}, {"/kick ALICE", true},
				{"/إزالة @alice", true}, {"/طرد alice", true},
				{"/kick\t@alice", true}, {"/remove @alice ignored", true},
				{"/kick @bob", false}, {"/remove @bob", false}, {"/remove", false},
				{"/remove  @alice", false}, // native parser treats empty first field as missing username
				{"/me hello", false},
			} {
				t.Run(tc.command+"/licensed="+strconv.FormatBool(licensed)+"/admin="+strconv.FormatBool(teamAdmin), func(t *testing.T) {
					userID, channelID, teamID := model.NewId(), model.NewId(), model.NewId()
					f := &fakeApplication{licensed: licensed, user: &model.User{Id: userID, Username: "alice", Roles: "system_user"},
						channel: &model.Channel{Id: channelID, TeamId: teamID, Type: model.ChannelTypePrivate},
						member:  &model.TeamMember{SchemeAdmin: teamAdmin}, session: &model.Session{UserId: userID}}
					// Client-supplied user/team IDs must not influence the authorization.
					body, err := json.Marshal(&model.CommandArgs{Command: tc.command, ChannelId: channelID, UserId: model.NewId(), TeamId: model.NewId()})
					require.NoError(t, err)
					r := httptest.NewRequest(http.MethodPost, "/workspace/api/v4/commands/execute", strings.NewReader(string(body)))
					r.Header.Set("Authorization", "Bearer test-token")
					nextCalled := false
					next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						nextCalled = true
						restored, readErr := io.ReadAll(r.Body)
						require.NoError(t, readErr)
						require.Equal(t, body, restored)
						w.WriteHeader(http.StatusNoContent)
					})
					response := httptest.NewRecorder()
					(&Service{app: f}).guard(next).ServeHTTP(response, r)
					blocked := tc.self && !teamAdmin
					require.Equal(t, !blocked, nextCalled)
					if blocked {
						require.Equal(t, http.StatusForbidden, response.Code)
					}
					if f.teamRead != "" {
						require.Equal(t, teamID, f.teamRead)
					}
				})
			}
		}
	}
}

func TestCommandBodyPreserved(t *testing.T) {
	for _, body := range []string{"not-json", `{}`, strings.Repeat(" ", (1<<20)+5)} {
		r := httptest.NewRequest(http.MethodPost, "/api/v4/commands/execute", strings.NewReader(body))
		_, ok := readCommand(r)
		require.False(t, ok)
		restored, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(restored))
	}
}
