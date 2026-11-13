package saml

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mattermost/enterprise/internal/advisorylock"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestSAMLLoginCompletesWithOneApplicationConnection(t *testing.T) {
	for _, mode := range []string{"create", "update", "local_conversion"} {
		t.Run(mode, func(t *testing.T) {
			f := setupSAML(t)
			th := f.th
			service := th.App.Saml().(*Service)
			require.NoError(t, service.ensureSchema(th.Context))
			var props map[string]string
			values := map[string]string{"uid": "immutable-1", "mail": "pool@example.test", "username": "pool_user", "givenName": "Pool"}
			requestID := ""
			userID := ""
			if mode == "update" {
				user, _, created, appErr := service.DoLogin(th.Context, f.response(t, "", values, nil, true), nil)
				require.Nil(t, appErr)
				require.True(t, created)
				userID = user.Id
				values["givenName"] = "Updated"
			} else if mode == "local_conversion" {
				local := th.BasicUser
				userID = local.Id
				values["mail"], values["username"] = local.Email, local.Username
				token, appErr := th.App.CreateSamlRelayToken(model.TokenTypeSaml, local.Email)
				require.Nil(t, appErr)
				props = map[string]string{"action": model.OAuthActionEmailToSSO, "email_token": token.Token}
				requestID, _ = f.request(t, props)
			}
			encoded := f.response(t, requestID, values, nil, true)
			db := th.Store.GetInternalMasterDB()
			previousLimit := db.Stats().MaxOpenConnections
			db.SetMaxOpenConns(1)
			defer db.SetMaxOpenConns(previousLimit)
			ctx, cancel := context.WithTimeout(th.Context.Context(), 1500*time.Millisecond)
			defer cancel()
			user, _, created, appErr := service.DoLogin(th.Context.WithContext(ctx), encoded, props)
			require.Nil(t, appErr, "identity coordination must leave the application pool available for native account operations")
			require.NoError(t, ctx.Err())
			require.Equal(t, mode == "create", created)
			if userID != "" {
				require.Equal(t, userID, user.Id)
			}
			stored, err := th.Store.User().Get(th.Context, user.Id)
			require.NoError(t, err)
			require.Equal(t, model.UserAuthServiceSaml, stored.AuthService)
			require.Equal(t, "immutable-1", *stored.AuthData)
			if mode == "update" {
				require.Equal(t, "Updated", stored.FirstName)
			}
			if mode == "local_conversion" {
				require.Empty(t, stored.Password)
				_, _, err := th.Client.GetMe(th.Context.Context(), "")
				require.Error(t, err, "the native conversion must still revoke the old local session")
			}
		})
	}
}

func TestSAMLIdentityCoordinationBoundsAndCancellation(t *testing.T) {
	f := setupSAML(t)
	th := f.th
	service := th.App.Saml().(*Service)
	service.identityLocks = advisorylock.NewWithCapacity(*th.App.Config().SqlSettings.DataSource, 2)
	ctx, cancel := context.WithTimeout(th.Context.Context(), 5*time.Second)
	defer cancel()
	rctx := th.Context.WithContext(ctx)
	first, appErr := service.identityLock(rctx, "identity:pool-first")
	require.Nil(t, appErr)
	defer first.Release()
	second, appErr := service.identityLock(rctx, "identity:pool-second")
	require.Nil(t, appErr, "different identities must be able to proceed independently")
	defer second.Release()
	localKey := samlIdentityLockKey("local:pool-fixture")
	require.NoError(t, first.Lock(ctx, localKey), "local conversion must not need a third connection or gate slot")
	db := th.Store.GetInternalMasterDB()
	previousLimit := db.Stats().MaxOpenConnections
	db.SetMaxOpenConns(1)
	defer db.SetMaxOpenConns(previousLimit)
	var pids, locks int
	identityKey := uint64(samlIdentityLockKey("identity:pool-first"))
	localBits := uint64(localKey)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(DISTINCT pid),count(*) FROM pg_locks
		WHERE database=(SELECT oid FROM pg_database WHERE datname=current_database())
		AND locktype='advisory' AND granted AND
		((classid=$1::oid AND objid=$2::oid) OR (classid=$3::oid AND objid=$4::oid))`,
		int64(identityKey>>32), int64(uint32(identityKey)), int64(localBits>>32), int64(uint32(localBits))).Scan(&pids, &locks))
	require.Equal(t, 1, pids)
	require.Equal(t, 2, locks, "both identity keys must belong to one actual database session")
	gateContext, stopGate := context.WithTimeout(ctx, 150*time.Millisecond)
	defer stopGate()
	extra, appErr := service.identityLock(th.Context.WithContext(gateContext), "identity:over-capacity")
	if extra != nil {
		defer extra.Release()
	}
	require.NotNil(t, appErr, "the coordination budget must bound different identities as well as same-key waiters")
	other := New(th.App).(*Service)
	waitContext, stopWait := context.WithCancel(ctx)
	defer stopWait()
	done := make(chan *model.AppError, 1)
	go func() {
		lease, appErr := other.identityLock(th.Context.WithContext(waitContext), "identity:pool-first")
		if lease != nil {
			lease.Release()
		}
		done <- appErr
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		return db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
			WHERE database=(SELECT oid FROM pg_database WHERE datname=current_database())
			AND locktype='advisory' AND classid=$1::oid AND objid=$2::oid AND NOT granted)`,
			int64(identityKey>>32), int64(uint32(identityKey))).Scan(&waiting) == nil && waiting
	}, time.Second, 10*time.Millisecond, "a different service instance must wait on the same distributed identity key")
	stopWait()
	select {
	case appErr := <-done:
		require.NotNil(t, appErr)
	case <-time.After(time.Second):
		t.Fatal("identity database wait ignored cancellation")
	}
	first.Release()
	second.Release()
	lease, appErr := service.identityLock(rctx, "identity:pool-first")
	require.Nil(t, appErr, "canceled waiters and nested keys must not leak slots or locks")
	lease.Release()
}

func TestSAMLIdentityConcurrentLoginsKeepOneAccount(t *testing.T) {
	f := setupSAML(t)
	th := f.th
	service := th.App.Saml().(*Service)
	require.NoError(t, service.ensureSchema(th.Context))
	responses := make([]string, 12)
	for i := range responses {
		responses[i] = f.response(t, "", nil, nil, true)
	}
	db := th.Store.GetInternalMasterDB()
	previousLimit := db.Stats().MaxOpenConnections
	db.SetMaxOpenConns(2)
	defer db.SetMaxOpenConns(previousLimit)
	ctx, cancel := context.WithTimeout(th.Context.Context(), 5*time.Second)
	defer cancel()
	results := make(chan struct {
		id  string
		err *model.AppError
	}, len(responses))
	for _, encoded := range responses {
		go func() {
			user, _, _, appErr := service.DoLogin(th.Context.WithContext(ctx), encoded, nil)
			id := ""
			if user != nil {
				id = user.Id
			}
			results <- struct {
				id  string
				err *model.AppError
			}{id, appErr}
		}()
	}
	ids := map[string]bool{}
	for i := range responses {
		select {
		case result := <-results:
			require.Nil(t, result.err, fmt.Sprintf("concurrent signed login %d", i))
			require.True(t, model.IsValidId(result.id))
			ids[result.id] = true
		case <-ctx.Done():
			t.Fatal("concurrent logins failed to finish with native pool2")
		}
	}
	require.Len(t, ids, 1)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM Users WHERE AuthService=$1 AND AuthData=$2`, model.UserAuthServiceSaml, "immutable-1").Scan(&count))
	require.Equal(t, 1, count, "moving coordination must preserve native account identity serialization")
}
