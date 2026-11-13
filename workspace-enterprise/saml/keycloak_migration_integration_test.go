package saml

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/mattermost/enterprise/account_migration"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

// Both public migration contracts are followed by a real provider login. The
// runner owns the loopback Keycloak and PostgreSQL; production is never selected.
func TestKeycloakAccountMigration(t *testing.T) {
	admin := keycloakTestAdmin(t)
	for _, externalID := range []bool{false, true} {
		name := "native_email_api"
		if externalID {
			name = "explicit_external_id_api"
		}
		t.Run(name, func(t *testing.T) {
			f := setupSAML(t)
			ctx := f.th.Context.Context()
			realm := "workspace-migration-" + model.NewId()
			providerUser := keycloakSAMLUser{id: model.NewId(), username: f.th.BasicUser.Username,
				email: f.th.BasicUser.Email, password: model.NewId() + "-Provider1!"}
			configureKeycloakSAML(t, f, admin, realm, f.th.Client.URL, providerUser, true, true)
			f.th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.SamlSettings.IdAttribute = new("")
				if externalID {
					cfg.SamlSettings.IdAttribute = new("uid")
				}
			})

			oldPassword := f.th.BasicUser.Password
			secondLocalSession := f.th.CreateClient()
			_, _, err := secondLocalSession.Login(ctx, f.th.BasicUser.Email, oldPassword)
			require.NoError(t, err)
			oldSessions := []string{f.th.Client.AuthToken, secondLocalSession.AuthToken, f.th.SystemAdminClient.AuthToken}
			require.NotEqual(t, oldSessions[0], oldSessions[1])
			post, _, err := f.th.Client.CreatePost(ctx, &model.Post{ChannelId: f.th.BasicChannel.Id, Message: "رسالة قبل ترحيل المصادقة إلى SAML"})
			require.NoError(t, err)
			require.Nil(t, f.th.App.UpdateUserActive(f.th.Context, f.th.BasicUser2.Id, false))
			_, err = f.th.Store.GetInternalMasterDB().Exec(`UPDATE Users SET MfaActive=true, MfaSecret='isolated-migration-fixture' WHERE Id=$1`, f.th.BasicUser2.Id)
			require.NoError(t, err)
			users, err := f.th.Store.User().GetAllUsingAuthService("")
			require.NoError(t, err)
			before := make([]*model.User, 0, len(users))
			identities := make([]account_migration.SAMLIdentity, 0, len(users))
			matches := map[string]string{}
			for _, user := range users {
				if user.IsBot || user.IsRemote() {
					continue
				}
				before = append(before, user.DeepCopy())
				provider := providerUser
				if user.Id != f.th.BasicUser.Id {
					provider = keycloakSAMLUser{id: model.NewId(), username: user.Username,
						email: user.Email, password: model.NewId() + "-Provider1!"}
					admin.request(t, http.MethodPost, "/admin/realms/"+realm+"/users", provider.representation(), http.StatusCreated)
				}
				identities = append(identities, account_migration.SAMLIdentity{UserID: user.Id,
					Username: user.Username, Email: user.Email, SAMLID: provider.id})
				matches[user.Email] = user.Username
			}
			require.Greater(t, len(before), 1, "the migration must cover the complete source population")
			teamMember, _, err := f.th.Client.GetTeamMember(ctx, f.th.BasicTeam.Id, f.th.BasicUser.Id, "")
			require.NoError(t, err)
			channelMember, _, err := f.th.Client.GetChannelMember(ctx, f.th.BasicChannel.Id, f.th.BasicUser.Id, "")
			require.NoError(t, err)
			assertUnchanged := func() {
				t.Helper()
				for _, old := range before {
					stored, err := f.th.Store.User().Get(f.th.Context, old.Id)
					require.NoError(t, err)
					require.Equal(t, old.AuthService, stored.AuthService)
					require.Equal(t, old.AuthData, stored.AuthData)
					require.Equal(t, old.Password, stored.Password)
					require.Equal(t, old.Username, stored.Username)
					require.Equal(t, old.Email, stored.Email)
				}
				_, _, err := secondLocalSession.GetMe(ctx, "")
				require.NoError(t, err, "a failed or dry migration must preserve the original session")
			}

			path := "/api/v4/users/migrate_auth/saml"
			input := map[string]any{"from": "email", "auto": false, "matches": matches}
			licenseStatus, incompleteStatus := http.StatusNotImplemented, http.StatusInternalServerError
			if externalID {
				path = "/api/v4/workspace/migrate_auth/saml/identities"
				input = map[string]any{"from": "email", "dry_run": true, "identities": identities}
				licenseStatus, incompleteStatus = http.StatusForbidden, http.StatusBadRequest
			}
			call := func(token string) int {
				t.Helper()
				body, err := json.Marshal(input)
				require.NoError(t, err)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.th.Client.URL+path, bytes.NewReader(body))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				response, err := f.th.Client.HTTPClient.Do(req)
				require.NoError(t, err)
				defer response.Body.Close()
				_, _ = io.Copy(io.Discard, response.Body)
				return response.StatusCode
			}
			require.Equal(t, http.StatusUnauthorized, call(""))
			require.Equal(t, http.StatusForbidden, call(f.th.Client.AuthToken))
			f.th.Server.SetLicense(nil)
			require.Equal(t, licenseStatus, call(f.th.SystemAdminClient.AuthToken))
			assertUnchanged()
			require.True(t, f.th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
			if externalID {
				input["identities"] = identities[1:]
			} else {
				input["matches"] = map[string]string{providerUser.email: providerUser.username}
			}
			require.Equal(t, incompleteStatus, call(f.th.SystemAdminClient.AuthToken))
			assertUnchanged()
			if externalID {
				input["identities"] = identities
				require.Equal(t, http.StatusOK, call(f.th.SystemAdminClient.AuthToken))
				input["dry_run"] = false
			} else {
				input["matches"] = matches
				// The native HTTP route always executes. Its Go contract retains
				// the dry-run option, which is exercised before the actual API call.
				require.Nil(t, f.th.App.AccountMigration().MigrateToSaml(f.th.Context, "email", matches, false, true))
			}
			assertUnchanged()
			require.Equal(t, http.StatusOK, call(f.th.SystemAdminClient.AuthToken))
			for i, old := range before {
				stored, err := f.th.Store.User().Get(f.th.Context, old.Id)
				require.NoError(t, err)
				require.Equal(t, model.UserAuthServiceSaml, stored.AuthService)
				wantID := old.Email
				if externalID {
					wantID = identities[i].SAMLID
				}
				require.Equal(t, wantID, model.SafeDereference(stored.AuthData))
				require.Empty(t, stored.Password)
				require.Equal(t, old.Roles, stored.Roles)
				require.Equal(t, old.DeleteAt, stored.DeleteAt)
				require.Equal(t, old.MfaActive, stored.MfaActive)
				require.Equal(t, old.MfaSecret, stored.MfaSecret)
			}
			for _, token := range oldSessions {
				api := f.th.CreateClient()
				api.SetToken(token)
				_, response, err := api.GetMe(ctx, "")
				require.Error(t, err)
				require.Equal(t, http.StatusUnauthorized, response.StatusCode)
			}
			_, response, err := f.th.CreateClient().Login(ctx, providerUser.email, oldPassword)
			require.Error(t, err)
			require.Contains(t, []int{http.StatusBadRequest, http.StatusUnauthorized}, response.StatusCode, "the old local password must not authenticate after migration")

			browser, values := keycloakResponse(t, f.th.Client.URL, admin.origin, providerUser.username, providerUser.password, true)
			raw, err := base64.StdEncoding.DecodeString(values.Get("SAMLResponse"))
			require.NoError(t, err)
			require.Contains(t, string(raw), "EncryptedAssertion")
			sp, err := configure(f.th.App.Config().SamlSettings, f.th.App.GetConfigFile)
			require.NoError(t, err)
			info, err := validatedAssertion(sp, values.Get("SAMLResponse"))
			require.NoError(t, err)
			require.True(t, info.ResponseSignatureValidated)
			require.Len(t, info.Assertions, 1)
			require.True(t, info.Assertions[0].SignatureValidated)
			user := finishKeycloakLogin(t, f, browser, values)
			require.Equal(t, f.th.BasicUser.Id, user.Id, "a provider login must reuse the migrated account")
			api := f.th.CreateClient()
			parsed, err := url.Parse(f.th.Client.URL)
			require.NoError(t, err)
			for _, cookie := range browser.Jar.Cookies(parsed) {
				if cookie.Name == model.SessionCookieToken {
					api.SetToken(cookie.Value)
				}
			}
			require.NotEmpty(t, api.AuthToken)
			storedPost, _, err := api.GetPost(ctx, post.Id, "")
			require.NoError(t, err)
			require.Equal(t, post.UserId, storedPost.UserId)
			require.Equal(t, post.Message, storedPost.Message)
			afterTeam, _, err := api.GetTeamMember(ctx, f.th.BasicTeam.Id, user.Id, "")
			require.NoError(t, err)
			require.Equal(t, teamMember.Roles, afterTeam.Roles)
			afterChannel, _, err := api.GetChannelMember(ctx, f.th.BasicChannel.Id, user.Id, "")
			require.NoError(t, err)
			require.Equal(t, channelMember.Roles, afterChannel.Roles)
			_, _, err = api.CreatePost(ctx, &model.Post{ChannelId: f.th.BasicChannel.Id, Message: "رسالة بعد الدخول بحساب SAML المرحّل"})
			require.NoError(t, err, "the migrated account must remain usable for writing")
			if externalID {
				admin.request(t, http.MethodPut, "/admin/realms/"+realm+"/users/"+providerUser.id,
					map[string]any{"email": "changed-" + providerUser.email, "emailVerified": true}, http.StatusNoContent)
				changedBrowser, changedValues := keycloakResponse(t, f.th.Client.URL, admin.origin, providerUser.username, providerUser.password, false)
				changed := finishKeycloakLogin(t, f, changedBrowser, changedValues)
				require.Equal(t, user.Id, changed.Id)
				require.Equal(t, "changed-"+providerUser.email, changed.Email)
			}
			result := map[string]any{"status": "passed", "mode": name, "migratedAccounts": len(before),
				"wrongPasswordRejectedByProvider": true, "encryptedAssertion": true, "signedRequest": true,
				"originalAccountReused": true, "priorPostReadable": true, "newPostCreated": true,
				"membershipsAndRolesPreserved": true, "mfaAndActivationPreserved": true,
				"oldSessionsRejected": len(oldSessions), "oldPasswordRejected": true,
				"dryRunPreservedSource": true, "unlicensedHTTPStatus": licenseStatus, "incompleteMapHTTPStatus": incompleteStatus,
				"externalIDSurvivesProviderEmailChange": externalID}
			encoded, err := json.MarshalIndent(result, "", "  ")
			require.NoError(t, err)
			if output := os.Getenv("WORKSPACE_TEST_KEYCLOAK_MIGRATION_OUTPUT"); output != "" {
				require.NoError(t, os.WriteFile(filepath.Join(output, name+".json"), append(encoded, '\n'), 0o600))
			}
			t.Logf("KEYCLOAK_MIGRATION_RESULT %s", encoded)
		})
	}
}
