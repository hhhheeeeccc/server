package elasticsearch_indexing

import (
	"context"
	"database/sql"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

type indexingSQLProxy struct {
	listener  net.Listener
	target    string
	blocked   atomic.Bool
	dropped   atomic.Int64
	mu        sync.Mutex
	links     map[net.Conn]net.Conn
	closed    bool
	workers   sync.WaitGroup
	finished  chan struct{}
	closeOnce sync.Once
}

func newIndexingSQLProxy(t *testing.T, dsn string) (*indexingSQLProxy, string) {
	t.Helper()
	address, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, address.Scheme)
	require.Contains(t, []string{"127.0.0.1", "localhost"}, address.Hostname(), "only isolated loopback PostgreSQL may be proxied")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &indexingSQLProxy{listener: listener, target: address.Host, links: make(map[net.Conn]net.Conn), finished: make(chan struct{})}
	go func() {
		defer close(p.finished)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			p.workers.Add(1)
			go p.relay(client)
		}
	}()
	t.Cleanup(p.Close)
	address.Host = listener.Addr().String()
	query := address.Query()
	query.Set("connect_timeout", "0")
	address.RawQuery = query.Encode()
	return p, address.String()
}

func (p *indexingSQLProxy) relay(client net.Conn) {
	defer p.workers.Done()
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.links[client] = upstream
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.links, client); p.mu.Unlock() }()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(indexingSQLProxyWriter{p: p, target: upstream}, client)
		_ = upstream.Close()
		_ = client.Close()
		close(done)
	}()
	_, _ = io.Copy(indexingSQLProxyWriter{p: p, target: client}, upstream)
	_ = upstream.Close()
	_ = client.Close()
	<-done
}

func (p *indexingSQLProxy) Close() {
	p.closeOnce.Do(func() {
		_ = p.listener.Close()
		<-p.finished
		p.mu.Lock()
		p.closed = true
		for client, upstream := range p.links {
			_ = client.Close()
			_ = upstream.Close()
		}
		p.mu.Unlock()
		p.workers.Wait()
	})
}

type indexingSQLProxyWriter struct {
	p      *indexingSQLProxy
	target io.Writer
}

func (w indexingSQLProxyWriter) Write(data []byte) (int, error) {
	if w.p.blocked.Load() {
		w.p.dropped.Add(int64(len(data)))
		return len(data), nil
	}
	return w.target.Write(data)
}

func TestIndexingExecutionBoundsSilentQuery(t *testing.T) {
	th, _, _ := setupIndexer(t)
	w := New(th.Server).MakeWorker().(*checkpointWorker)
	proxy, dsn := newIndexingSQLProxy(t, w.databaseDSN)
	execution, err := newIndexingExecution(dsn, 2*time.Second)
	require.NoError(t, err)
	defer func() { proxy.Close(); execution.Close() }()
	job := saveRecoveryJob(t, th, model.JobStatusPending, model.JobTypeElasticsearchPostIndexing, nil)
	acquired, err := execution.acquire(context.Background(), job.Id)
	require.NoError(t, err)
	require.True(t, acquired)
	claimed, err := execution.claim(job.Id)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	execution.timeout = 100 * time.Millisecond
	proxy.blocked.Store(true)
	completed := make(chan error, 1)
	started := time.Now()
	go func() { completed <- execution.check(context.Background()) }()
	select {
	case err := <-completed:
		require.ErrorIs(t, err, errIndexingOwnershipLost)
	case <-time.After(700 * time.Millisecond):
		t.Fatal("ownership query remained blocked after its SQL deadline and silent cancellation request")
	}
	require.Less(t, time.Since(started), 700*time.Millisecond)
	require.Positive(t, proxy.dropped.Load(), "the live query must encounter actual discarded network traffic")
	require.True(t, execution.lost.Load())
	_, err = execution.claim(job.Id)
	require.ErrorIs(t, err, errIndexingOwnershipLost)
	// Closing the owning TCP socket must release its advisory lock at the real
	// PostgreSQL server; a subsequent independent recovery can then proceed.
	require.Eventually(t, func() bool {
		if w.recoverJobs(make(chan struct{})) != nil {
			return false
		}
		current, err := th.Store.Job().Get(th.Context, job.Id)
		return err == nil && current.Status == model.JobStatusPending
	}, 3*time.Second, 20*time.Millisecond)
}

func TestIndexingRecoveryBoundsSilentDatabase(t *testing.T) {
	th, _, _ := setupIndexer(t)
	for _, stopEarly := range []bool{false, true} {
		name := "deadline"
		if stopEarly {
			name = "stop"
		}
		t.Run(name, func(t *testing.T) {
			w := New(th.Server).MakeWorker().(*checkpointWorker)
			dsn, entered, _, cleanup := indexingSilentDatabase(t)
			// Both old and new candidate-reader paths point only at the silent
			// fixture. No application database pool is closed or modified.
			db, err := sql.Open("postgres", dsn)
			require.NoError(t, err)
			defer func() { cleanup(); _ = db.Close() }()
			w.master, w.databaseDSN = db, dsn
			w.queryTimeout = 100 * time.Millisecond
			stop := make(chan struct{})
			completed := make(chan error, 1)
			started := time.Now()
			go func() { completed <- w.recoverJobs(stop) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("recovery did not reach the silent database")
			}
			if stopEarly {
				close(stop)
			}
			select {
			case err := <-completed:
				require.Error(t, err)
			case <-time.After(700 * time.Millisecond):
				t.Fatal("recovery remained blocked on a silent database despite deadline or stop")
			}
			require.Less(t, time.Since(started), time.Second)
		})
	}
}
