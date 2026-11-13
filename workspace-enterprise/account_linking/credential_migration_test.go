package account_linking

import (
	"context"
	"net/http"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/app/password/hashers"
	"github.com/mattermost/mattermost/server/v8/channels/web"
	"github.com/stretchr/testify/require"
)

func TestLinkedCredentialMigrationDoesNotOverwriteNewerState(t *testing.T) {
	type credentialState struct {
		Password string
		UpdateAt int64
	}
	for _, change := range []string{"reset", "removed", "unlinked", "unchanged", "concurrent_migration", "detached"} {
		t.Run(change, func(t *testing.T) {
			th := setupMergeTest(t).InitBasic(t)
			service := &Service{app: th.App}
			service.initCredentials()
			service.initMergeMetadata()
			user, appErr := th.App.GetUser(th.Context, th.BasicUser.Id)
			require.Nil(t, appErr)
			require.Nil(t, service.updateOpenIDAuthWithCredential(context.Background(), user, "test-subject-"+model.NewId()))
			th.App.InvalidateCacheForUser(user.Id)
			user, appErr = th.App.GetUser(th.Context, user.Id)
			require.Nil(t, appErr)
			require.Equal(t, model.ServiceOpenid, user.AuthService)
			legacy, err := bcrypt.GenerateFromPassword([]byte("Original123!"), bcrypt.MinCost)
			require.NoError(t, err)
			originalHash := string(legacy)
			require.NoError(t, upsertCredential(context.Background(), th.Store.GetInternalMasterDB(), user.Id, originalHash, model.GetMillis()))
			storedSnapshot, err := service.getCredential(context.Background(), user.Id)
			require.NoError(t, err)
			require.Equal(t, originalHash, storedSnapshot)
			// Emulate another request committing after login fetched its hash,
			// before verification upgrades that old hash to the latest algorithm.
			switch change {
			case "reset", "concurrent_migration":
				password := "Replacement123!"
				if change == "concurrent_migration" {
					password = "Original123!"
				}
				replacement, err := hashers.Hash(password)
				require.NoError(t, err)
				require.Nil(t, service.updateCredentialPassword(context.Background(), user.Id, replacement))
			case "removed":
				_, err := th.Store.GetInternalMasterDB().Exec(`DELETE FROM workspace_linked_credentials WHERE userid=$1`, user.Id)
				require.NoError(t, err)
			case "unlinked":
				restored, appErr := service.restoreLocalAuthWithCredential(context.Background(), user.Id)
				require.Nil(t, appErr)
				require.True(t, restored)
			case "detached":
				_, err := th.Store.GetInternalMasterDB().Exec(`UPDATE Users SET AuthService='',AuthData=NULL WHERE Id=$1`, user.Id)
				require.NoError(t, err)
			}
			db := th.Store.GetInternalMasterDB()
			// Read the exact persisted value/timestamp independently of the
			// snapshot passed into password verification.
			read := func() []credentialState {
				t.Helper()
				rows, err := db.Query(`SELECT password,updateat FROM workspace_linked_credentials WHERE userid=$1`, user.Id)
				require.NoError(t, err)
				defer rows.Close()
				var values []credentialState
				for rows.Next() {
					var value credentialState
					require.NoError(t, rows.Scan(&value.Password, &value.UpdateAt))
					values = append(values, value)
				}
				require.NoError(t, rows.Err())
				return values
			}
			before := read()
			c := &web.Context{App: th.App, AppContext: th.Context, Logger: th.App.Log()}
			appErr = service.verifyCredential(c, user, storedSnapshot, "Original123!", false)
			after := read()
			if change == "concurrent_migration" {
				require.Nil(t, appErr, "another successful upgrade of this same password must keep valid concurrent login working")
				require.Equal(t, before, after, "the already upgraded credential must not be replaced again")
			} else if change != "unchanged" {
				require.NotNil(t, appErr, "a credential changed after it was fetched must not be migrated or authenticated from its stale snapshot")
				require.Contains(t, []int{http.StatusConflict, http.StatusUnauthorized}, appErr.StatusCode)
				require.Equal(t, before, after, "migration must not overwrite a reset or recreate an unlinked/removed credential")
				var attempts int
				require.NoError(t, db.QueryRow(`SELECT failedattempts FROM users WHERE id=$1`, user.Id).Scan(&attempts))
				require.Zero(t, attempts, "a conflict with a newer credential must refund the reserved login attempt")
			} else {
				require.Nil(t, appErr)
				require.Len(t, after, 1)
				require.NotEqual(t, originalHash, after[0].Password)
				hasher, phc, err := hashers.GetHasherFromPHCString(after[0].Password)
				require.NoError(t, err)
				require.True(t, hashers.IsLatestHasher(hasher))
				require.NoError(t, hasher.CompareHashAndPassword(phc, "Original123!"))
			}
		})
	}
}
