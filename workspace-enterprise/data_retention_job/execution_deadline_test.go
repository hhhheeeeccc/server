package data_retention_job

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Accept actual PostgreSQL startup bytes without answering them. No database
// or application pool participates in this fixture.
func silentRetentionDatabase(t *testing.T) (string, <-chan struct{}, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	entered := make(chan struct{})
	done := make(chan struct{})
	var mu sync.Mutex
	var active net.Conn
	closed := false
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		mu.Lock()
		if closed {
			mu.Unlock()
			return
		}
		active = conn
		mu.Unlock()
		buffer := make([]byte, 1)
		if _, err := io.ReadFull(conn, buffer); err == nil {
			close(entered)
			_, _ = io.Copy(io.Discard, conn)
		}
	}()
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			_ = listener.Close()
			mu.Lock()
			closed = true
			if active != nil {
				_ = active.Close()
			}
			mu.Unlock()
			<-done
		})
	}
	t.Cleanup(cleanup)
	return "postgres://probe@" + listener.Addr().String() + "/probe?sslmode=disable&connect_timeout=0", entered, cleanup
}

func TestRetentionExecutionBoundsSilentStartup(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			dsn, entered, cleanup := silentRetentionDatabase(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			execution, err := newRetentionExecution(ctx, dsn, 150*time.Millisecond)
			require.NoError(t, err)
			defer func() { cleanup(); execution.Close() }()
			finished := make(chan error, 1)
			go func() { finished <- execution.checkSession() }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("retention did not send startup bytes to the silent fixture")
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-finished:
				require.Error(t, err)
			case <-time.After(time.Second):
				t.Fatal("retention remained blocked in PostgreSQL startup after its context expired")
			}
			_, err = execution.exec("SELECT 1")
			require.Error(t, err, "an expired retention session must never reconnect")
		})
	}
}
