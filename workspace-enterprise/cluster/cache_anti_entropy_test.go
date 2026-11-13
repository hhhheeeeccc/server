package cluster

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/stretchr/testify/require"
)

func TestCacheAntiEntropyRecoversMissingMiddleAndIgnoresOldState(t *testing.T) {
	event := model.ClusterEventInvalidateCacheForProfileByIds
	node := model.NewId()
	values := map[string]string{"first": "old", "missing": "old", "latest": "old"}
	purges := 0
	handler := func(message *model.ClusterMessage) {
		if len(message.Data) == 0 {
			clear(values)
			purges++
		} else {
			delete(values, string(message.Data))
		}
	}
	s := &Service{handlers: map[model.ClusterEvent]einterfaces.ClusterMessageHandler{event: handler}}
	apply := func(version uint64, key string) {
		require.True(t, s.applyCacheEnvelope(envelope{From: node, CacheVersion: version, Message: &model.ClusterMessage{Event: event, Data: []byte(key)}}, handler))
	}
	apply(1, "first")
	require.Equal(t, map[string]string{"missing": "old", "latest": "old"}, values)
	require.Zero(t, purges, "healthy delivery must only invalidate its key")
	// The second message is absent; receipt of 3 must not mask that hole.
	apply(3, "latest")
	require.Empty(t, values)
	require.Equal(t, 1, purges)
	values["missing"] = "fresh from SQL"
	apply(2, "missing")
	s.mergeCacheState(cacheState{Node: node, Versions: map[model.ClusterEvent]uint64{event: 2}})
	s.mergeCacheState(cacheState{Node: node, Versions: map[model.ClusterEvent]uint64{event: 3}})
	require.Equal(t, "fresh from SQL", values["missing"])
	require.Equal(t, 1, purges, "duplicates and stale state must not purge a fresh cache")
	// Also recover the final missing message without a later application write.
	s.mergeCacheState(cacheState{Node: node, Versions: map[model.ClusterEvent]uint64{event: 4}})
	require.Empty(t, values)
	require.Equal(t, 2, purges)
}

func TestCacheAntiEntropyScopesCountersAndDoesNotReplayApplicationEvents(t *testing.T) {
	s := &Service{handlers: make(map[model.ClusterEvent]einterfaces.ClusterMessageHandler)}
	profile := model.ClusterEventInvalidateCacheForProfileByIds
	role := model.ClusterEventInvalidateCacheForRoles
	for _, event := range []model.ClusterEvent{model.ClusterEventPublish, model.ClusterEventUpdateStatus, model.ClusterEventInstallPlugin, model.ClusterEventPluginEvent, model.ClusterEventUpdateSessionAttributes, "inv_unrecognized_custom_cache"} {
		require.Zero(t, s.cacheEnvelope(&model.ClusterMessage{Event: event}).CacheVersion)
		calls := 0
		s.RegisterClusterMessageHandler(event, func(*model.ClusterMessage) { calls++ })
		s.mergeCacheState(cacheState{Node: model.NewId(), Versions: map[model.ClusterEvent]uint64{event: 10}})
		require.Zero(t, calls, "state sync must not replay or purge an unsupported event")
	}
	require.EqualValues(t, 1, s.cacheEnvelope(&model.ClusterMessage{Event: profile}).CacheVersion)
	require.EqualValues(t, 1, s.cacheEnvelope(&model.ClusterMessage{Event: role}).CacheVersion)
	require.EqualValues(t, 2, s.cacheEnvelope(&model.ClusterMessage{Event: profile}).CacheVersion)
	counts := make(map[model.ClusterEvent]int)
	for _, event := range []model.ClusterEvent{profile, role} {
		s.RegisterClusterMessageHandler(event, func(message *model.ClusterMessage) {
			require.Empty(t, message.Data)
			counts[message.Event]++
		})
	}
	node := model.NewId()
	s.markCacheVersion(node, role, 1)
	s.mergeCacheState(cacheState{Node: node, Versions: map[model.ClusterEvent]uint64{profile: 2, role: 1}})
	require.Equal(t, map[model.ClusterEvent]int{profile: 1}, counts)
	// A missing handler must not be acknowledged as repaired.
	s.mergeCacheState(cacheState{Node: node, Versions: map[model.ClusterEvent]uint64{model.ClusterEventInvalidateCacheForTeams: 5}})
	require.Zero(t, s.receivedCacheVersion(node, model.ClusterEventInvalidateCacheForTeams))
}

func TestCacheAntiEntropyBoundsPeerMemoryAndPreservesSessionResetSemantics(t *testing.T) {
	s := &Service{handlers: make(map[model.ClusterEvent]einterfaces.ClusterMessageHandler)}
	event := model.ClusterEventInvalidateCacheForUserAttributes
	for index := 0; index < maxCacheStatePeers+10; index++ {
		s.markCacheVersion(fmt.Sprint(index), event, 1)
	}
	require.Len(t, s.cacheVersions.received, maxCacheStatePeers)
	for _, event := range []model.ClusterEvent{model.ClusterEventClearSessionCacheForUser, model.ClusterEventInvalidateWebConnCacheForUser} {
		target, ok := cachePurgeEvent(event)
		require.True(t, ok)
		require.Equal(t, model.ClusterEventInvalidateAllCaches, target, "empty user ID must not masquerade as an all-user invalidation")
	}
	// A summary stays small even after all supported cache scopes have changed.
	for _, event := range []model.ClusterEvent{model.ClusterEventInvalidateCacheForUser, model.ClusterEventInvalidateCacheForRoles} {
		s.cacheEnvelope(&model.ClusterMessage{Event: event, Data: []byte("must-not-be-retained")})
	}
	data, err := json.Marshal(cacheState{Protocol: 1, Node: model.NewId(), Versions: s.cacheVersions.local})
	require.NoError(t, err)
	require.Less(t, len(data), maxCacheStateBytes)
	require.False(t, strings.Contains(string(data), "must-not-be-retained"))
}
