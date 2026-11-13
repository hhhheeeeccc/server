package cluster

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/stretchr/testify/require"
)

func TestPeriodicCacheRecoveryHonorsLicenseAndConfiguration(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.ClusterSettings.Enable = new(true)
	}).InitBasic(t)
	s := New(th.Server.Platform()).(*Service)
	var purges atomic.Int64
	s.RegisterClusterMessageHandler(model.ClusterEventInvalidateCacheForUser, func(*model.ClusterMessage) {
		purges.Add(1)
	})
	validatorBefore := utils.LicenseValidator
	t.Cleanup(func() { utils.LicenseValidator = validatorBefore })
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Id = model.NewId()
	fixture, err := json.Marshal(license)
	require.NoError(t, err)
	t.Setenv("MM_LICENSE", string(fixture))
	utils.LicenseValidator = clusterProcessLicenseValidator{LicenseValidatorIface: validatorBefore, fixture: fixture}

	stop := make(chan struct{})
	ticks := make(chan time.Time)
	requests := make(chan envelope)
	incoming := make(chan envelope)
	workerDone, receiverDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(workerDone); s.cacheRecoveryLoopWithTicks(stop, nil, requests, ticks) }()
	go func() { defer close(receiverDone); s.receiveLoop(stop, incoming) }()
	t.Cleanup(func() {
		close(stop)
		for _, done := range []chan struct{}{workerDone, receiverDone} {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("periodic recovery did not stop")
			}
		}
	})
	tick := func(want int64) {
		t.Helper()
		select {
		case ticks <- time.Now():
		case <-time.After(time.Second):
			t.Fatal("recovery worker did not accept a tick")
		}
		var request envelope
		select {
		case request = <-requests:
			require.True(t, request.cacheRecoveryRequested)
		case <-time.After(time.Second):
			t.Fatal("a tick must request recovery without discovery state")
		}
		select {
		case incoming <- request:
		case <-time.After(time.Second):
			t.Fatal("receiver did not accept the periodic recovery request")
		}
		// A second send on this unbuffered channel cannot complete until the
		// receiver finishes the first request, including native license reload.
		select {
		case incoming <- envelope{}:
		case <-time.After(5 * time.Second):
			t.Fatal("receiver did not finish the periodic recovery request")
		}
		require.Equal(t, want, purges.Load())
	}
	th.Server.SetLicense(nil)
	tick(0)
	require.True(t, th.Server.SetLicense(license))
	tick(1)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ClusterSettings.Enable = new(false) })
	tick(1)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ClusterSettings.Enable = new(true) })
	withoutCluster := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	withoutCluster.Id = model.NewId()
	withoutCluster.Features.Cluster = new(false)
	require.True(t, th.Server.SetLicense(withoutCluster))
	tick(1)
	th.Server.SetLicense(nil)
	tick(1)
	require.True(t, th.Server.SetLicense(license))
	tick(2)
}
