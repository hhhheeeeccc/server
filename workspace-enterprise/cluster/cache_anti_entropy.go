package cluster

import (
	"encoding/json"
	"maps"
	"sync"

	"github.com/mattermost/mattermost/server/public/model"
)

const maxCacheStateBytes = 8 << 10
const maxCacheStatePeers = 256

// Only event names and counters cross the anti-entropy channel. No user IDs,
// session tokens, cached values or invalidation payloads are retained here.
type cacheState struct {
	Protocol int                           `json:"protocol"`
	Node     string                        `json:"node"`
	Versions map[model.ClusterEvent]uint64 `json:"versions"`
}

type cacheVersions struct {
	mu       sync.Mutex
	local    map[model.ClusterEvent]uint64
	received map[string]map[model.ClusterEvent]uint64
}

// These native local-cache handlers accept an empty Data field as a purge of
// that cache. Keep the allowlist explicit: application/plugin events may be
// mutations, and must never be replayed or converted into a purge.
func cachePurgeEvent(event model.ClusterEvent) (model.ClusterEvent, bool) {
	switch event {
	case model.ClusterEventInvalidateCacheForReactions,
		model.ClusterEventInvalidateCacheForRoles,
		model.ClusterEventInvalidateCacheForRolePermissions,
		model.ClusterEventInvalidateCacheForSchemes,
		model.ClusterEventInvalidateCacheForFileInfos,
		model.ClusterEventInvalidateCacheForLastPostTime,
		model.ClusterEventInvalidateCacheForPostsUsage,
		model.ClusterEventInvalidateCacheForWebhooks,
		model.ClusterEventInvalidateCacheForEmojisById,
		model.ClusterEventInvalidateCacheForEmojisIdByName,
		model.ClusterEventInvalidateCacheForChannelPinnedpostsCounts,
		model.ClusterEventInvalidateCacheForChannelMemberCounts,
		model.ClusterEventInvalidateCacheForChannelGuestCount,
		model.ClusterEventInvalidateCacheForChannel,
		model.ClusterEventInvalidateCacheForUser,
		model.ClusterEventInvalidateCacheForChannelMembersNotifyProps,
		model.ClusterEventInvalidateCacheForChannelByName,
		model.ClusterEventInvalidateCacheForLastPosts,
		model.ClusterEventInvalidateCacheForTermsOfService,
		model.ClusterEventInvalidateCacheForProfileByIds,
		model.ClusterEventInvalidateCacheForProfileInChannel,
		model.ClusterEventInvalidateCacheForAllProfiles,
		model.ClusterEventInvalidateCacheForTeams,
		model.ClusterEventInvalidateCacheForUserAutoTranslation,
		model.ClusterEventInvalidateCacheForPostTranslationEtag,
		model.ClusterEventInvalidateCacheForContentFlagging,
		model.ClusterEventInvalidateCacheForDeliveryTracking,
		model.ClusterEventInvalidateCacheForReadReceipts,
		model.ClusterEventInvalidateCacheForTemporaryPosts,
		model.ClusterEventInvalidateCacheForSessionAttributes,
		model.ClusterEventInvalidateCacheForPropertyFields,
		model.ClusterEventInvalidateCacheForAccessControlPolicyEtag,
		model.ClusterEventInvalidateCacheForUserPropertyValuesEpoch,
		model.ClusterEventInvalidateCacheForUserAttributes,
		model.ClusterEventInvalidateAllCaches,
		model.ClusterEventClearSessionCacheForAllUsers:
		return event, true
	case model.ClusterEventClearSessionCacheForUser, model.ClusterEventInvalidateWebConnCacheForUser:
		// Empty Data on a per-user handler means an empty user ID, not all
		// users. The native full local purge refreshes WebConn sessions softly
		// so valid sockets can reauthenticate with their existing tokens.
		return model.ClusterEventInvalidateAllCaches, true
	default:
		return "", false
	}
}

func (s *Service) cacheEnvelope(message *model.ClusterMessage) envelope {
	result := envelope{Message: message}
	if _, ok := cachePurgeEvent(message.Event); !ok {
		return result
	}
	s.cacheVersions.mu.Lock()
	defer s.cacheVersions.mu.Unlock()
	if s.cacheVersions.local == nil {
		s.cacheVersions.local = make(map[model.ClusterEvent]uint64)
	}
	s.cacheVersions.local[message.Event]++
	result.CacheVersion = s.cacheVersions.local[message.Event]
	return result
}

// LocalState rides on memberlist's existing encrypted TCP push/pull (normally
// every 30s, scaled by cluster size). Healthy delivery advances received clocks
// below; a sync with no missing versions performs no cache work.
func (s *Service) LocalState(bool) []byte {
	if !s.enabled() {
		return nil
	}
	s.cacheVersions.mu.Lock()
	versions := maps.Clone(s.cacheVersions.local)
	s.cacheVersions.mu.Unlock()
	data, _ := json.Marshal(cacheState{Protocol: 1, Node: s.id, Versions: versions})
	return data
}

// Queue work for the existing serialized receiver. Memberlist callbacks never
// run native handlers, SQL or license listeners. If the queue is full, leave
// the received clocks unchanged and let the next state sync retry.
func (s *Service) MergeRemoteState(data []byte, _ bool) {
	if len(data) == 0 || len(data) > maxCacheStateBytes || !s.enabled() {
		return
	}
	var state cacheState
	if json.Unmarshal(data, &state) != nil || state.Protocol != 1 || !model.IsValidId(state.Node) || state.Node == s.id || len(state.Versions) > 64 {
		return
	}
	s.mu.RLock()
	queue, stop, running := s.incoming, s.stop, s.list != nil
	s.mu.RUnlock()
	if !running {
		return
	}
	select {
	case queue <- envelope{cacheState: &state}:
	case <-stop:
	default:
	}
}

func (s *Service) receivedCacheVersion(node string, event model.ClusterEvent) uint64 {
	s.cacheVersions.mu.Lock()
	defer s.cacheVersions.mu.Unlock()
	return s.cacheVersions.received[node][event]
}

func (s *Service) markCacheVersion(node string, event model.ClusterEvent, version uint64) {
	s.cacheVersions.mu.Lock()
	defer s.cacheVersions.mu.Unlock()
	if s.cacheVersions.received == nil {
		s.cacheVersions.received = make(map[string]map[model.ClusterEvent]uint64)
	}
	if s.cacheVersions.received[node] == nil {
		// Bound memory under membership churn. Eviction can cause another
		// conservative purge on a future sync; it cannot hide an invalidation.
		if len(s.cacheVersions.received) >= maxCacheStatePeers {
			for peer := range s.cacheVersions.received {
				delete(s.cacheVersions.received, peer)
				break
			}
		}
		s.cacheVersions.received[node] = make(map[model.ClusterEvent]uint64)
	}
	if version > s.cacheVersions.received[node][event] {
		s.cacheVersions.received[node][event] = version
	}
}

func (s *Service) purgeCacheEvent(event model.ClusterEvent) bool {
	target, ok := cachePurgeEvent(event)
	if !ok {
		return false
	}
	s.mu.RLock()
	handler := s.handlers[target]
	s.mu.RUnlock()
	if handler == nil {
		return false
	}
	handler(&model.ClusterMessage{Event: target})
	return true
}

// A higher version alone is not enough: if message 2 was lost and message 3
// arrives, applying only 3 would silently hide 2. Purge the event's cache on
// that gap before recording the high watermark. Duplicates and late messages
// are harmless invalidations and can be skipped after that purge.
func (s *Service) applyCacheEnvelope(message envelope, handler func(*model.ClusterMessage)) bool {
	_, tracked := cachePurgeEvent(message.Message.Event)
	if !tracked || message.CacheVersion == 0 || !model.IsValidId(message.From) {
		handler(message.Message)
		return true
	}
	seen := s.receivedCacheVersion(message.From, message.Message.Event)
	if message.CacheVersion <= seen {
		return true
	}
	if message.CacheVersion > seen+1 && !s.purgeCacheEvent(message.Message.Event) {
		return false
	}
	handler(message.Message)
	s.markCacheVersion(message.From, message.Message.Event, message.CacheVersion)
	return true
}

func (s *Service) mergeCacheState(state cacheState) {
	for event, version := range state.Versions {
		if version > s.receivedCacheVersion(state.Node, event) && s.purgeCacheEvent(event) {
			s.markCacheVersion(state.Node, event, version)
		}
	}
}
