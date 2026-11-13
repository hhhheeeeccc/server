package cluster

import (
	"encoding/json"
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/hashicorp/memberlist"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func discoveryTestNode(t *testing.T, hostname, ip string, port uint16) *memberlist.Node {
	t.Helper()
	meta, err := json.Marshal(model.ClusterInfo{Hostname: hostname})
	require.NoError(t, err)
	return &memberlist.Node{Name: model.NewId(), Addr: net.ParseIP(ip), Port: port, Meta: meta}
}

func TestDiscoveryCacheRecoveryFindsUnseenWritersWithoutRepeatedImmediatePurges(t *testing.T) {
	s := &Service{cacheRecovery: make(chan struct{}, 1)}
	known := discoveryTestNode(t, "known.internal", "127.0.0.2", 8074)
	addresses := []string{"known.internal:8074", "127.0.0.2:8074", "unseen.internal:8074"}
	s.NotifyJoin(known)
	<-s.cacheRecovery
	got := s.observeDiscoveredCachePeers(addresses)
	require.True(t, got["known.internal:8074"])
	require.True(t, got["127.0.0.2:8074"])
	require.False(t, got["unseen.internal:8074"])
	require.True(t, s.hasMissingCachePeers())
	require.Len(t, s.cacheRecovery, 1)
	<-s.cacheRecovery
	for range 100 {
		s.observeDiscoveredCachePeers(addresses)
	}
	require.Empty(t, s.cacheRecovery, "discovery retries must not purge every two seconds")
	// SQL expiry/deletion is not proof that a partitioned writer has finished.
	s.observeDiscoveredCachePeers(nil)
	require.True(t, s.hasMissingCachePeers())
	require.Empty(t, s.cacheRecovery)
	missing := discoveryTestNode(t, "unseen.internal", "127.0.0.3", 8074)
	s.NotifyJoin(missing)
	require.False(t, s.hasMissingCachePeers())
	require.Len(t, s.cacheRecovery, 1, "a real join still schedules its native purge")
	<-s.cacheRecovery
	s.observeDiscoveredCachePeers(addresses)
	require.Empty(t, s.cacheRecovery, "healthy registered peers must not cause repeated immediate recovery")
}

func TestDiscoveryCacheRecoveryMatchesHostnameAndIPAfterReplacement(t *testing.T) {
	s := &Service{cacheRecovery: make(chan struct{}, 1)}
	peer := discoveryTestNode(t, "writer.internal", "::1", 8074)
	s.observeDiscoveredCachePeers([]string{"writer.internal:8074"})
	require.True(t, s.hasMissingCachePeers())
	<-s.cacheRecovery
	s.NotifyJoin(peer)
	require.False(t, s.hasMissingCachePeers())
	<-s.cacheRecovery
	s.NotifyLeave(peer)
	require.True(t, s.hasMissingCachePeers())
	<-s.cacheRecovery
	// New process ID, identical endpoint and configured hostname. No DNS
	// lookup is needed to clear either form of the missing peer's address.
	replacement := discoveryTestNode(t, "writer.internal", "::1", 8074)
	s.NotifyJoin(replacement)
	require.False(t, s.hasMissingCachePeers())
	<-s.cacheRecovery
	got := s.observeDiscoveredCachePeers([]string{"writer.internal:8074", "[::1]:8074"})
	require.True(t, got["writer.internal:8074"])
	require.True(t, got["[::1]:8074"])
	require.Empty(t, s.cacheRecovery)
}

func TestDiscoveryCacheRecoveryBoundsMissingPeerKnowledgeForTruncatedSQLView(t *testing.T) {
	s := &Service{cacheRecovery: make(chan struct{}, 1)}
	var addresses []string
	for i := range maxCacheMissingPeers + 1 {
		port := 10000 + i
		addresses = append(addresses, "127.0.0.2:"+strconv.Itoa(port))
		s.NotifyJoin(&memberlist.Node{Name: model.NewId(), Addr: net.ParseIP("127.0.0.2"), Port: uint16(port)})
	}
	<-s.cacheRecovery
	s.observeDiscoveredCachePeers(addresses[:maxCacheMissingPeers])
	require.False(t, s.hasMissingCachePeers())
	require.Empty(t, s.cacheRecovery)
	s.observeDiscoveredCachePeers(addresses)
	require.True(t, s.hasMissingCachePeers(), "the extra SQL row signals that the bounded discovery view can hide another writer")
	require.Len(t, s.cacheRecovery, 1)
	<-s.cacheRecovery
	s.observeDiscoveredCachePeers(addresses)
	require.Empty(t, s.cacheRecovery)
	s.observeDiscoveredCachePeers(nil)
	require.True(t, s.hasMissingCachePeers(), "a later small SQL page must not silently forget a hidden writer")
}

func TestDiscoveryCacheRecoveryConcurrentWithMembershipCallbacks(t *testing.T) {
	s := &Service{cacheRecovery: make(chan struct{}, 1)}
	peer := discoveryTestNode(t, "writer.internal", "127.0.0.2", 8074)
	addresses := []string{"writer.internal:8074"}
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				switch worker {
				case 0:
					s.observeDiscoveredCachePeers(addresses)
				case 1:
					s.NotifyUpdate(peer)
				case 2:
					s.NotifyLeave(peer)
				case 3:
					s.NotifyJoin(peer)
				}
			}
		}()
	}
	workers.Wait()
	s.NotifyJoin(peer)
	s.observeDiscoveredCachePeers(addresses)
	require.False(t, s.hasMissingCachePeers())
}

func TestDiscoveryCacheRecoveryCopiesMemberDataBeforeAsyncUse(t *testing.T) {
	s := &Service{cacheRecovery: make(chan struct{}, 1), list: new(memberlist.Memberlist)}
	peer := discoveryTestNode(t, "writer.internal", "127.0.0.2", 8074)
	s.NotifyJoin(peer)
	<-s.cacheRecovery
	// Simulate the library replacing a Node's backing fields after its event
	// callback returns. Asynchronous consumers must retain an independent copy.
	peer.Addr = net.ParseIP("127.0.0.3")
	peer.Meta = []byte(`{"hostname":"updated.internal"}`)
	known := s.observeDiscoveredCachePeers([]string{"writer.internal:8074"})
	require.True(t, known["writer.internal:8074"])
	require.True(t, known["127.0.0.2:8074"])
	require.False(t, known["127.0.0.3:8074"])
	infos, err := s.GetClusterInfos()
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.Equal(t, "writer.internal", infos[0].Hostname)
	s.NotifyUpdate(peer)
	known = s.observeDiscoveredCachePeers([]string{"updated.internal:8074"})
	require.True(t, known["updated.internal:8074"])
	require.True(t, known["127.0.0.3:8074"])
	infos, err = s.GetClusterInfos()
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.Equal(t, "updated.internal", infos[0].Hostname)
	infos[0].Hostname = "caller-modified"
	infos, err = s.GetClusterInfos()
	require.NoError(t, err)
	require.Equal(t, "updated.internal", infos[0].Hostname)
	s.NotifyLeave(peer)
	infos, err = s.GetClusterInfos()
	require.NoError(t, err)
	require.Empty(t, infos)
}
