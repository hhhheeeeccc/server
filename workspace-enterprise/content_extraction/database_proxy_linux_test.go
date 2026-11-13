package content_extraction_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

type extractionSQLLink struct {
	client, server net.Conn
	blocked        atomic.Bool
}

type extractionSQLProxy struct {
	listener     net.Listener
	target       string
	role         string
	dropped      atomic.Int64
	entered      chan struct{}
	seen         sync.Once
	unblock      atomic.Bool
	dropAt       atomic.Int64
	queries      atomic.Int64
	responseOnly atomic.Bool
	mu           sync.Mutex
	links        map[*extractionSQLLink]struct{}
	closed       bool
	events       []string
	workers      sync.WaitGroup
	accepted     chan struct{}
	once         sync.Once
}

func newExtractionSQLProxy(t *testing.T, dsn, role string) (*extractionSQLProxy, string) {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", u.Hostname())
	require.Equal(t, "disable", u.Query().Get("sslmode"), "the isolated protocol fixture requires plaintext PostgreSQL")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &extractionSQLProxy{listener: listener, target: u.Host, role: role, entered: make(chan struct{}),
		links: make(map[*extractionSQLLink]struct{}), accepted: make(chan struct{})}
	go func() {
		defer close(p.accepted)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			p.workers.Add(1)
			go p.relay(client)
		}
	}()
	t.Cleanup(func() {
		p.Close()
		if t.Failed() {
			for _, event := range p.events {
				t.Log(event)
			}
		}
	})
	u.Host = listener.Addr().String()
	return p, u.String()
}

func (p *extractionSQLProxy) relay(client net.Conn) {
	defer p.workers.Done()
	defer client.Close()
	trace := func(format string, args ...any) {
		p.mu.Lock()
		if len(p.events) < 2048 {
			p.events = append(p.events, fmt.Sprintf("%s %s", client.RemoteAddr(), fmt.Sprintf(format, args...)))
		}
		p.mu.Unlock()
	}
	trace("connect unblock=%t", p.unblock.Load())
	defer trace("disconnected")
	link := &extractionSQLLink{client: client}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.links[link] = struct{}{}
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.links, link); p.mu.Unlock() }()
	var length [4]byte
	if _, err := io.ReadFull(client, length[:]); err != nil {
		return
	}
	n := int(binary.BigEndian.Uint32(length[:]))
	if n < 8 || n > 65536 {
		return
	}
	startup := make([]byte, n)
	copy(startup, length[:])
	if _, err := io.ReadFull(client, startup[4:]); err != nil {
		return
	}
	if binary.BigEndian.Uint32(startup[4:8]) == 80877102 && !p.unblock.Load() {
		trace("cancel dropped")
		// A native cancellation connection must not accidentally rescue a
		// silently dropped query. The owner must bound and close it too.
		p.dropped.Add(int64(n))
		_, _ = io.Copy(io.Discard, client)
		return
	}
	upstream, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	p.mu.Lock()
	link.server = upstream
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return
	}
	if _, err := upstream.Write(startup); err != nil {
		return
	}
	responseDone := make(chan struct{})
	go func() {
		defer close(responseDone)
		_, _ = io.Copy(extractionSQLResponse{p: p, link: link}, upstream)
		_ = client.Close()
	}()
	defer func() { _ = upstream.Close(); _ = client.Close(); <-responseDone }()
	roleMatches := bytes.Contains(startup, []byte("workspace_content_extraction_"+p.role))
	trace("startup target=%t unblock=%t", roleMatches, p.unblock.Load())
	for {
		var header [5]byte
		if _, err := io.ReadFull(client, header[:]); err != nil {
			trace("client read ended: %T", err)
			return
		}
		n := int(binary.BigEndian.Uint32(header[1:]))
		if n < 4 || n > 16*1024*1024 {
			return
		}
		message := make([]byte, n+1)
		copy(message, header[:])
		if _, err := io.ReadFull(client, message[5:]); err != nil {
			return
		}
		if roleMatches && !p.unblock.Load() && (header[0] == 'P' || header[0] == 'Q') {
			if p.queries.Add(1) >= max(1, p.dropAt.Load()) {
				link.blocked.Store(true)
				p.seen.Do(func() { close(p.entered) })
			}
		}
		trace("message %c length=%d unblock=%t blocked=%t", header[0], n, p.unblock.Load(), link.blocked.Load())
		if link.blocked.Load() && !p.responseOnly.Load() {
			p.dropped.Add(int64(len(message)))
			continue
		}
		if _, err := upstream.Write(message); err != nil {
			return
		}
	}
}

type extractionSQLResponse struct {
	p    *extractionSQLProxy
	link *extractionSQLLink
}

func (w extractionSQLResponse) Write(data []byte) (int, error) {
	if w.link.blocked.Load() {
		w.p.dropped.Add(int64(len(data)))
		return len(data), nil
	}
	return w.link.client.Write(data)
}

func (p *extractionSQLProxy) blocked() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for link := range p.links {
		if link.blocked.Load() {
			count++
		}
	}
	return count
}

func (p *extractionSQLProxy) Close() {
	p.once.Do(func() {
		_ = p.listener.Close()
		<-p.accepted
		p.mu.Lock()
		p.closed = true
		for link := range p.links {
			_ = link.client.Close()
			if link.server != nil {
				_ = link.server.Close()
			}
		}
		p.mu.Unlock()
		p.workers.Wait()
	})
}

func setupSQLProxy(t *testing.T, role string, queryTimeout int) (*api4.TestHelper, *extractionSQLProxy) {
	t.Helper()
	var proxy *extractionSQLProxy
	var dsn string
	th := setupWithConfig(t, func(cfg *model.Config) {
		// api4 reapplies this callback after creating the server. Reuse the
		// fixture rather than stacking a second silently blocked proxy.
		if proxy == nil {
			proxy, dsn = newExtractionSQLProxy(t, *cfg.SqlSettings.DataSource, role)
		}
		cfg.SqlSettings.DataSource = &dsn
		if queryTimeout > 0 {
			cfg.SqlSettings.QueryTimeout = &queryTimeout
		}
	})
	return th, proxy
}

func TestRegisteredExtractionBoundsSilentBatchRead(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel", "stop"} {
		t.Run(mode, func(t *testing.T) {
			timeout := 30
			if mode == "deadline" {
				timeout = 1
			}
			// Cancellation and Stop must return before the SQL deadline, so
			// a timeout cannot accidentally satisfy those assertions.
			th, proxy := setupSQLProxy(t, "reader", timeout)
			info := upload(t, th, "after network recovers", "silent.txt", time.Now().Add(-time.Minute))
			job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeExtractContent, nil)
			require.Nil(t, appErr)
			select {
			case <-proxy.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the centrally registered worker did not send a batch query through its configured SQL proxy")
			}
			status := model.JobStatusError
			if mode == "cancel" {
				_, err := th.SystemAdminClient.CancelJob(context.Background(), job.Id)
				require.NoError(t, err)
				status = model.JobStatusCanceled
			} else if mode == "stop" {
				finished := make(chan error, 1)
				go func() { finished <- th.Server.Jobs.StopWorkers() }()
				select {
				case err := <-finished:
					require.NoError(t, err)
				case <-time.After(2 * time.Second):
					t.Fatal("StopWorkers remained blocked by silent batch SQL")
				}
				status = model.JobStatusPending
			}
			require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == status }, 5*time.Second, 20*time.Millisecond)
			require.Eventually(t, func() bool { return proxy.blocked() == 0 }, time.Second, 20*time.Millisecond)
			require.Positive(t, proxy.dropped.Load(), "the fixture must discard actual PostgreSQL traffic")
			require.Empty(t, get(t, th, info.Id).Content)
			require.Equal(t, "0", currentJob(t, th, job.Id).Data["processed"])
			pingCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			require.NoError(t, th.Store.GetInternalMasterDB().PingContext(pingCtx), "private socket cancellation must not close the application pool")
			cancel()
			proxy.unblock.Store(true)
			if mode == "stop" {
				require.NoError(t, th.Server.Jobs.StartWorkers())
				require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == model.JobStatusSuccess }, 10*time.Second, 20*time.Millisecond)
			} else {
				run(t, th, nil, model.JobStatusSuccess)
			}
			require.Equal(t, "after network recovers", get(t, th, info.Id).Content)
		})
	}
}

func TestRegisteredExtractionBoundsSilentCancellationRead(t *testing.T) {
	for _, mode := range []string{"deadline", "stop"} {
		t.Run(mode, func(t *testing.T) {
			th, proxy := setupSQLProxy(t, "control", 0)
			blocked, tail, job, release := stalledExtraction(t, th, false)
			select {
			case <-proxy.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("no real cancellation-status query reached the silent proxy")
			}
			status := model.JobStatusError
			if mode == "stop" {
				finished := make(chan error, 1)
				go func() { finished <- th.Server.Jobs.StopWorkers() }()
				select {
				case err := <-finished:
					require.NoError(t, err)
				case <-time.After(2 * time.Second):
					t.Fatal("StopWorkers did not join its silent cancellation monitor")
				}
				status = model.JobStatusPending
			}
			require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == status }, 6*time.Second, 20*time.Millisecond)
			require.Eventually(t, func() bool { return proxy.blocked() == 0 }, time.Second, 20*time.Millisecond)
			require.Positive(t, proxy.dropped.Load())
			require.Empty(t, get(t, th, tail.Id).Content)
			release()
			path := filepath.Join(*th.App.Config().FileSettings.Directory, blocked.Path)
			waitNoNativeReader(t, path)
			require.NoError(t, os.Remove(path))
			_, err := th.Server.FileBackend().WriteFile(strings.NewReader("resumed control"), blocked.Path)
			require.NoError(t, err)
			proxy.unblock.Store(true)
			if mode == "stop" {
				require.NoError(t, th.Server.Jobs.StartWorkers())
				require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == model.JobStatusSuccess }, 10*time.Second, 20*time.Millisecond)
			} else {
				run(t, th, nil, model.JobStatusSuccess)
			}
			require.Equal(t, "resumed control", get(t, th, blocked.Id).Content)
		})
	}
}

func TestRegisteredExtractionBoundsSilentCandidateSelection(t *testing.T) {
	th, proxy := setupSQLProxy(t, "selector", 0)
	blocked, tail, job, release := stalledExtraction(t, th, true)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings.ExtractContent = new(false) })
	require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == model.JobStatusPending }, 4*time.Second, 20*time.Millisecond)
	paused := currentJob(t, th, job.Id)
	require.Equal(t, "true", paused.Data["workspace_extraction_paused"])
	select {
	case <-proxy.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the native watcher did not trigger a real successor-selection query behind its paused candidate")
	}
	finished := make(chan error, 1)
	go func() { finished <- th.Server.Jobs.StopWorkers() }()
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("StopWorkers did not cancel its silent candidate-selection read")
	}
	require.Eventually(t, func() bool { return proxy.blocked() == 0 }, time.Second, 20*time.Millisecond)
	require.Positive(t, proxy.dropped.Load())
	require.Equal(t, model.JobStatusPending, currentJob(t, th, job.Id).Status)
	require.Equal(t, paused.StartAt, currentJob(t, th, job.Id).StartAt, "the paused job must not be reclaimed while selection is blocked")
	require.Empty(t, get(t, th, tail.Id).Content)
	release()
	path := filepath.Join(*th.App.Config().FileSettings.Directory, blocked.Path)
	waitNoNativeReader(t, path)
	require.NoError(t, os.Remove(path))
	_, err := th.Server.FileBackend().WriteFile(strings.NewReader("resumed selector"), blocked.Path)
	require.NoError(t, err)
	proxy.unblock.Store(true)
	th.App.UpdateConfig(func(cfg *model.Config) { cfg.FileSettings.ExtractContent = new(true) })
	require.NoError(t, th.Server.Jobs.StartWorkers())
	require.Eventually(t, func() bool { return currentJob(t, th, job.Id).Status == model.JobStatusSuccess }, 10*time.Second, 20*time.Millisecond)
	require.Equal(t, "resumed selector", get(t, th, blocked.Id).Content)
	require.Equal(t, "must wait", get(t, th, tail.Id).Content)
}
