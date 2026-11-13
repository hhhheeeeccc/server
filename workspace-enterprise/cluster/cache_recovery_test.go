package cluster

import (
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/stretchr/testify/require"
)

func TestMembershipRecoveryPurgesAllNativeCacheScopes(t *testing.T) {
	// These 34 events are registered by the native localcachelayer. Empty Data
	// is their full-cache purge sentinel, including scopes outside the platform
	// InvalidateAllCachesSkipSend implementation (roles, emojis, attributes).
	events := []model.ClusterEvent{
		model.ClusterEventInvalidateCacheForReactions,
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
	}
	s := &Service{handlers: make(map[model.ClusterEvent]einterfaces.ClusterMessageHandler)}
	counts := make(map[model.ClusterEvent]int)
	for _, event := range events {
		require.True(t, membershipCachePurgeEvent(event), event)
		s.RegisterClusterMessageHandler(event, func(message *model.ClusterMessage) {
			require.Equal(t, event, message.Event)
			require.Empty(t, message.Data)
			counts[message.Event]++
			// The service mutex must not be held while a native handler runs.
			s.RegisterClusterMessageHandler("not-a-cache", nil)
		})
	}
	for _, event := range []model.ClusterEvent{
		model.ClusterEventClearSessionCacheForAllUsers,
		model.ClusterEventClearSessionCacheForUser,
		model.ClusterEventInvalidateWebConnCacheForUser,
		model.ClusterEventInvalidateAllCaches,
		model.ClusterEventPublish,
		model.ClusterEventUpdateStatus,
		model.ClusterEventInstallPlugin,
		model.ClusterEventPluginEvent,
		model.ClusterEventUpdateSessionAttributes,
		"inv_unsupported_cache",
	} {
		require.False(t, membershipCachePurgeEvent(event), event)
		s.RegisterClusterMessageHandler(event, func(*model.ClusterMessage) {
			t.Errorf("membership recovery called unsafe event %s", event)
		})
	}
	s.purgeMembershipStoreCaches()
	require.Len(t, counts, len(events))
	for _, event := range events {
		require.Equal(t, 1, counts[event], event)
	}
}

func TestMembershipRecoveryQueuesAndStops(t *testing.T) {
	t.Run("membership callbacks coalesce without waiting for handlers", func(t *testing.T) {
		stop := make(chan struct{})
		s := &Service{id: model.NewId(), stop: stop, cacheRecovery: make(chan struct{}, 1)}
		s.NotifyJoin(&memberlist.Node{Name: s.id})
		s.NotifyLeave(&memberlist.Node{Name: s.id})
		require.Empty(t, s.cacheRecovery)
		for range 100 {
			s.NotifyJoin(&memberlist.Node{Name: model.NewId()})
			s.NotifyLeave(&memberlist.Node{Name: model.NewId()})
		}
		require.Len(t, s.cacheRecovery, 1)
		incoming := make(chan envelope, 10)
		done := make(chan struct{})
		go func() { defer close(done); s.cacheRecoveryLoop(stop, s.cacheRecovery, incoming) }()
		require.Eventually(t, func() bool { return len(incoming) == 1 }, 3*time.Second, 10*time.Millisecond)
		close(stop)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("recovery worker did not stop")
		}
		require.Len(t, incoming, 1)
		require.True(t, (<-incoming).cacheRecoveryRequested)
	})
	t.Run("stop cancels a debounce instead of executing cache work", func(t *testing.T) {
		stop := make(chan struct{})
		queue := make(chan struct{}, 1)
		queue <- struct{}{}
		incoming := make(chan envelope, 1)
		done := make(chan struct{})
		s := &Service{stop: stop, cacheRecovery: queue}
		go func() { defer close(done); s.cacheRecoveryLoop(stop, queue, incoming) }()
		require.Eventually(t, func() bool { return len(queue) == 0 }, time.Second, time.Millisecond)
		close(stop)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("debounce did not cancel")
		}
		require.Empty(t, incoming)
		s.NotifyLeave(&memberlist.Node{Name: model.NewId()})
		require.Empty(t, queue)
	})
	t.Run("a full receiver queue can be interrupted by shutdown", func(t *testing.T) {
		stop := make(chan struct{})
		queue := make(chan struct{}, 1)
		queue <- struct{}{}
		incoming := make(chan envelope)
		done := make(chan struct{})
		s := &Service{}
		go func() { defer close(done); s.cacheRecoveryLoop(stop, queue, incoming) }()
		// Past the debounce, the worker is blocked delivering the only recovery
		// signal. It must still exit without requiring a receiver to consume it.
		time.Sleep(1100 * time.Millisecond)
		select {
		case <-done:
			t.Fatal("recovery signal was discarded when the receiver was full")
		default:
		}
		close(stop)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("full receiver queue prevented shutdown")
		}
	})
}
