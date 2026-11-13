package cluster

import (
	"net"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestMembershipRecheckRunsWithoutCompleteDiscovery(t *testing.T) {
	s := &Service{id: model.NewId()}
	stop := make(chan struct{})
	ticks := make(chan time.Time)
	incoming := make(chan envelope, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.cacheRecoveryLoopWithTicks(stop, nil, incoming, ticks)
	}()
	t.Cleanup(func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("periodic recovery worker did not stop")
		}
	})
	tick := func() {
		t.Helper()
		select {
		case ticks <- time.Now():
		case <-time.After(time.Second):
			t.Fatal("recovery worker did not receive a tick")
		}
		select {
		case event := <-incoming:
			require.True(t, event.cacheRecoveryRequested)
		case <-time.After(time.Second):
			t.Fatal("periodic cache recovery must not depend on complete discovery")
		}
	}
	// A fresh process can know neither a live member nor a missing writer.
	tick()
	peer := &memberlist.Node{Name: model.NewId(), Addr: net.ParseIP("127.0.0.2"), Port: 8074}
	s.NotifyJoin(peer)
	s.NotifyLeave(&memberlist.Node{Name: s.id})
	tick()
	s.NotifyLeave(peer)
	tick()
	// No new membership notification: later writes still need another purge.
	tick()
	other := &memberlist.Node{Name: model.NewId(), Addr: net.ParseIP("127.0.0.3"), Port: 8074}
	s.NotifyLeave(other)
	s.NotifyJoin(peer)
	tick()
	// Even a fully connected known membership cannot prove that no unseen
	// writer exists. Rejoining must not turn the periodic fallback off.
	// Restarting a peer changes its process ID but retains its endpoint.
	s.NotifyJoin(&memberlist.Node{Name: model.NewId(), Addr: other.Addr, Port: other.Port})
	tick()
	// A graceful departure may conceal an in-flight write too; no timeout
	// silently erases the knowledge that another SQL writer is absent.
	s.NotifyLeave(peer)
	tick()
}

func TestMembershipRecheckBoundsMemoryWithoutForgettingMissingWriters(t *testing.T) {
	s := &Service{}
	for i := range maxCacheMissingPeers {
		node := &memberlist.Node{Name: model.NewId(), Addr: net.ParseIP("127.0.0.2"), Port: uint16(10000 + i)}
		s.NotifyLeave(node)
		s.NotifyLeave(node)
	}
	require.Len(t, s.cacheMissingPeers, maxCacheMissingPeers)
	require.False(t, s.cacheMissingPeerOverflow, "duplicate leaves must not trigger overflow")
	s.NotifyLeave(&memberlist.Node{Name: model.NewId(), Addr: net.ParseIP("127.0.0.3"), Port: 8074})
	require.Len(t, s.cacheMissingPeers, maxCacheMissingPeers)
	for i := range maxCacheMissingPeers {
		s.NotifyJoin(&memberlist.Node{Name: model.NewId(), Addr: net.ParseIP("127.0.0.2"), Port: uint16(10000 + i)})
	}
	require.Empty(t, s.cacheMissingPeers)
	require.True(t, s.hasMissingCachePeers(), "untracked departures must remain marked as potentially missing")
}

func TestMembershipRecheckShutdownWithFullReceiverAndWorkerRestart(t *testing.T) {
	s := &Service{}
	s.NotifyLeave(&memberlist.Node{Name: model.NewId(), Addr: net.ParseIP("127.0.0.2"), Port: 8074})
	for range 2 {
		stop := make(chan struct{})
		ticks := make(chan time.Time)
		incoming := make(chan envelope)
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.cacheRecoveryLoopWithTicks(stop, nil, incoming, ticks)
		}()
		select {
		case ticks <- time.Now():
		case <-time.After(time.Second):
			close(stop)
			t.Fatal("restarted recovery worker did not receive a tick")
		}
		// A full receiver must retain the periodic signal and remain
		// interruptible. Restarting only the worker must preserve periodic
		// delivery, regardless of whether a writer can be discovered.
		select {
		case event := <-incoming:
			require.True(t, event.cacheRecoveryRequested)
		case <-time.After(time.Second):
			close(stop)
			t.Fatal("restarted recovery worker lost its periodic request")
		}
		select {
		case ticks <- time.Now():
		case <-time.After(time.Second):
			close(stop)
			t.Fatal("periodic recovery worker failed to receive another tick")
		}
		close(stop)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("full receiver blocked periodic recovery shutdown")
		}
	}
}
