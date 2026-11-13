package cluster

import (
	"maps"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

var _ memberlist.EventDelegate = (*Service)(nil)

const cacheRecoveryRecheckInterval = 30 * time.Second
const maxCacheMissingPeers = 256

// A peer may have written to the shared database before it joined, or while
// gossip was partitioned. It may also die after committing a write and before
// its invalidation or version summary reaches a peer. Reliable transport is
// not a durable invalidation queue. Refresh on joins and departures, without
// blocking memberlist's callbacks on cache operations or database reads.
func (s *Service) NotifyJoin(node *memberlist.Node) {
	s.queueMembershipCacheRecovery(node, false)
}

func (s *Service) NotifyLeave(node *memberlist.Node) {
	s.queueMembershipCacheRecovery(node, true)
}

func (s *Service) queueMembershipCacheRecovery(node *memberlist.Node, departed bool) {
	addresses := cachePeerAddresses(node)
	s.mu.Lock()
	if departed {
		delete(s.cacheMembers, node.Name)
		if node.Name != s.id {
			s.recordMissingCachePeerLocked(addresses[0])
		}
	} else {
		s.rememberCacheMemberLocked(node, addresses)
	}
	s.mu.Unlock()
	if node.Name != s.id {
		s.queueCacheRecovery()
	}
}

// Caller holds s.mu. Only a new missing peer (or first overflow) needs an
// immediate purge. Repeated discovery retries use the periodic worker instead
// of emptying every cache on each two-second discovery pass.
func (s *Service) recordMissingCachePeerLocked(address string) bool {
	if _, exists := s.cacheMissingPeers[address]; exists {
		return false
	}
	if len(s.cacheMissingPeers) >= maxCacheMissingPeers {
		changed := !s.cacheMissingPeerOverflow
		s.cacheMissingPeerOverflow = true
		return changed
	}
	if s.cacheMissingPeers == nil {
		s.cacheMissingPeers = make(map[string]struct{})
	}
	s.cacheMissingPeers[address] = struct{}{}
	return true
}

func (s *Service) queueCacheRecovery() {
	s.mu.RLock()
	queue, stop := s.cacheRecovery, s.stop
	s.mu.RUnlock()
	if queue == nil {
		return
	}
	select {
	case <-stop:
		return
	default:
	}
	select {
	case queue <- struct{}{}:
	default:
	}
}

func (s *Service) NotifyUpdate(node *memberlist.Node) {
	addresses := cachePeerAddresses(node)
	s.mu.Lock()
	s.rememberCacheMemberLocked(node, addresses)
	s.mu.Unlock()
}

func (s *Service) hasMissingCachePeers() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cacheMissingPeerOverflow || len(s.cacheMissingPeers) != 0
}

func (s *Service) cacheRecoveryLoop(stop <-chan struct{}, queue <-chan struct{}, incoming chan<- envelope) {
	ticker := time.NewTicker(cacheRecoveryRecheckInterval)
	defer ticker.Stop()
	s.cacheRecoveryLoopWithTicks(stop, queue, incoming, ticker.C)
}

func (s *Service) cacheRecoveryLoopWithTicks(stop <-chan struct{}, queue <-chan struct{}, incoming chan<- envelope, ticks <-chan time.Time) {
	for {
		select {
		case <-stop:
			return
		case <-queue:
			// Coalesce a burst of membership events into one local purge.
			timer := time.NewTimer(time.Second)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			select {
			case <-queue:
			default:
			}
		case <-ticks:
			// Neither gossip membership nor a successful discovery query
			// proves that every SQL writer is known: a process can start
			// during a partition with an absent or unreadable discovery
			// record. Recheck even without a known missing peer, including
			// after that writer dies without any membership notification.
			// The serialized receiver still enforces licensing and Enable.
		}
		select {
		case <-stop:
			return
		default:
		}
		// Serialize with received invalidations and anti-entropy. A full
		// queue must not discard the only signal after a peer's death.
		select {
		case incoming <- envelope{cacheRecoveryRequested: true}:
		case <-stop:
			return
		}
	}
}

// The platform's full local API does not purge every localcachelayer scope.
// Complete those scopes with their original empty-Data handlers. In particular,
// never invoke ClearSessionCacheForAllUsers here: it removes valid WebConn
// tokens. The platform API performs a soft session invalidation instead.
func membershipCachePurgeEvent(event model.ClusterEvent) bool {
	target, supported := cachePurgeEvent(event)
	return supported && target == event && event != model.ClusterEventInvalidateAllCaches && event != model.ClusterEventClearSessionCacheForAllUsers
}

func (s *Service) purgeMembershipStoreCaches() {
	s.mu.RLock()
	handlers := maps.Clone(s.handlers)
	s.mu.RUnlock()
	for event, handler := range handlers {
		if handler != nil && membershipCachePurgeEvent(event) {
			handler(&model.ClusterMessage{Event: event})
		}
	}
}

func (s *Service) recoverMembershipCaches() {
	s.purgeMembershipStoreCaches()
	// SkipSend avoids rebroadcasting inv_all_caches. Native Channel/Post
	// ClearCaches can still emit their normal scoped invalidation messages.
	if err := s.ps.InvalidateAllCachesSkipSend(); err != nil {
		s.ps.Log().Warn("Unable to refresh local cluster caches", mlog.Err(err))
	} else {
		s.ps.Log().Info("Local cluster caches refreshed")
	}
}
