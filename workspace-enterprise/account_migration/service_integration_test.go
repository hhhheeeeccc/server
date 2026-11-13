package account_migration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/mattermost/mattermost/server/v8/einterfaces/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// Match main.go: install the primary session adapter from the server
	// constructor before Platform.Start launches WebSocket hub readers.
	app.RegisterAutoTranslationInterface(func(server *app.Server) einterfaces.AutoTranslationInterface {
		RegisterPrimarySessionLookupsBeforeStart(server)
		return nil
	})
	app.RegisterAccountMigrationInterface(New)
	app.RegisterComplianceInterface(func(a *app.App) einterfaces.ComplianceInterface { RegisterAPI(a); return nil })
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func TestExplicitSAMLIdentitiesAPI(t *testing.T) {
	th, _ := setupMigration(t)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.IdAttribute = new("immutable-oid") })
	before := localUsers(t, th)
	identities := make([]SAMLIdentity, 0, len(before))
	for _, user := range before {
		identities = append(identities, SAMLIdentity{UserID: user.Id, Username: user.Username, Email: user.Email, SAMLID: "oid-" + user.Id})
	}
	call := func(token string, input explicitSAMLRequest) int {
		t.Helper()
		body, err := json.Marshal(input)
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, th.Client.URL+explicitSAMLRoute, bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response, err := th.Client.HTTPClient.Do(req)
		require.NoError(t, err)
		defer response.Body.Close()
		return response.StatusCode
	}
	input := explicitSAMLRequest{From: "email", DryRun: true, Identities: identities}
	require.Equal(t, http.StatusUnauthorized, call("", input))
	require.Equal(t, http.StatusForbidden, call(th.Client.AuthToken, input))
	require.Equal(t, http.StatusOK, call(th.SystemAdminClient.AuthToken, input))
	assertUnchanged(t, th, before)
	th.Server.SetLicense(nil)
	require.Equal(t, http.StatusForbidden, call(th.SystemAdminClient.AuthToken, input))
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	input.Identities = identities[:len(identities)-1]
	require.Equal(t, http.StatusBadRequest, call(th.SystemAdminClient.AuthToken, input))
	assertUnchanged(t, th, before)
	input.Identities = identities
	input.Identities[0].SAMLID = identities[1].SAMLID
	require.Equal(t, http.StatusConflict, call(th.SystemAdminClient.AuthToken, input))
	assertUnchanged(t, th, before)
	input.Identities[0].SAMLID = "oid-" + identities[0].UserID
	input.DryRun = false
	require.Equal(t, http.StatusOK, call(th.SystemAdminClient.AuthToken, input))
	for _, identity := range identities {
		user, err := th.Store.User().Get(th.Context, identity.UserID)
		require.NoError(t, err)
		require.Equal(t, model.UserAuthServiceSaml, user.AuthService)
		require.Equal(t, identity.SAMLID, *user.AuthData)
	}
}

func setupMigration(t *testing.T) (*api4.TestHelper, *Service) {
	t.Helper()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.SamlSettings.Enable = new(true)
		cfg.SamlSettings.Verify = new(true)
		cfg.SamlSettings.Encrypt = new(false)
		cfg.SamlSettings.IdpURL = new("https://idp.example.test/sso")
		cfg.SamlSettings.IdpDescriptorURL = new("https://idp.example.test/entity")
		cfg.SamlSettings.ServiceProviderIdentifier = new("https://workspace.example.test/entity")
		cfg.SamlSettings.AssertionConsumerServiceURL = new("https://workspace.example.test/login/sso/saml")
		cfg.SamlSettings.IdpCertificateFile = new("fixture-idp.pem")
		cfg.SamlSettings.EmailAttribute = new("mail")
		cfg.SamlSettings.UsernameAttribute = new("username")
		cfg.SamlSettings.IdAttribute = new("")
		cfg.LdapSettings.Enable = new(true)
		cfg.LdapSettings.LdapServer = new("127.0.0.1")
		cfg.LdapSettings.BaseDN = new("dc=example,dc=test")
		cfg.LdapSettings.IdAttribute = new("entryUUID")
		cfg.LdapSettings.UsernameAttribute = new("uid")
		cfg.LdapSettings.EmailAttribute = new("mail")
		cfg.LdapSettings.LoginIdAttribute = new("uid")
	})
	require.True(t, *th.App.Config().SamlSettings.Enable)
	require.True(t, *th.App.Config().LdapSettings.Enable)
	return th, th.App.AccountMigration().(*Service)
}

func localUsers(t *testing.T, th *api4.TestHelper) []*model.User {
	t.Helper()
	users, err := th.Store.User().GetAllUsingAuthService("")
	require.NoError(t, err)
	return users
}

func assertUnchanged(t *testing.T, th *api4.TestHelper, users []*model.User) {
	t.Helper()
	for _, before := range users {
		after, err := th.Store.User().Get(th.Context, before.Id)
		require.NoError(t, err)
		require.Equal(t, before.AuthService, after.AuthService)
		require.Equal(t, before.AuthData, after.AuthData)
		require.Equal(t, before.Password, after.Password)
		require.Equal(t, before.Username, after.Username)
	}
}

func samlMap(users []*model.User) map[string]string {
	out := make(map[string]string, len(users))
	for _, u := range users {
		out[u.Email] = "migrated" + u.Id
	}
	return out
}

func TestSAMLNativeMigrationDryRunPreservesIdentityHistoryAndMembership(t *testing.T) {
	th, service := setupMigration(t)
	post := th.CreatePost(t)
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE Users SET MfaActive=true, MfaSecret='fixture-only' WHERE Id=$1`, th.BasicUser.Id)
	require.NoError(t, err)
	require.Nil(t, th.App.UpdateUserActive(th.Context, th.BasicUser2.Id, false))
	before := localUsers(t, th)
	matches := samlMap(before)
	require.Nil(t, service.MigrateToSaml(th.Context, "", matches, false, true))
	assertUnchanged(t, th, before)
	_, _, err = th.Client.GetMe(th.Context.Context(), "")
	require.NoError(t, err, "dry run must keep existing sessions")
	response, err := th.SystemAdminClient.MigrateAuthToSaml(th.Context.Context(), "email", matches, false)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	for _, old := range before {
		u, err := th.Store.User().Get(th.Context, old.Id)
		require.NoError(t, err)
		require.Equal(t, "saml", u.AuthService)
		require.Equal(t, old.Email, *u.AuthData)
		require.Equal(t, matches[old.Email], u.Username)
		require.Empty(t, u.Password)
		require.Equal(t, old.DeleteAt, u.DeleteAt)
		require.Equal(t, old.Roles, u.Roles)
		require.Equal(t, old.MfaActive, u.MfaActive)
		require.Equal(t, old.MfaSecret, u.MfaSecret)
	}
	_, _, err = th.Client.GetMe(th.Context.Context(), "")
	require.Error(t, err, "migration revokes the prior local session")
	_, err = th.Store.Channel().GetMember(th.Context, th.BasicChannel.Id, th.BasicUser.Id)
	require.NoError(t, err)
	stored, err := th.Store.Post().GetSingle(th.Context, post.Id, false)
	require.NoError(t, err)
	require.Equal(t, post.UserId, stored.UserId)
	require.Equal(t, post.Message, stored.Message)
}

func TestMigrationPreflightRejectsIncompleteConflictingAndUnknownSAMLIdentity(t *testing.T) {
	th, service := setupMigration(t)
	before := localUsers(t, th)
	matches := samlMap(before)
	delete(matches, before[len(before)-1].Email)
	require.NotNil(t, service.MigrateToSaml(th.Context, "", matches, false, false))
	assertUnchanged(t, th, before)
	matches = samlMap(before)
	matches[before[0].Email] = before[1].Username
	err := service.MigrateToSaml(th.Context, "", matches, false, false)
	require.NotNil(t, err)
	require.Equal(t, http.StatusConflict, err.StatusCode)
	assertUnchanged(t, th, before)
	require.NotNil(t, service.MigrateToSaml(th.Context, "", samlMap(before), true, false))
	require.NotNil(t, service.MigrateToSaml(th.Context, "openid", nil, true, false))
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.IdAttribute = new("immutable-oid") })
	err = service.MigrateToSaml(th.Context, "", samlMap(before), false, false)
	require.NotNil(t, err)
	require.Equal(t, http.StatusBadRequest, err.StatusCode)
	assertUnchanged(t, th, before)
}

func TestSAMLExplicitMigrationMatchesEmailWithoutCaseSensitivity(t *testing.T) {
	th, service := setupMigration(t)
	upperEmail := strings.ToUpper(th.BasicUser2.Email)
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE Users SET Email=$1 WHERE Id=$2`, upperEmail, th.BasicUser2.Id)
	require.NoError(t, err)
	before := localUsers(t, th)
	matches := samlMap(before)
	require.Nil(t, service.MigrateToSaml(th.Context, "", matches, false, true))
	require.Nil(t, service.MigrateToSaml(th.Context, "", matches, false, false))
	user, err := th.Store.User().Get(th.Context, th.BasicUser2.Id)
	require.NoError(t, err)
	require.Equal(t, matches[upperEmail], user.Username)
	require.Equal(t, model.UserAuthServiceSaml, user.AuthService)
}

func TestSAMLPreflightRejectsLegacyEmailCaseCollision(t *testing.T) {
	th, service := setupMigration(t)
	other := th.CreateUserWithAuth(t, "google")
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE Users SET Email=$1 WHERE Id=$2`, strings.ToUpper(th.BasicUser2.Email), other.Id)
	require.NoError(t, err)
	before := localUsers(t, th)
	appErr := service.MigrateToSaml(th.Context, "", samlMap(before), false, false)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusConflict, appErr.StatusCode)
	assertUnchanged(t, th, before)
}

func TestMigrationPreflightChecksConflictBeyondFirstBatch(t *testing.T) {
	th, _ := setupMigration(t)
	plan := make([]change, 501)
	for i := range plan {
		u := th.BasicUser.DeepCopy()
		u.Id = model.NewId()
		u.Username = fmt.Sprintf("migrationbatch%d", i)
		u.Email = fmt.Sprintf("migrationbatch%d@example.test", i)
		u.AuthService = model.UserAuthServiceSaml
		u.AuthData = new(fmt.Sprintf("migration-batch-%d", i))
		plan[i] = change{after: u}
	}
	tx, err := th.Store.GetInternalMasterDB().BeginTx(th.Context.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.Nil(t, validatePlan(th.Context, tx, plan))
	plan[len(plan)-1].after.Email = th.BasicUser2.Email
	appErr := validatePlan(th.Context, tx, plan)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusConflict, appErr.StatusCode)
}

func TestLDAPMigrationDuplicatesFailuresAndForce(t *testing.T) {
	th, service := setupMigration(t)
	before := localUsers(t, th)
	directory := make([]*model.User, 0, len(before))
	for _, old := range before {
		u := old.DeepCopy()
		u.AuthService, u.AuthData = "ldap", new("ldap-"+old.Id)
		directory = append(directory, u)
	}
	setDirectory := func(users []*model.User, err *model.AppError) {
		fixture := &mocks.LdapInterface{}
		fixture.On("GetAllLdapUsers", mock.Anything).Return(users, err)
		th.App.Channels().Ldap = fixture
	}
	setDirectory(nil, model.NewAppError("fixture", "directory.unavailable", nil, "", http.StatusBadGateway))
	require.NotNil(t, service.MigrateToLdap(th.Context, "", "email", false, false))
	assertUnchanged(t, th, before)
	setDirectory(directory[:len(directory)-1], nil)
	require.NotNil(t, service.MigrateToLdap(th.Context, "", "email", false, false))
	assertUnchanged(t, th, before)
	duplicate := directory[len(directory)-1].DeepCopy()
	duplicate.AuthData = new("different-ldap-id")
	setDirectory(append(directory, duplicate), nil)
	err := service.MigrateToLdap(th.Context, "", "email", false, false)
	require.NotNil(t, err)
	require.Equal(t, http.StatusConflict, err.StatusCode)
	assertUnchanged(t, th, before)
	require.Nil(t, service.MigrateToLdap(th.Context, "email", "email", true, true))
	assertUnchanged(t, th, before)
	require.Nil(t, service.MigrateToLdap(th.Context, "email", "email", true, false))
	for i, old := range before {
		u, err := th.Store.User().Get(th.Context, old.Id)
		require.NoError(t, err)
		if i == len(before)-1 {
			require.Equal(t, old.AuthService, u.AuthService, "ambiguous directory user must be skipped")
			require.Equal(t, old.Password, u.Password)
		} else {
			require.Equal(t, "ldap", u.AuthService)
			require.Equal(t, "ldap-"+old.Id, *u.AuthData)
		}
	}
}

func TestMigrationNativePermissionsAndEntitlement(t *testing.T) {
	th, service := setupMigration(t)
	before := localUsers(t, th)
	response, err := th.Client.MigrateAuthToSaml(th.Context.Context(), "email", samlMap(before), false)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	response, err = th.Client.MigrateAuthToLdap(th.Context.Context(), "email", "email", false)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	th.Server.SetLicense(nil)
	errSAML := service.MigrateToSaml(th.Context, "", nil, true, false)
	require.NotNil(t, errSAML)
	require.Equal(t, http.StatusForbidden, errSAML.StatusCode)
	errLDAP := service.MigrateToLdap(th.Context, "", "email", false, false)
	require.NotNil(t, errLDAP)
	require.Equal(t, http.StatusForbidden, errLDAP.StatusCode)
	assertUnchanged(t, th, before)
}

func TestMigrationRollsBackWholeBatchWhenSourceChanges(t *testing.T) {
	th, service := setupMigration(t)
	before := localUsers(t, th)
	cfg, appErr := service.settings("", "saml")
	require.Nil(t, appErr)
	appErr = service.migrate(th.Context, "", "saml", cfg, false, func(users []*model.User) ([]change, *model.AppError) {
		plan := make([]change, 0, len(users))
		for _, u := range users {
			next := u.DeepCopy()
			next.AuthService, next.AuthData = "saml", new(u.Email)
			plan = append(plan, change{u, next})
		}
		// An unrelated writer changes a late row after the plan was read.
		_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE Users SET UpdateAt=UpdateAt+1 WHERE Id=$1`, users[len(users)-1].Id)
		require.NoError(t, err)
		return plan, nil
	})
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusConflict, appErr.StatusCode)
	assertUnchanged(t, th, before)
	_, _, err := th.Client.GetMe(th.Context.Context(), "")
	require.NoError(t, err, "rolled-back migration must not revoke sessions")
}

func TestMigrationSerializesIndependentServicesAndSupportsUsernameSAMLID(t *testing.T) {
	th, service := setupMigration(t)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.SamlSettings.IdAttribute = new("username") })
	before := localUsers(t, th)
	var wg sync.WaitGroup
	results := make(chan *model.AppError, 2)
	for range 2 {
		wg.Go(func() {
			other := New(service.app)
			results <- other.MigrateToSaml(request.EmptyContext(th.App.Log()), "", nil, true, false)
		})
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.Nil(t, err)
	}
	for _, old := range before {
		u, err := th.Store.User().Get(th.Context, old.Id)
		require.NoError(t, err)
		require.Equal(t, old.Username, *u.AuthData)
		require.Equal(t, "saml", u.AuthService)
	}
}

func TestMigrationRejectsDirectoryConfigChangeBeforeWrites(t *testing.T) {
	th, service := setupMigration(t)
	before := localUsers(t, th)
	directory := make([]*model.User, 0, len(before))
	for _, old := range before {
		u := old.DeepCopy()
		u.AuthData = new("ldap-" + old.Id)
		directory = append(directory, u)
	}
	fixture := &mocks.LdapInterface{}
	fixture.On("GetAllLdapUsers", mock.Anything).Run(func(mock.Arguments) {
		th.App.UpdateConfig(func(cfg *model.Config) { cfg.LdapSettings.IdAttribute = new("objectGUID") })
	}).Return(directory, (*model.AppError)(nil))
	th.App.Channels().Ldap = fixture
	err := service.MigrateToLdap(th.Context, "", "username", false, false)
	require.NotNil(t, err)
	require.Equal(t, http.StatusConflict, err.StatusCode)
	assertUnchanged(t, th, before)
}

func TestMigrationReportsSessionCleanupFailureAfterCommit(t *testing.T) {
	th, service := setupMigration(t)
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec(fmt.Sprintf(`CREATE FUNCTION workspace_migration_test_delete() RETURNS trigger AS $$
		BEGIN IF OLD.UserId = '%s' THEN RAISE EXCEPTION 'fixture session deletion failure'; END IF;
		RETURN OLD; END; $$ LANGUAGE plpgsql`, th.BasicUser.Id))
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER workspace_migration_test_delete BEFORE DELETE ON Sessions
		FOR EACH ROW EXECUTE FUNCTION workspace_migration_test_delete()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS workspace_migration_test_delete ON Sessions`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS workspace_migration_test_delete()`)
	})
	appErr := service.MigrateToSaml(th.Context, "", nil, true, false)
	require.NotNil(t, appErr, "cleanup failure must not be reported as full success")
	u, err := th.Store.User().Get(th.Context, th.BasicUser.Id)
	require.NoError(t, err)
	require.Equal(t, "saml", u.AuthService, "committed identity change is not falsely claimed to have rolled back")
	sessions, err := th.Store.Session().GetSessions(th.Context, th.BasicUser.Id)
	require.NoError(t, err)
	require.NotEmpty(t, sessions)
	otherSessions, err := th.Store.Session().GetSessions(th.Context, th.BasicUser2.Id)
	require.NoError(t, err)
	require.Empty(t, otherSessions, "continue revoking other users after one cleanup failure")
	var cutoff int64
	require.NoError(t, db.QueryRow(`SELECT CutoffAt FROM WorkspaceMigrationSessionCleanup WHERE UserId=$1`, th.BasicUser.Id).Scan(&cutoff))
	_, err = db.Exec(`UPDATE WorkspaceMigrationSessionCleanup SET RetryAt=0 WHERE UserId=$1`, th.BasicUser.Id)
	require.NoError(t, err)
	require.NoError(t, service.retryPendingSessionCleanup(th.Context.Context()))
	sessions, err = th.Store.Session().GetSessions(th.Context, th.BasicUser.Id)
	require.NoError(t, err)
	require.NotEmpty(t, sessions, "failed retry must retain the original sessions")
	_, err = db.Exec(`DROP TRIGGER workspace_migration_test_delete ON Sessions`)
	require.NoError(t, err)
	newSession, err := th.Store.Session().Save(th.Context, &model.Session{UserId: th.BasicUser.Id, ExpiresAt: model.GetMillis() + 3600000})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE Sessions SET CreateAt=$1 WHERE Id=$2`, cutoff+1, newSession.Id)
	require.NoError(t, err)
	candidates, err := service.sessionsToRevoke(th.Context, th.BasicUser.Id, cutoff)
	require.NoError(t, err)
	require.Len(t, candidates, len(sessions))
	for _, candidate := range candidates {
		require.NotEqual(t, newSession.Id, candidate.Id)
		require.LessOrEqual(t, candidate.CreateAt, cutoff)
		require.NotEmpty(t, candidate.Token)
	}
	require.NoError(t, service.retryPendingSessionCleanup(th.Context.Context()))
	sessions, err = th.Store.Session().GetSessions(th.Context, th.BasicUser.Id)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, newSession.Id, sessions[0].Id, "a session established after migration must survive retry")
	var pending int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM WorkspaceMigrationSessionCleanup WHERE UserId=$1`, th.BasicUser.Id).Scan(&pending))
	require.Zero(t, pending)
}

func TestSessionCleanupCoversSessionCreatedBeforeIdentityCommit(t *testing.T) {
	th, service := setupMigration(t)
	db := th.Store.GetInternalMasterDB()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO WorkspaceMigrationSessionCleanup (UserId,CutoffAt,RetryAt) VALUES ($1,0,0)`, th.BasicUser.Id)
	require.NoError(t, err)

	// This session is created after cleanup intent is written, but before the
	// identity transaction commits. A pre-commit cutoff would miss it on retry.
	gapSession, err := th.Store.Session().Save(th.Context, &model.Session{UserId: th.BasicUser.Id, ExpiresAt: model.GetMillis() + 3600000})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	require.NoError(t, service.retryPendingSessionCleanup(th.Context.Context()))
	sessions, err := th.Store.Session().GetSessions(th.Context, th.BasicUser.Id)
	require.NoError(t, err)
	for _, session := range sessions {
		require.NotEqual(t, gapSession.Id, session.Id, "a session created before commit must be revoked")
	}
	var pending int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM WorkspaceMigrationSessionCleanup WHERE UserId=$1`, th.BasicUser.Id).Scan(&pending))
	require.Zero(t, pending)
}

func TestSessionCleanupContinuesAfterOneRevocationFails(t *testing.T) {
	th, service := setupMigration(t)
	db := th.Store.GetInternalMasterDB()
	oldSessions, err := th.Store.Session().GetSessions(th.Context, th.BasicUser.Id)
	require.NoError(t, err)
	require.NotEmpty(t, oldSessions)
	blockedID := oldSessions[0].Id
	other, err := th.Store.Session().Save(th.Context, &model.Session{UserId: th.BasicUser.Id, ExpiresAt: model.GetMillis() + 3600000})
	require.NoError(t, err)
	cutoff := model.GetMillis()
	_, err = db.Exec(`INSERT INTO WorkspaceMigrationSessionCleanup (UserId,CutoffAt,RetryAt) VALUES ($1,$2,0)`, th.BasicUser.Id, cutoff)
	require.NoError(t, err)
	_, err = db.Exec(fmt.Sprintf(`CREATE FUNCTION workspace_migration_test_one_delete() RETURNS trigger AS $$
		BEGIN IF OLD.Id = '%s' THEN RAISE EXCEPTION 'fixture blocked session'; END IF;
		RETURN OLD; END; $$ LANGUAGE plpgsql`, blockedID))
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER workspace_migration_test_one_delete BEFORE DELETE ON Sessions
		FOR EACH ROW EXECUTE FUNCTION workspace_migration_test_one_delete()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS workspace_migration_test_one_delete ON Sessions`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS workspace_migration_test_one_delete()`)
	})

	require.NoError(t, service.retryPendingSessionCleanup(th.Context.Context()))
	sessions, err := th.Store.Session().GetSessions(th.Context, th.BasicUser.Id)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, blockedID, sessions[0].Id)
	require.NotEqual(t, other.Id, sessions[0].Id, "another session must be revoked despite the first failure")
	var pending int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM WorkspaceMigrationSessionCleanup WHERE UserId=$1`, th.BasicUser.Id).Scan(&pending))
	require.Equal(t, 1, pending)

	_, err = db.Exec(`DROP TRIGGER workspace_migration_test_one_delete ON Sessions`)
	require.NoError(t, err)
	require.NoError(t, service.retryPendingSessionCleanup(th.Context.Context()))
	sessions, err = th.Store.Session().GetSessions(th.Context, th.BasicUser.Id)
	require.NoError(t, err)
	require.Empty(t, sessions)
}

func TestMigrationSkipsBotsRemoteAndUnselectedProviders(t *testing.T) {
	th, service := setupMigration(t)
	bot, appErr := th.App.CreateBot(th.Context, &model.Bot{Username: "migrationbot", OwnerId: th.BasicUser.Id})
	require.Nil(t, appErr)
	remote := th.CreateUser(t)
	other := th.CreateUserWithAuth(t, "google")
	_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE Users SET RemoteId=$1 WHERE Id=$2`, model.NewId(), remote.Id)
	require.NoError(t, err)
	before := make([]*model.User, 0, 3)
	for _, id := range []string{bot.UserId, remote.Id, other.Id} {
		u, err := th.Store.User().Get(th.Context, id)
		require.NoError(t, err)
		before = append(before, u)
	}
	require.Nil(t, service.MigrateToSaml(th.Context, "", nil, true, false))
	assertUnchanged(t, th, before)
	u, err := th.Store.User().Get(th.Context, th.BasicUser.Id)
	require.NoError(t, err)
	require.Equal(t, "saml", u.AuthService)
}
