package access_control_job

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestEnterprisePolicyActivationRevision(t *testing.T) {
	for _, initiallyActive := range []bool{false, true} {
		t.Run(map[bool]string{false: "activate", true: "deactivate"}[initiallyActive], func(t *testing.T) {
			th, _ := setupChannelRules(t)
			service := th.App.Srv().Channels().AccessControl
			policy := saveTestPolicy(t, th, &model.AccessControlPolicy{
				Type: model.AccessControlPolicyTypePermission, Active: initiallyActive,
				Roles: []string{model.SystemUserRoleId},
				Rules: []model.AccessControlPolicyRule{{Actions: []string{downloadAction}, Expression: "true"}},
			})
			before := *policy
			policy.Active = !initiallyActive
			policy.Name += "-renamed"
			policy.Props = model.StringInterface{"source": "activation"}
			saved, appErr := service.SavePolicy(th.Context, policy)
			require.Nil(t, appErr)
			require.Equal(t, !initiallyActive, saved.Active)
			require.Equal(t, before.Revision+1, saved.Revision)
			require.Equal(t, policy.Name, saved.Name)
			require.Equal(t, policy.Props, saved.Props)
			stored, appErr := service.GetPolicy(th.Context, saved.ID)
			require.Nil(t, appErr)
			require.Equal(t, saved, stored)
			var historyName string
			err := th.Store.GetInternalMasterDB().QueryRow(`SELECT Name FROM AccessControlPolicyHistory WHERE ID=$1 AND Revision=$2`, saved.ID, before.Revision).Scan(&historyName)
			require.NoError(t, err)
			require.Equal(t, before.Name, historyName, "history must retain the name before the combined activation/rename")

			again, appErr := service.SavePolicy(th.Context, saved)
			require.Nil(t, appErr)
			require.Equal(t, saved.Revision, again.Revision, "identical saves must not create extra history")
			again.Name += "-cosmetic"
			renamed, appErr := service.SavePolicy(th.Context, again)
			require.Nil(t, appErr)
			require.Equal(t, saved.Revision, renamed.Revision, "name-only updates keep their native behavior")
			renamed.Active = initiallyActive
			renamed.Rules[0].Expression = "false"
			changed, appErr := service.SavePolicy(th.Context, renamed)
			require.Nil(t, appErr)
			require.Equal(t, renamed.Revision+1, changed.Revision)
			require.Equal(t, initiallyActive, changed.Active)
			require.Equal(t, "false", changed.Rules[0].Expression)
		})
	}
}

func TestEnterprisePolicyActivationConflictAndCancellation(t *testing.T) {
	th, _ := setupChannelRules(t)
	service := th.App.Srv().Channels().AccessControl
	newPolicy := func() *model.AccessControlPolicy {
		return saveTestPolicy(t, th, &model.AccessControlPolicy{
			Type: model.AccessControlPolicyTypeParent, Active: true,
			Rules: []model.AccessControlPolicyRule{{Actions: []string{model.AccessControlPolicyActionMembership}, Expression: "true"}},
		})
	}
	policy, occupied := newPolicy(), newPolicy()
	before := *policy
	original, err := th.Store.AccessControlPolicy().Get(th.Context, policy.ID)
	require.NoError(t, err)
	policy.Active = false
	policy.Name = occupied.Name
	_, appErr := service.SavePolicy(th.Context, policy)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusConflict, appErr.StatusCode)
	stored, err := th.Store.AccessControlPolicy().Get(th.Context, policy.ID)
	require.NoError(t, err)
	require.Equal(t, original, stored)
	var historyCount int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT COUNT(*) FROM AccessControlPolicyHistory WHERE ID=$1`, policy.ID).Scan(&historyCount))
	require.Zero(t, historyCount, "failed updates must not leave a history revision")

	policy.Name = before.Name
	ctx, cancel := context.WithCancel(th.Context.Context())
	cancel()
	_, appErr = service.SavePolicy(th.Context.WithContext(ctx), policy)
	require.NotNil(t, appErr)
	stored, err = th.Store.AccessControlPolicy().Get(th.Context, policy.ID)
	require.NoError(t, err)
	require.Equal(t, original, stored)

	th.Server.SetLicense(nil)
	require.Nil(t, th.Server.License())
	_, appErr = service.SavePolicy(th.Context, policy)
	require.NotNil(t, appErr)
	require.Equal(t, http.StatusForbidden, appErr.StatusCode)
	stored, err = th.Store.AccessControlPolicy().Get(th.Context, policy.ID)
	require.NoError(t, err)
	require.Equal(t, original, stored)
}

func TestEnterprisePolicyConcurrentSameActivation(t *testing.T) {
	th, _ := setupChannelRules(t)
	service := th.App.Srv().Channels().AccessControl
	policy := saveTestPolicy(t, th, &model.AccessControlPolicy{
		Type: model.AccessControlPolicyTypePermission, Active: false,
		Roles: []string{model.SystemUserRoleId},
		Rules: []model.AccessControlPolicyRule{{Actions: []string{downloadAction}, Expression: "true"}},
	})
	revision := policy.Revision
	policy.Active = true
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			saved, appErr := service.SavePolicy(th.Context, policy)
			if appErr != nil || saved == nil || !saved.Active || saved.Revision != revision+1 {
				t.Errorf("activation result: saved=%+v error=%v", saved, appErr)
			}
		})
	}
	wg.Wait()
	var historyCount int
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT COUNT(*) FROM AccessControlPolicyHistory WHERE ID=$1`, policy.ID).Scan(&historyCount))
	require.Equal(t, 1, historyCount)
}

func TestEnterprisePolicyActivationAllowsNativeWriterProgress(t *testing.T) {
	th, _ := setupChannelRules(t)
	service := th.App.Srv().Channels().AccessControl
	policy := saveTestPolicy(t, th, &model.AccessControlPolicy{
		Type: model.AccessControlPolicyTypePermission, Active: false,
		Roles: []string{model.SystemUserRoleId},
		Rules: []model.AccessControlPolicyRule{{Actions: []string{downloadAction}, Expression: "true"}},
	})
	db := th.Store.GetInternalMasterDB()
	nativeTx, err := db.Begin()
	require.NoError(t, err)
	defer nativeTx.Rollback()
	// Pause at the native store's actual write order: claim the history
	// revision first, then write/delete the current row. Another writer
	// waiting on that history key must not retain a lock on the current row.
	_, err = nativeTx.Exec(`INSERT INTO AccessControlPolicyHistory (ID, Name, Type, CreateAt, Revision, Version, Data, Props)
		SELECT ID, Name, Type, CreateAt, Revision, Version, Data, Props FROM AccessControlPolicies WHERE ID=$1`, policy.ID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(th.Context.Context(), 5*time.Second)
	defer cancel()
	done := make(chan *model.AppError, 1)
	completed := false
	defer func() {
		cancel()
		nativeTx.Rollback()
		if !completed {
			<-done
		}
	}()
	policy.Active = true
	go func() {
		_, appErr := service.SavePolicy(th.Context.WithContext(ctx), policy)
		done <- appErr
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
			AND wait_event_type='Lock' AND query LIKE '%INSERT INTO AccessControlPolicyHistory%'
		)`).Scan(&waiting)
		return err == nil && waiting
	}, 3*time.Second, 10*time.Millisecond, "activation must reach the held native history revision")
	var id string
	err = nativeTx.QueryRow(`SELECT ID FROM AccessControlPolicies WHERE ID=$1 FOR UPDATE NOWAIT`, policy.ID).Scan(&id)
	require.NoError(t, err, "a native history owner must be able to finish its current-row write")
	_, err = nativeTx.Exec(`UPDATE AccessControlPolicies SET Revision=Revision+1 WHERE ID=$1`, policy.ID)
	require.NoError(t, err)
	require.NoError(t, nativeTx.Commit())
	appErr := <-done
	completed = true
	require.Nil(t, appErr)
	stored, err := th.Store.AccessControlPolicy().Get(th.Context, policy.ID)
	require.NoError(t, err)
	require.True(t, stored.Active)
	require.Equal(t, policy.Revision+2, stored.Revision)
}
