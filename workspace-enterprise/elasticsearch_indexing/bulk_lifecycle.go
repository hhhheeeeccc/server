package elasticsearch_indexing

import (
	"context"
	"errors"
	"sync"
	"time"
)

type indexingWorkContext struct{ ctx context.Context }
type indexingFlushCancelKey struct{}
type indexingFlushContext struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// One session owns a processor and its discovery loop. Native Close is joined
// with a live context: returning early from Close(ctx) can leave native workers
// and callbacks running against a processor that the next job replaces.
type indexingBulkSession struct {
	ctx          context.Context
	work         context.Context
	cancel       context.CancelCauseFunc
	timeout      time.Duration
	closeTimeout time.Duration
	discovery    sync.WaitGroup
}

func newIndexingBulkSession(fence *indexingFence, timeout time.Duration) *indexingBulkSession {
	parent := context.Background()
	if work := fence.work.Load(); work != nil {
		parent = work.ctx
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	return &indexingBulkSession{ctx: ctx, work: parent, cancel: cancel, timeout: timeout, closeTimeout: timeout}
}

func (s *indexingBulkSession) watchFlush(ctx context.Context, state *indexingFlushContext) {
	defer close(state.done)
	select {
	case <-ctx.Done():
		return
	case <-s.work.Done():
	}
	if cause := indexingWorkCause(s.work); cause != nil && cause.err != nil {
		state.cancel()
		return
	}
	// Give a healthy queued flush a short drain. If it stalls, endFlush
	// cancels the whole session so later queued buffers cannot each spend
	// another grace period. Close also has a single cumulative budget.
	timer := time.NewTimer(min(5*time.Second, s.timeout))
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		state.cancel()
	}
}

func (s *indexingBulkSession) add(enqueue func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(s.ctx, s.timeout)
	defer cancel()
	stop := context.AfterFunc(s.work, cancel)
	defer stop()
	if err := s.work.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := enqueue(ctx)
	if err != nil {
		// Do not spend another queue timeout on every remaining batch item.
		s.cancel(err)
	}
	return err
}

func (s *indexingBulkSession) startFlush(context.Context) context.Context {
	ctx, cancel := context.WithTimeout(s.ctx, s.timeout)
	state := &indexingFlushContext{cancel: cancel, done: make(chan struct{})}
	go s.watchFlush(ctx, state)
	return context.WithValue(ctx, indexingFlushCancelKey{}, state)
}

func (s *indexingBulkSession) endFlush(ctx context.Context) {
	state := ctx.Value(indexingFlushCancelKey{}).(*indexingFlushContext)
	err := ctx.Err()
	state.cancel()
	<-state.done
	if err != nil {
		// A timed-out flush is terminal even during normal operation. Do not
		// keep spending request/retry budgets on the remaining queued data.
		cause := context.Cause(s.work)
		if cause == nil {
			cause = err
		}
		s.cancel(cause)
	}
}

// Own discovery rather than launching the clients' unjoined on-start loop.
// A single context bounds a whole discovery, including response decoding.
func (s *indexingBulkSession) discover(run func(context.Context) error, interval time.Duration, report func(error)) {
	s.discovery.Add(1)
	go func() {
		defer s.discovery.Done()
		for {
			ctx, cancel := context.WithTimeout(s.ctx, s.timeout)
			err := run(ctx)
			cancel()
			if err != nil && s.ctx.Err() == nil {
				report(err)
			}
			if interval <= 0 {
				return
			}
			timer := time.NewTimer(interval)
			select {
			case <-s.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func (s *indexingBulkSession) close(processor func(context.Context) error, client func() error) error {
	expired := make(chan struct{})
	timer := time.AfterFunc(s.closeTimeout, func() { s.cancel(context.DeadlineExceeded); close(expired) })
	// Requests use s.ctx, which the timer interrupts. Close must still wait
	// for all native workers/callbacks rather than abandoning its WaitGroup.
	err := processor(context.Background())
	cause := context.Cause(s.ctx)
	if cause == nil {
		cause = context.Cause(s.work)
	}
	s.cancel(nil)
	s.discovery.Wait()
	err = errors.Join(err, cause, client())
	if !timer.Stop() {
		<-expired
		err = errors.Join(err, context.DeadlineExceeded)
	}
	return err
}
