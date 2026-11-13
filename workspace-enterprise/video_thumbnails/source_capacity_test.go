package video_thumbnails

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestThumbnailSourceIsNotOpenedAtCapacity(t *testing.T) {
	require.Empty(t, thumbnailSlots)
	for range cap(thumbnailSlots) {
		thumbnailSlots <- struct{}{}
		defer func() { <-thumbnailSlots }()
	}
	opens := 0
	_, err := extractVideoThumbnailFromSource(context.Background(), func() (io.ReadCloser, error) {
		opens++
		return io.NopCloser(strings.NewReader("video")), nil
	}, 1024)
	require.ErrorContains(t, err, "capacity reached")
	require.Zero(t, opens)
}

func TestThumbnailCanceledSourceIsNotOpened(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opens := 0
	_, err := extractVideoThumbnailFromSource(ctx, func() (io.ReadCloser, error) {
		opens++
		return io.NopCloser(strings.NewReader("video")), nil
	}, 1024)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, opens)
	require.Empty(t, thumbnailSlots)
}

func TestThumbnailBurstBoundsConcurrentSourceOpens(t *testing.T) {
	require.Empty(t, thumbnailSlots)
	const requests = 12
	var active, peak, opens atomic.Int32
	release := make(chan struct{})
	dispositions := make(chan struct{}, requests*2)
	errorsSeen := make(chan error, requests)
	var workers sync.WaitGroup
	var once sync.Once
	finish := func() {
		once.Do(func() { close(release) })
		workers.Wait()
	}
	t.Cleanup(finish)
	sourceError := errors.New("controlled source failure after release")
	open := func() (io.ReadCloser, error) {
		opens.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		dispositions <- struct{}{}
		<-release
		return nil, sourceError
	}
	for range requests {
		workers.Go(func() {
			_, err := extractVideoThumbnailFromSource(context.Background(), open, 1024)
			errorsSeen <- err
			dispositions <- struct{}{}
		})
	}
	// Each request has either entered its blocked opener or already been rejected.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for range requests {
		select {
		case <-dispositions:
		case <-deadline.C:
			t.Fatal("requests did not reach their initial disposition")
		}
	}
	finish()
	t.Logf("requests=%d; source opens=%d; peak concurrent openers=%d", requests, opens.Load(), peak.Load())
	require.Equal(t, int32(cap(thumbnailSlots)), opens.Load())
	require.Equal(t, int32(cap(thumbnailSlots)), peak.Load())
	require.Zero(t, active.Load())
	require.Empty(t, thumbnailSlots)
	for range requests {
		err := <-errorsSeen
		require.Error(t, err)
		require.True(t, errors.Is(err, sourceError) || strings.Contains(err.Error(), "capacity reached"), "%v", err)
	}
}

func TestThumbnailSourceFailureReleasesCapacity(t *testing.T) {
	failure := errors.New("storage unavailable")
	for range cap(thumbnailSlots) + 1 {
		_, err := extractVideoThumbnailFromSource(context.Background(), func() (io.ReadCloser, error) {
			return nil, failure
		}, 1024)
		require.ErrorIs(t, err, failure)
		require.Empty(t, thumbnailSlots)
	}
}
