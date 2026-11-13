package cluster

// A failed transport must not leave application caches permanently stale when
// shared SQL remains usable. These channels belong to this worker, not the
// transport: retrying start replaces transport queues and must not restart the
// thirty-second recheck or strand its receiver. lifecycle protects the pointer.
type localCacheRecovery struct {
	stop     chan struct{}
	done     chan struct{}
	incoming chan envelope
}

// Caller holds lifecycle. Repeated failed starts retain the same worker/timer.
func (s *Service) startLocalCacheRecovery() {
	if s.localCacheRecovery != nil {
		return
	}
	worker := &localCacheRecovery{
		stop: make(chan struct{}), done: make(chan struct{}),
		incoming: make(chan envelope),
	}
	s.localCacheRecovery = worker
	receiverDone := make(chan struct{})
	go func() {
		defer close(receiverDone)
		// The existing receiver enforces both license and Cluster.Enable, and
		// uses native store/session/WebConn invalidation without token removal.
		s.receiveLoop(worker.stop, worker.incoming)
	}()
	go func() {
		defer close(worker.done)
		s.cacheRecoveryLoop(worker.stop, nil, worker.incoming)
		<-receiverDone
	}()
	s.ps.Log().Warn("Local cache recovery started without cluster transport")
}

// Caller holds lifecycle. Called even when list is nil, and before a successful
// retry starts the normal receiver. A running native purge is allowed to finish.
func (s *Service) stopLocalCacheRecovery() {
	if worker := s.localCacheRecovery; worker != nil {
		close(worker.stop)
		<-worker.done
		s.localCacheRecovery = nil
	}
}
