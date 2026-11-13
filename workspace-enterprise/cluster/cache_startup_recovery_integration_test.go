package cluster

import (
	"encoding/json"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/stretchr/testify/require"
)

func TestFailedTransportCacheRecoveryLifecycle(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.ClusterSettings.Enable = new(false)
		cfg.ClusterSettings.ClusterName = new("")
	}).InitBasic(t)
	s := New(th.Server.Platform()).(*Service)
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Id = model.NewId()
	fixture, err := json.Marshal(license)
	require.NoError(t, err)
	validatorBefore := utils.LicenseValidator
	t.Cleanup(func() { utils.LicenseValidator = validatorBefore })
	t.Setenv("MM_LICENSE", string(fixture))
	utils.LicenseValidator = clusterProcessLicenseValidator{LicenseValidatorIface: validatorBefore, fixture: fixture}
	t.Cleanup(s.Shutdown)
	th.Server.SetLicense(nil)
	s.StartInterNodeCommunication()
	require.Nil(t, s.localCacheRecovery)
	require.True(t, th.Server.SetLicense(license))
	s.StartInterNodeCommunication()
	require.Nil(t, s.localCacheRecovery, "disabled cluster must not start recovery")
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ClusterSettings.Enable = new(true) })
	s.StartInterNodeCommunication()
	first := s.localCacheRecovery
	require.NotNil(t, first)
	require.Nil(t, s.GetMyClusterInfo())
	require.False(t, s.IsLeader())
	for range 20 {
		s.StartInterNodeCommunication()
		require.Same(t, first, s.localCacheRecovery, "failed retries must not duplicate workers or reset the periodic timer")
	}
	var purges atomic.Int64
	s.RegisterClusterMessageHandler(model.ClusterEventInvalidateCacheForUser, func(*model.ClusterMessage) { purges.Add(1) })
	request := func(want int64) {
		t.Helper()
		for _, event := range []envelope{{cacheRecoveryRequested: true}, {}} {
			select {
			case first.incoming <- event:
			case <-time.After(5 * time.Second):
				t.Fatal("local receiver did not finish its previous request")
			}
		}
		require.Equal(t, want, purges.Load())
	}
	request(1)
	th.Server.SetLicense(nil)
	request(1)
	withoutCluster := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	withoutCluster.Features.Cluster = new(false)
	require.True(t, th.Server.SetLicense(withoutCluster))
	request(1)
	require.True(t, th.Server.SetLicense(license))
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ClusterSettings.Enable = new(false) })
	request(1)
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ClusterSettings.Enable = new(true)
		cfg.ClusterSettings.ClusterName = new(model.NewId())
		cfg.ClusterSettings.BindAddress = new("127.0.0.1")
		cfg.ClusterSettings.AdvertiseAddress = new("127.0.0.1")
		cfg.ClusterSettings.GossipPort = new(freePort(t))
	})
	// A successful retry must wait for the fallback receiver, including a
	// native full purge and its synchronous license listeners, to finish.
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	s.RegisterClusterMessageHandler(model.ClusterEventInvalidateCacheForUser, func(*model.ClusterMessage) {
		purges.Add(1)
		close(entered)
		<-release
	})
	first.incoming <- envelope{cacheRecoveryRequested: true}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("local purge did not enter native handler")
	}
	started := make(chan struct{})
	go func() { s.StartInterNodeCommunication(); close(started) }()
	select {
	case <-first.stop:
	case <-time.After(15 * time.Second):
		t.Fatal("transport retry did not reach the fallback handover")
	}
	select {
	case <-started:
		t.Fatal("transport receiver started while local recovery was still running")
	default:
	}
	require.Nil(t, s.GetMyClusterInfo(), "transport is published only after the fallback receiver has stopped")
	close(release)
	released = true
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("transport retry did not finish after native purge returned")
	}
	require.NotNil(t, s.GetMyClusterInfo())
	require.Nil(t, s.localCacheRecovery)
	select {
	case <-first.done:
	default:
		t.Fatal("successful start left a local recovery worker alive")
	}
	// Leave a harmless handler for the normal periodic receiver.
	s.RegisterClusterMessageHandler(model.ClusterEventInvalidateCacheForUser, func(*model.ClusterMessage) { purges.Add(1) })
	s.StopInterNodeCommunication()
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.ClusterSettings.ClusterName = new("") })
	s.StartInterNodeCommunication()
	second := s.localCacheRecovery
	require.NotNil(t, second)
	require.NotSame(t, first, second)
	require.Nil(t, s.GetMyClusterInfo())
	s.StopInterNodeCommunication()
	require.Nil(t, s.localCacheRecovery)
	select {
	case <-second.done:
	default:
		t.Fatal("stop with a nil transport left local cache workers alive")
	}
	s.StopInterNodeCommunication()
	t.Log("fallback survives failed retries, obeys license/config gates, drains before transport handover, restarts and stops without a transport")
}

func TestFailedTransportCacheRecoverySurvivesRegistrationRetries(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
		cfg.ClusterSettings.Enable = new(true)
		cfg.ClusterSettings.ClusterName = new(model.NewId())
		cfg.ClusterSettings.BindAddress = new("127.0.0.1")
		cfg.ClusterSettings.AdvertiseAddress = new("127.0.0.1")
		cfg.ClusterSettings.GossipPort = new(freePort(t))
	}).InitBasic(t)
	dsn := clusterStartupDeniedDataSource(t, th.Store.GetInternalMasterDB(), *th.Server.Config().SqlSettings.DataSource, "startup registration denied")
	s := New(th.Server.Platform()).(*Service)
	s.databaseConfig.DataSource = &dsn
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Id = model.NewId()
	fixture, err := json.Marshal(license)
	require.NoError(t, err)
	validatorBefore := utils.LicenseValidator
	t.Cleanup(func() { utils.LicenseValidator = validatorBefore })
	t.Setenv("MM_LICENSE", string(fixture))
	utils.LicenseValidator = clusterProcessLicenseValidator{LicenseValidatorIface: validatorBefore, fixture: fixture}
	t.Cleanup(s.Shutdown)
	require.True(t, th.Server.SetLicense(license))
	var purges atomic.Int64
	s.RegisterClusterMessageHandler(model.ClusterEventInvalidateCacheForUser, func(*model.ClusterMessage) { purges.Add(1) })
	var first *localCacheRecovery
	var previousIncoming chan envelope
	for attempt := range 3 {
		s.StartInterNodeCommunication()
		require.Nil(t, s.GetMyClusterInfo())
		require.False(t, s.IsLeader())
		require.Nil(t, s.database, "failed startup must close its coordination pool")
		require.NotEqual(t, previousIncoming, s.incoming, "each registration retry replaces the transport queues")
		previousIncoming = s.incoming
		if first == nil {
			first = s.localCacheRecovery
		}
		require.NotNil(t, first)
		require.Same(t, first, s.localCacheRecovery, "replacement transport queues must not strand local recovery")
		require.NotEqual(t, first.incoming, s.incoming)
		require.NotEqual(t, first.stop, s.stop)
		for _, event := range []envelope{{cacheRecoveryRequested: true}, {}} {
			select {
			case first.incoming <- event:
			case <-time.After(5 * time.Second):
				t.Fatal("registration retry stranded the local receiver")
			}
		}
		require.EqualValues(t, attempt+1, purges.Load())
	}
	address, err := url.Parse(dsn)
	require.NoError(t, err)
	_, err = th.Store.GetInternalMasterDB().Exec(`GRANT INSERT ON ClusterDiscovery TO ` + pq.QuoteIdentifier(address.User.Username()))
	require.NoError(t, err)
	s.StartInterNodeCommunication()
	require.NotNil(t, s.GetMyClusterInfo(), "actual registration privilege repair must allow transport startup")
	require.Nil(t, s.localCacheRecovery)
	select {
	case <-first.done:
	default:
		t.Fatal("successful registration left the fallback alive")
	}
	s.StopInterNodeCommunication()
	t.Log("three actual registration failures replaced transport queues while retaining a working fallback; permission repair enabled transport and stopped fallback")
}
