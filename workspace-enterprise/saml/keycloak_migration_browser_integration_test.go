package saml

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/mattermost/enterprise/account_migration"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

// The browser starts with an empty profile after the real HTTP migration. No
// session cookie or SAML response is injected into it by the fixture.
func TestKeycloakAccountMigrationBrowser(t *testing.T) {
	if os.Getenv("WORKSPACE_TEST_CHROME_EXECUTABLE") == "" || os.Getenv("WORKSPACE_TEST_SAML_STATIC_DIR") == "" {
		t.Skip("use --account-migration --browser --static-dir with the isolated runner")
	}
	admin := keycloakTestAdmin(t)
	for _, externalID := range []bool{false, true} {
		mode := "native_email_api"
		if externalID {
			mode = "explicit_external_id_api"
		}
		t.Run(mode, func(t *testing.T) {
			f := setupSAML(t)
			ctx := f.th.Context.Context()
			oldToken, oldPassword := f.th.Client.AuthToken, f.th.BasicUser.Password
			user := f.th.BasicUser.DeepCopy()
			user.Locale = "ar"
			_, appErr := f.th.App.UpdateUser(f.th.Context, user, false)
			require.Nil(t, appErr)
			const priorText = "رسالة الحساب الأصلي قبل ترحيل المصادقة"
			prior, _, err := f.th.Client.CreatePost(ctx, &model.Post{ChannelId: f.th.BasicChannel.Id, Message: priorText})
			require.NoError(t, err)
			teamBefore, err := f.th.Store.Team().GetMember(f.th.Context, f.th.BasicTeam.Id, user.Id)
			require.NoError(t, err)
			channelBefore, err := f.th.Store.Channel().GetMember(f.th.Context, f.th.BasicChannel.Id, user.Id)
			require.NoError(t, err)
			origin := samlBrowserOrigin(t, f)
			provider := keycloakSAMLUser{id: model.NewId(), username: user.Username,
				email: user.Email, password: model.NewId() + "-Migration1!"}
			configureKeycloakSAML(t, f, admin, "workspace-migration-browser-"+model.NewId(), origin, provider, true, true)
			f.th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.SamlSettings.IdAttribute = new("")
				if externalID {
					cfg.SamlSettings.IdAttribute = new("uid")
				}
			})

			users, err := f.th.Store.User().GetAllUsingAuthService("")
			require.NoError(t, err)
			identities := make([]account_migration.SAMLIdentity, 0, len(users))
			matches := map[string]string{}
			for _, source := range users {
				if source.IsBot || source.IsRemote() {
					continue
				}
				id := model.NewId()
				if source.Id == user.Id {
					id = provider.id
				}
				identities = append(identities, account_migration.SAMLIdentity{UserID: source.Id,
					Username: source.Username, Email: source.Email, SAMLID: id})
				matches[source.Email] = source.Username
			}
			path := "/api/v4/users/migrate_auth/saml"
			input := map[string]any{"from": "email", "auto": false, "matches": matches}
			authData := user.Email
			if externalID {
				path = "/api/v4/workspace/migrate_auth/saml/identities"
				input = map[string]any{"from": "email", "dry_run": false, "identities": identities}
				authData = provider.id
			}
			body, err := json.Marshal(input)
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.th.Client.URL+path, bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+f.th.SystemAdminClient.AuthToken)
			response, err := f.th.Client.HTTPClient.Do(req)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, response.Body.Close())
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode)
			stored, err := f.th.Store.User().Get(f.th.Context, user.Id)
			require.NoError(t, err)
			require.Equal(t, "saml", stored.AuthService)
			require.Equal(t, authData, model.SafeDereference(stored.AuthData))
			require.Empty(t, stored.Password)
			oldAPI := f.th.CreateClient()
			oldAPI.SetToken(oldToken)
			_, apiResponse, err := oldAPI.GetMe(ctx, "")
			require.Error(t, err)
			require.Equal(t, http.StatusUnauthorized, apiResponse.StatusCode)
			_, apiResponse, err = f.th.CreateClient().Login(ctx, user.Email, oldPassword)
			require.Error(t, err)
			require.Contains(t, []int{http.StatusBadRequest, http.StatusUnauthorized}, apiResponse.StatusCode)

			verifySAMLBrowserForAccount(t, f, origin, admin.origin, provider.username, provider.password, authData,
				&samlBrowserMigration{Mode: mode, UserID: user.Id, ChannelName: f.th.BasicChannel.Name,
					ChannelID: f.th.BasicChannel.Id, PriorPostID: prior.Id, PriorText: priorText})
			teamAfter, err := f.th.Store.Team().GetMember(f.th.Context, f.th.BasicTeam.Id, user.Id)
			require.NoError(t, err)
			require.Equal(t, teamBefore.Roles, teamAfter.Roles)
			channelAfter, err := f.th.Store.Channel().GetMember(f.th.Context, f.th.BasicChannel.Id, user.Id)
			require.NoError(t, err)
			require.Equal(t, channelBefore.Roles, channelAfter.Roles)
			var accounts int
			require.NoError(t, f.th.Store.GetInternalMasterDB().QueryRow(`SELECT count(*) FROM Users
				WHERE AuthService='saml' AND AuthData=$1`, authData).Scan(&accounts))
			require.Equal(t, 1, accounts, "provider login must not create a duplicate account")
		})
	}
}
