package autotranslation

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/config"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/stretchr/testify/require"
)

const publicationProcessEnv = "WORKSPACE_TRANSLATION_PUBLICATION_TEST_PROCESS"

type publicationProcessInput struct {
	Config *model.Config
	JobID  string
}

// The test-binary child bypasses TestMain's database creation and uses only the
// parent's disposable database. Its license is an explicit test fixture.
func runPublicationTestProcess(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var input publicationProcessInput
	if err = json.Unmarshal(data, &input); err != nil {
		return err
	}
	if err = utils.TranslationsPreInit(); err != nil {
		return err
	}
	logger, err := mlog.NewLogger()
	if err != nil {
		return err
	}
	defer logger.Shutdown()
	mem, err := config.NewMemoryStoreWithOptions(&config.MemoryStoreOptions{IgnoreEnvironmentOverrides: true})
	if err != nil {
		return err
	}
	if err = mem.Set(input.Config); err != nil {
		return err
	}
	settings, err := config.NewStoreFromBacking(mem, nil, false)
	if err != nil {
		return err
	}
	defer settings.Close()
	app.RegisterAutoTranslationInterface(func(server *app.Server) einterfaces.AutoTranslationInterface {
		RegisterPostWritesBeforeStart(server)
		return New(server)
	})
	server, err := app.NewServer(app.ConfigStore(settings), app.SetLogger(logger), app.DisableGlobalLogger())
	if err != nil {
		return err
	}
	defer server.Shutdown()
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Features.AutoTranslation = new(true)
	if !server.SetLicense(license) {
		return fmt.Errorf("publication child rejected explicit test license")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rctx := request.EmptyContext(logger).WithContext(ctx)
	job, err := server.Store().Job().Get(rctx, input.JobID)
	if err != nil {
		return err
	}
	New(server).MakeWorker().(interface{ DoJob(*model.Job) }).DoJob(job)
	return fmt.Errorf("publication child finished before the injected crash")
}

// This PostgreSQL protocol relay stops only the publication SELECT, after the
// real UPDATE has committed and replied. No product hook or fake result is used.
type publicationSQLBarrier struct {
	listener net.Listener
	target   string
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	mu       sync.Mutex
	links    map[net.Conn]net.Conn
	wg       sync.WaitGroup
}

func newPublicationSQLBarrier(t *testing.T, raw string) (*publicationSQLBarrier, string) {
	t.Helper()
	dsn, err := url.Parse(raw)
	require.NoError(t, err)
	require.Equal(t, "disable", dsn.Query().Get("sslmode"), "fixture inspects only its own plaintext loopback test protocol")
	ip := net.ParseIP(dsn.Hostname())
	require.True(t, ip != nil && ip.IsLoopback())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	proxy := &publicationSQLBarrier{listener: listener, target: dsn.Host, entered: make(chan struct{}),
		release: make(chan struct{}), links: make(map[net.Conn]net.Conn)}
	proxy.wg.Add(1)
	go func() {
		defer proxy.wg.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			proxy.wg.Add(1)
			go proxy.relay(client)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		close(proxy.release)
		proxy.mu.Lock()
		for client, upstream := range proxy.links {
			_ = client.Close()
			_ = upstream.Close()
		}
		proxy.mu.Unlock()
		proxy.wg.Wait()
	})
	dsn.Host = listener.Addr().String()
	return proxy, dsn.String()
}

func (p *publicationSQLBarrier) relay(client net.Conn) {
	defer p.wg.Done()
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", p.target, 3*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	p.mu.Lock()
	p.links[client] = upstream
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.links, client); p.mu.Unlock() }()
	var startup [4]byte
	if _, err = io.ReadFull(client, startup[:]); err != nil {
		return
	}
	length := binary.BigEndian.Uint32(startup[:])
	if length < 8 || length > 1<<20 {
		return
	}
	body := make([]byte, length-4)
	if _, err = io.ReadFull(client, body); err != nil {
		return
	}
	if _, err = upstream.Write(append(startup[:], body...)); err != nil {
		return
	}
	responsesDone := make(chan struct{})
	go func() {
		defer close(responsesDone)
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
	}()
	defer func() { _ = upstream.Close(); <-responsesDone }()
	for {
		var header [5]byte
		if _, err = io.ReadFull(client, header[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint32(header[1:])
		if length < 4 || length > 16<<20 {
			return
		}
		body = make([]byte, length-4)
		if _, err = io.ReadFull(client, body); err != nil {
			return
		}
		if (header[0] == 'P' || header[0] == 'Q') && strings.Contains(string(body), "SELECT t.text,t.state,t.updateat") {
			p.once.Do(func() { close(p.entered) })
			<-p.release
		}
		if _, err = upstream.Write(append(header[:], body...)); err != nil {
			return
		}
	}
}

func TestRecoveryPublicationSurvivesPublisherSIGKILL(t *testing.T) {
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/detect" {
			_, _ = w.Write([]byte(`[{"language":"en","confidence":99}]`))
			return
		}
		_, _ = w.Write([]byte(`{"translatedText":"The durable recovered translation."}`))
	}))
	t.Cleanup(provider.Close)
	th := setupRecovery(t, provider.URL)
	post := seedRecoveryPost(t, th, "Synthetic source for a publisher process crash.", model.TranslationStateUnavailable)
	socket := th.CreateConnectedWebSocketClientWithClient(t, th.Client)
	proxy, proxiedDSN := newPublicationSQLBarrier(t, *th.App.Config().SqlSettings.DataSource)
	job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeAutoTranslationRecovery, nil)
	require.Nil(t, appErr)
	cfg := th.App.Config().Clone()
	cfg.SqlSettings.DataSource = new(proxiedDSN)
	cfg.SqlSettings.DataSourceReplicas = nil
	cfg.ServiceSettings.ListenAddress = new("127.0.0.1:0")
	cfg.ServiceSettings.EnableLocalMode = new(false)
	cfg.JobSettings.RunJobs = new(false)
	cfg.JobSettings.RunScheduler = new(false)
	cfg.PluginSettings.Enable = new(false)
	cfg.LogSettings.EnableFile = new(false)
	cfg.LogSettings.EnableConsole = new(false)
	cfg.LogSettings.EnableDiagnostics = new(false)
	cfg.MetricsSettings.Enable = new(false)
	dir := t.TempDir()
	path := filepath.Join(dir, "child.json")
	input, err := json.Marshal(publicationProcessInput{Config: cfg, JobID: job.Id})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, input, 0600))
	logPath := filepath.Join(dir, "child.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })
	cmd := exec.Command(os.Args[0])
	cmd.Stdout, cmd.Stderr = log, log
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "MM_") && !strings.HasPrefix(value, publicationProcessEnv+"=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, publicationProcessEnv+"="+path)
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			<-done
		}
	})
	select {
	case <-proxy.entered:
	case err = <-done:
		waited = true
		data, _ := os.ReadFile(logPath)
		t.Fatalf("publication child ended before the barrier: %v\n%s", err, data)
	case <-time.After(20 * time.Second):
		t.Fatal("child did not reach the publication SELECT after saving its result")
	}
	stored, err := th.Store.AutoTranslation().Get(model.TranslationObjectTypePost, post.Id, "ar")
	require.NoError(t, err)
	require.Equal(t, model.TranslationStateReady, stored.State, "the result must have committed before killing the publisher")
	require.Equal(t, "The durable recovered translation.", stored.Text)
	before := providerCalls.Load()
	require.EqualValues(t, 2, before)
	require.NoError(t, cmd.Process.Kill())
	err = <-done
	waited = true
	require.Error(t, err, "the first process must actually die")
	t.Logf("publisher killed at the SQL publication barrier: child_pid=%d surviving_server_pid=%d ready_result_persisted=true", cmd.Process.Pid, os.Getpid())
	// The dead publisher's durable lease must expire using PostgreSQL time.
	// Do not rewrite metadata, state or timestamps to manufacture recovery.
	require.Eventually(t, func() bool {
		var expired bool
		err := th.Store.GetInternalMasterDB().QueryRow(`SELECT
			(meta#>>'{workspace_publish_v1,lease_until}')::numeric <= floor(extract(epoch FROM clock_timestamp())*1000)
			FROM translations WHERE objectid=$1 AND dstlang='ar'`, post.Id).Scan(&expired)
		return err == nil && expired
	}, publicationLease+10*time.Second, 250*time.Millisecond)
	// A fresh service on the independent surviving server processes the next
	// genuine recovery Job; no row state is manually changed for recovery.
	runRecoveryWorker(t, th, New(th.Server))
	data := nextRecoveredTranslation(t, socket, post.Id)
	require.Equal(t, post.Message, data["source_text"])
	require.Equal(t, before, providerCalls.Load(), "a committed result must be republished without inference")
	encoded, err := json.Marshal(data["translations"])
	require.NoError(t, err)
	require.Contains(t, string(encoded), stored.Text)
	var pending bool
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT meta ? 'workspace_publish_v1'
		FROM translations WHERE objectid=$1 AND dstlang='ar'`, post.Id).Scan(&pending))
	require.False(t, pending, "the surviving dispatcher must acknowledge the result")
}
