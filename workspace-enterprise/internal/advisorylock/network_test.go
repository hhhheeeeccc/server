package advisorylock

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// Speak just enough PostgreSQL protocol to finish authentication, then go
// silent on a chosen SQL operation. A TCP connection alone cannot expose a
// driver blocked in BEGIN after it has removed the startup deadline.
func silentDatabase(t *testing.T, stage string) (string, <-chan struct{}, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	done := make(chan struct{})
	connections := make(chan net.Conn, 8)
	var workers sync.WaitGroup
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections <- conn
			workers.Add(1)
			go func() {
				defer workers.Done()
				var size [4]byte
				if _, err := io.ReadFull(conn, size[:]); err != nil {
					return
				}
				length := binary.BigEndian.Uint32(size[:])
				if length < 8 || length > 65536 {
					return
				}
				startup := make([]byte, length-4)
				if _, err := io.ReadFull(conn, startup); err != nil {
					return
				}
				if binary.BigEndian.Uint32(startup[:4]) != 196608 {
					// Cancellation connections need not receive any response.
					return
				}
				_, _ = conn.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 0, 'K', 0, 0, 0, 12, 0, 0, 0, 1, 0, 0, 0, 2, 'Z', 0, 0, 0, 5, 'I'})
				locks := 0
				for {
					var header [5]byte
					if _, err := io.ReadFull(conn, header[:]); err != nil {
						return
					}
					length := binary.BigEndian.Uint32(header[1:])
					if length < 4 || length > 65536 {
						return
					}
					body := make([]byte, length-4)
					if _, err := io.ReadFull(conn, body); err != nil {
						return
					}
					if header[0] == 'Q' && strings.HasPrefix(string(body), "BEGIN") && stage != "begin" {
						_, _ = conn.Write([]byte{'C', 0, 0, 0, 10, 'B', 'E', 'G', 'I', 'N', 0, 'Z', 0, 0, 0, 5, 'T'})
						continue
					}
					if header[0] == 'P' && strings.Contains(string(body), "pg_advisory_xact_lock") {
						locks++
						if stage == "release" || (stage == "additional" && locks == 1) {
							continue
						}
						close(entered)
						return
					}
					if header[0] == 'S' {
						// Unnamed extended query with binary_parameters=yes:
						// ParseComplete, BindComplete, NoData, command, ready.
						_, _ = conn.Write([]byte{'1', 0, 0, 0, 4, '2', 0, 0, 0, 4, 'n', 0, 0, 0, 4,
							'C', 0, 0, 0, 13, 'S', 'E', 'L', 'E', 'C', 'T', ' ', '1', 0, 'Z', 0, 0, 0, 5, 'T'})
						continue
					}
					if header[0] == 'Q' {
						close(entered)
						return
					}
				}
			}()
		}
	}()
	var once sync.Once
	closeServer := func() {
		once.Do(func() {
			_ = listener.Close()
			<-done
			for {
				select {
				case conn := <-connections:
					_ = conn.Close()
				default:
					workers.Wait()
					return
				}
			}
		})
	}
	return "postgres://fixture@" + listener.Addr().String() + "/fixture?sslmode=disable&binary_parameters=yes", entered, closeServer
}

func TestAdditionalLockCancellationInterruptsSilentDatabase(t *testing.T) {
	dsn, entered, closeServer := silentDatabase(t, "additional")
	defer closeServer()
	locker := New(dsn)
	owner, cancelOwner := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelOwner()
	lease, err := locker.AcquireLease(owner, 41)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	ctx, cancel := context.WithCancel(owner)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lease.Lock(ctx, 42) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture did not receive the additional lock")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected cancellation, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("shorter additional-lock context did not interrupt its socket")
		closeServer()
		<-done
	}
	if owner.Err() != nil {
		t.Error("additional cancellation incorrectly required owner cancellation")
	}
	lease.Release()
	if len(locker.gate) != 0 {
		t.Error("release retained connection permit")
	}
}

func TestReleaseInterruptsSilentRollback(t *testing.T) {
	dsn, entered, closeServer := silentDatabase(t, "release")
	defer closeServer()
	locker := New(dsn)
	lease, err := locker.AcquireLease(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { lease.Release(); close(done) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture did not receive ROLLBACK")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("silent ROLLBACK retained the lease indefinitely")
		closeServer()
		<-done
	}
	lease.Release() // Normal and cancellation cleanup remain idempotent.
	if len(locker.gate) != 0 {
		t.Error("release retained connection permit")
	}
}

func TestQueryCancellationInterruptsSilentDatabase(t *testing.T) {
	dsn, entered, closeServer := silentDatabase(t, "additional")
	defer closeServer()
	locker := New(dsn)
	owner, stopOwner := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopOwner()
	lease, err := locker.AcquireLease(owner, 41)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	ctx, cancel := context.WithCancel(owner)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- lease.Query(ctx, "SELECT pg_sleep(30)", nil, func(*sql.Rows) error {
			t.Error("silent query must not reach row consumption")
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture did not receive the identity query")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected cancellation, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("query cancellation did not interrupt its socket")
		closeServer()
		<-done
	}
	if owner.Err() != nil {
		t.Error("shorter query cancellation incorrectly required owner cancellation")
	}
	lease.Release()
	if len(locker.gate) != 0 {
		t.Error("query cancellation retained connection permit")
	}
}

func TestCancellationInterruptsSilentDatabase(t *testing.T) {
	for _, stage := range []string{"begin", "lock"} {
		t.Run(stage, func(t *testing.T) {
			dsn, entered, closeServer := silentDatabase(t, stage)
			defer closeServer()
			locker := New(dsn)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				release, err := locker.Acquire(ctx, 42)
				if release != nil {
					release()
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("fixture did not receive the SQL operation")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("expected context cancellation, got %v", err)
				}
			case <-time.After(500 * time.Millisecond):
				t.Error("canceled coordination operation remained blocked on a silent PostgreSQL socket")
				closeServer()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("coordination operation did not exit after fixture shutdown")
				}
			}
			if len(locker.gate) != 0 {
				t.Error("canceled coordination operation retained its connection permit")
			}
		})
	}
}
