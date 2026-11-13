package elasticsearch_indexing

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The fixture accepts and drains TCP but never sends PostgreSQL startup or
// cancellation replies. It never connects to an application database.
func indexingSilentDatabase(t *testing.T) (string, <-chan struct{}, *atomic.Int32, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var clients []net.Conn
	var workers sync.WaitGroup
	var count atomic.Int32
	entered := make(chan struct{}, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			clients = append(clients, client)
			mu.Unlock()
			count.Add(1)
			select {
			case entered <- struct{}{}:
			default:
			}
			workers.Add(1)
			go func() { defer workers.Done(); _, _ = io.Copy(io.Discard, client); _ = client.Close() }()
		}
	}()
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			_ = listener.Close()
			<-finished
			mu.Lock()
			for _, client := range clients {
				_ = client.Close()
			}
			mu.Unlock()
			workers.Wait()
		})
	}
	t.Cleanup(cleanup)
	return fmt.Sprintf("postgres://fixture:fixture@%s/fixture?sslmode=disable&connect_timeout=0", listener.Addr()), entered, &count, cleanup
}

func TestIndexingExecutionBoundsSilentStartup(t *testing.T) {
	dsn, _, count, cleanup := indexingSilentDatabase(t)
	execution, err := newIndexingExecution(dsn, 100*time.Millisecond)
	require.NoError(t, err)
	// Fixture cleanup closes the silent peer before DB.Close even on failure.
	defer func() { cleanup(); execution.Close() }()
	completed := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := execution.acquire(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaa")
		completed <- err
	}()
	select {
	case err := <-completed:
		require.Error(t, err)
	case <-time.After(700 * time.Millisecond):
		t.Fatal("startup ignored the execution SQL deadline while PostgreSQL remained silent")
	}
	require.Less(t, time.Since(started), 700*time.Millisecond)
	require.True(t, execution.lost.Load())
	_, err = execution.acquire(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.Error(t, err)
	require.EqualValues(t, 1, count.Load(), "an expired execution must not reconnect")
	execution.Close()
}

func TestIndexingExecutionCancelsSilentStartup(t *testing.T) {
	dsn, entered, _, cleanup := indexingSilentDatabase(t)
	execution, err := newIndexingExecution(dsn, 5*time.Second)
	require.NoError(t, err)
	defer func() { cleanup(); execution.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := make(chan error, 1)
	go func() { _, err := execution.acquire(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaa"); completed <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("silent startup fixture was not reached")
	}
	started := time.Now()
	cancel()
	select {
	case err := <-completed:
		require.Error(t, err)
	case <-time.After(700 * time.Millisecond):
		t.Fatal("cancel did not interrupt a silent PostgreSQL startup")
	}
	require.Less(t, time.Since(started), 700*time.Millisecond)
	require.True(t, execution.lost.Load())
	execution.Close()
}
