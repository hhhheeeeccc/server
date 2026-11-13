package cluster

import (
	"encoding/json"
	"net"
	"strconv"

	"github.com/hashicorp/memberlist"
	"github.com/mattermost/mattermost/server/public/model"
)

type cacheMember struct {
	addresses []string
	metadata  string
}

// Called only inside memberlist's serialized event callbacks: Members returns
// references that can change after its internal lock is released.
// Discovery stores the configured hostname, while memberlist exposes its
// resolved IP. Match both without DNS or SQL work inside membership callbacks.
// A returning replacement can have a different process ID at the same endpoint.
func cachePeerAddresses(node *memberlist.Node) []string {
	address := net.JoinHostPort(node.Addr.String(), strconv.Itoa(int(node.Port)))
	if len(node.Addr) == 0 {
		address = node.Name
	}
	addresses := []string{address}
	var info model.ClusterInfo
	if json.Unmarshal(node.Meta, &info) == nil && info.Hostname != "" {
		configured := net.JoinHostPort(info.Hostname, strconv.Itoa(int(node.Port)))
		if configured != address {
			addresses = append(addresses, configured)
		}
	}
	return addresses
}

func (s *Service) rememberCacheMemberLocked(node *memberlist.Node, addresses []string) {
	if s.cacheMembers == nil {
		s.cacheMembers = make(map[string]cacheMember)
	}
	s.cacheMembers[node.Name] = cacheMember{addresses: addresses, metadata: string(node.Meta)}
	for _, address := range addresses {
		delete(s.cacheMissingPeers, address)
	}
}

// A fresh process has no remote NotifyLeave to trigger cache recovery when it
// starts inside a partition. SQL may still contain a live writer missing from
// gossip. Record every such endpoint before dialing: a slow/unreachable first
// peer must not hide the other writers for the duration of sequential joins.
func (s *Service) observeDiscoveredCachePeers(addresses []string) map[string]bool {
	s.mu.Lock()
	known := make(map[string]bool)
	for _, member := range s.cacheMembers {
		for _, address := range member.addresses {
			known[address] = true
		}
	}
	// A previous discovery snapshot can race a successful join. A fresh live
	// membership snapshot clears its IP and configured hostname aliases too.
	for address := range known {
		delete(s.cacheMissingPeers, address)
	}
	changed := false
	if len(addresses) > maxCacheMissingPeers && !s.cacheMissingPeerOverflow {
		// The bounded SQL page may conceal another writer. Never treat a
		// truncated discovery view as proof that all peers are reachable.
		s.cacheMissingPeerOverflow = true
		changed = true
	}
	for _, address := range addresses {
		if !known[address] && s.recordMissingCachePeerLocked(address) {
			changed = true
		}
	}
	s.mu.Unlock()
	if changed {
		s.queueCacheRecovery()
	}
	return known
}
