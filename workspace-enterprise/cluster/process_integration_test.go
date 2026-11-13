package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/channels/wsapi"
	"github.com/mattermost/mattermost/server/v8/config"
	"github.com/stretchr/testify/require"
)

const clusterProcessConfigEnv = "WORKSPACE_CLUSTER_TEST_PROCESS_CONFIG"
const clusterPartitionProbeEvent model.ClusterEvent = "workspace_test_partition_probe"

type clusterPartitionProbe struct {
	Target string `json:"target"`
	ID     string `json:"id"`
}

type clusterProcessConfig struct {
	Config       *model.Config `json:"config"`
	ReadyPath    string        `json:"ready_path"`
	ControlToken string        `json:"control_token"`
	// Test-only connection privileges can isolate discovery failure from the
	// native application's healthy database connections.
	CoordinationDataSource string `json:"coordination_data_source,omitempty"`
	ExpectTransportFailure bool   `json:"expect_transport_failure,omitempty"`
	PersistedLicense       bool   `json:"persisted_license,omitempty"`
}

// Only the child test binary uses this exact-byte fixture validator. The
// native local cache purge reloads the license; a bare SetLicense fixture
// cannot survive that because it has no persisted or environment source.
// Other input still goes through the real validator. No product validator,
// signing key or license file is changed or created.
type clusterProcessLicenseValidator struct {
	utils.LicenseValidatorIface
	fixture []byte
}

func (validator clusterProcessLicenseValidator) ValidateLicense(data []byte) (string, error) {
	if bytes.Equal(data, validator.fixture) {
		return string(validator.fixture), nil
	}
	return validator.LicenseValidatorIface.ValidateLicense(data)
}

func (validator clusterProcessLicenseValidator) LicenseFromBytes(data []byte) (*model.License, *model.AppError) {
	if !bytes.Equal(data, validator.fixture) {
		return validator.LicenseValidatorIface.LicenseFromBytes(data)
	}
	var license model.License
	if err := json.Unmarshal(data, &license); err != nil {
		return nil, utils.NewLicenseValidationAppError("clusterProcessFixture", err)
	}
	return &license, nil
}

type clusterProcessStatus struct {
	ID                string `json:"id"`
	PID               int    `json:"pid"`
	Leader            bool   `json:"leader"`
	NotifiedLeader    bool   `json:"notified_leader"`
	Peers             int    `json:"peers"`
	URL               string `json:"url"`
	PrimaryUnchanged  bool   `json:"primary_unchanged"`
	ProbeCount        int64  `json:"probe_count"`
	LastProbe         string `json:"last_probe"`
	CachePeersMissing bool   `json:"cache_peers_missing"`
	TransportRunning  bool   `json:"transport_running"`
	LicenseID         string `json:"license_id"`
}

// This entry point exists only in the test binary. Each child has its own
// server, caches, listeners, goroutines and process lifetime, but shares the
// parent's isolated database and file directory. EOF requests a graceful stop;
// Process.Kill bypasses cleanup, leaving the SQL lease to expire naturally.
func runClusterProcess(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var input clusterProcessConfig
	if err = json.Unmarshal(data, &input); err != nil {
		return err
	}
	if input.PersistedLicense {
		// This opt-in fixture exercises the native SQL reload, including shutdown
		// while it is blocked. SIGUSR1 writes stacks without terminating the child.
		if err = os.Unsetenv("MM_LICENSE"); err != nil {
			return err
		}
		dumps := make(chan os.Signal, 1)
		signal.Notify(dumps, syscall.SIGUSR1)
		defer signal.Stop(dumps)
		stopDumps := make(chan struct{})
		defer close(stopDumps)
		go func() {
			for {
				select {
				case <-dumps:
					_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
				case <-stopDumps:
					return
				}
			}
		}()
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
	cs, err := config.NewStoreFromBacking(mem, nil, false)
	if err != nil {
		return err
	}
	defer cs.Close()
	server, err := app.NewServer(app.ConfigStore(cs), app.JoinCluster, app.SetLogger(logger), app.DisableGlobalLogger())
	if err != nil {
		return err
	}
	defer server.Shutdown()
	if _, err = api4.Init(server); err != nil {
		return err
	}
	wsapi.Init(server)
	service := server.Platform().Cluster().(*Service)
	if input.CoordinationDataSource != "" {
		service.databaseConfig.DataSource = &input.CoordinationDataSource
	}
	var probeCount atomic.Int64
	var lastProbe atomic.Value
	lastProbe.Store("")
	service.RegisterClusterMessageHandler(clusterPartitionProbeEvent, func(message *model.ClusterMessage) {
		lastProbe.Store(string(message.Data))
		probeCount.Add(1)
	})
	var notifiedLeader atomic.Bool
	server.AddClusterLeaderChangedListener(func() { notifiedLeader.Store(service.IsLeader()) })
	if service.GetMyClusterInfo() != nil {
		return fmt.Errorf("test node started cluster transport before a license was set")
	}
	license := model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)
	license.Id = model.NewId()
	fixture, err := json.Marshal(license)
	if err != nil {
		return err
	}
	utils.LicenseValidator = clusterProcessLicenseValidator{LicenseValidatorIface: utils.LicenseValidator, fixture: fixture}
	if input.PersistedLicense {
		if err = server.Store().License().Save(&model.LicenseRecord{Id: license.Id, Bytes: string(fixture)}); err != nil {
			return err
		}
		if err = server.Store().System().SaveOrUpdate(&model.System{Name: model.SystemActiveLicenseId, Value: license.Id}); err != nil {
			return err
		}
		server.Platform().LoadLicense()
		if loaded := server.Platform().License(); loaded == nil || loaded.Id != license.Id {
			return fmt.Errorf("test node did not load its SQL-persisted fixture license")
		}
	} else {
		if err = os.Setenv("MM_LICENSE", string(fixture)); err != nil {
			return err
		}
		if !server.SetLicense(license) {
			return fmt.Errorf("test node rejected fixture license")
		}
	}
	status := func() (clusterProcessStatus, error) {
		peers, err := service.GetClusterInfos()
		licenseID := ""
		if loaded := server.Platform().License(); loaded != nil {
			licenseID = loaded.Id
		}
		return clusterProcessStatus{ID: service.GetClusterId(), PID: os.Getpid(), Leader: service.IsLeader(), NotifiedLeader: notifiedLeader.Load(), Peers: len(peers), URL: fmt.Sprintf("http://127.0.0.1:%d", server.ListenAddr.Port), PrimaryUnchanged: model.SafeDereference(server.Config().SqlSettings.DataSource) == model.SafeDereference(input.Config.SqlSettings.DataSource), ProbeCount: probeCount.Load(), LastProbe: lastProbe.Load().(string), CachePeersMissing: service.hasMissingCachePeers(), TransportRunning: service.GetMyClusterInfo() != nil, LicenseID: licenseID}, err
	}
	server.Router.HandleFunc("/_workspace_cluster_test", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+input.ControlToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPut && input.PersistedLicense {
			// Enqueue onto the actual serialized receiver, never call the purge
			// directly from this HTTP handler or create a parallel receiver.
			service.lifecycle.Lock()
			service.mu.RLock()
			incoming, stop := service.incoming, service.stop
			service.mu.RUnlock()
			if worker := service.localCacheRecovery; worker != nil {
				incoming, stop = worker.incoming, worker.stop
			}
			service.lifecycle.Unlock()
			select {
			case incoming <- envelope{cacheRecoveryRequested: true}:
				w.WriteHeader(http.StatusAccepted)
			case <-stop:
				w.WriteHeader(http.StatusServiceUnavailable)
			case <-r.Context().Done():
			}
			return
		} else if r.Method == http.MethodDelete {
			server.SetLicense(nil)
		} else if r.Method == http.MethodPost {
			var probe clusterPartitionProbe
			if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&probe) != nil || !model.IsValidId(probe.Target) || !model.IsValidId(probe.ID) {
				http.Error(w, "invalid test probe", http.StatusBadRequest)
				return
			}
			if err := service.SendClusterMessageToNode(probe.Target, &model.ClusterMessage{Event: clusterPartitionProbeEvent, SendType: model.ClusterSendReliable, Data: []byte(probe.ID)}); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
		} else if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		value, err := status()
		if err != nil {
			http.Error(w, "test status unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	})
	if err = server.Start(); err != nil {
		return err
	}
	if input.ExpectTransportFailure && service.GetMyClusterInfo() != nil {
		return fmt.Errorf("test node unexpectedly started the failed transport")
	}
	if !input.ExpectTransportFailure && service.GetMyClusterInfo() == nil {
		return fmt.Errorf("licensed test node did not start cluster transport")
	}
	ready, err := status()
	if err != nil {
		return err
	}
	data, err = json.Marshal(ready)
	if err != nil {
		return err
	}
	if err = os.WriteFile(input.ReadyPath+".pending", data, 0600); err != nil {
		return err
	}
	if err = os.Rename(input.ReadyPath+".pending", input.ReadyPath); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, os.Stdin)
	return err
}

type clusterTestProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	done    chan struct{}
	err     error
	killed  bool
	url     string
	token   string
	id      string
	logPath string
}

func startClusterTestProcess(t *testing.T, cfg *model.Config) *clusterTestProcess {
	t.Helper()
	return startClusterTestProcessWithCoordination(t, cfg, "")
}

func startClusterTestProcessWithCoordination(t *testing.T, cfg *model.Config, coordinationDSN string) *clusterTestProcess {
	t.Helper()
	return startClusterTestProcessWithStartupExpectation(t, cfg, coordinationDSN, false)
}

func startClusterTestProcessWithStartupExpectation(t *testing.T, cfg *model.Config, coordinationDSN string, expectTransportFailure bool) *clusterTestProcess {
	t.Helper()
	return startClusterTestProcessWithConfig(t, clusterProcessConfig{Config: cfg, CoordinationDataSource: coordinationDSN, ExpectTransportFailure: expectTransportFailure})
}

func startClusterTestProcessWithConfig(t *testing.T, input clusterProcessConfig) *clusterTestProcess {
	t.Helper()
	cfg := input.Config
	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready.json")
	node := &clusterTestProcess{token: model.NewId(), done: make(chan struct{})}
	input.ReadyPath, input.ControlToken = readyPath, node.token
	data, err := json.Marshal(input)
	require.NoError(t, err)
	configPath := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(configPath, data, 0600))
	logPath := filepath.Join(dir, "node.log")
	if artifacts := os.Getenv("WORKSPACE_CLUSTER_TEST_ARTIFACTS"); artifacts != "" {
		require.NoError(t, os.MkdirAll(artifacts, 0700))
		logPath = filepath.Join(artifacts, fmt.Sprintf("node-%d-%s.log", *cfg.ClusterSettings.GossipPort, model.NewId()))
	}
	node.logPath = logPath
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = logFile.Close() })
	node.cmd = exec.Command(os.Args[0])
	node.cmd.Env = append(os.Environ(), clusterProcessConfigEnv+"="+configPath)
	node.cmd.Stdout, node.cmd.Stderr = logFile, logFile
	node.stdin, err = node.cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, node.cmd.Start())
	go func() {
		node.err = node.cmd.Wait()
		close(node.done)
	}()
	t.Cleanup(func() {
		_ = node.stdin.Close()
		select {
		case <-node.done:
			if !node.killed && node.err != nil {
				t.Errorf("test node failed during graceful stop: %v", node.err)
			}
		case <-time.After(15 * time.Second):
			_ = node.cmd.Process.Kill()
			<-node.done
			t.Error("test node did not stop gracefully")
		}
		if t.Failed() {
			log, _ := os.ReadFile(logPath)
			t.Logf("test process log %s:\n%s", logPath, log)
		}
	})
	var ready clusterProcessStatus
	require.Eventually(t, func() bool {
		select {
		case <-node.done:
			t.Logf("test node exited during startup: %v", node.err)
			return false
		default:
		}
		data, err := os.ReadFile(readyPath)
		return err == nil && json.Unmarshal(data, &ready) == nil
	}, 30*time.Second, 100*time.Millisecond)
	require.Equal(t, node.cmd.Process.Pid, ready.PID)
	require.NotEqual(t, os.Getpid(), ready.PID)
	node.url, node.id = ready.URL, ready.ID
	t.Logf("node ready: pid=%d id=%s gossip_port=%d", ready.PID, ready.ID, *cfg.ClusterSettings.GossipPort)
	return node
}

func (node *clusterTestProcess) status(method string) (clusterProcessStatus, error) {
	request, err := http.NewRequest(method, node.url+"/_workspace_cluster_test", nil)
	if err != nil {
		return clusterProcessStatus{}, err
	}
	request.Header.Set("Authorization", "Bearer "+node.token)
	client := &http.Client{Timeout: time.Second}
	response, err := client.Do(request)
	if err != nil {
		return clusterProcessStatus{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return clusterProcessStatus{}, fmt.Errorf("test node status: %d", response.StatusCode)
	}
	var result clusterProcessStatus
	err = json.NewDecoder(response.Body).Decode(&result)
	return result, err
}

func (node *clusterTestProcess) probe(target, id string) (int, string, error) {
	data, err := json.Marshal(clusterPartitionProbe{Target: target, ID: id})
	if err != nil {
		return 0, "", err
	}
	request, err := http.NewRequest(http.MethodPost, node.url+"/_workspace_cluster_test", bytes.NewReader(data))
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Authorization", "Bearer "+node.token)
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	return response.StatusCode, string(body), err
}

func TestSeparateProcessesTransportAndCrashFailover(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.EmailSettings.SendEmailNotifications = new(false)
	}).InitBasic(t)
	cfg := th.Server.Config().Clone()
	cfg.ClusterSettings.Enable = new(true)
	cfg.ClusterSettings.ClusterName = new(model.NewId())
	cfg.ClusterSettings.BindAddress = new("127.0.0.1")
	cfg.ClusterSettings.AdvertiseAddress = new("127.0.0.1")
	cfg.ClusterSettings.ReadOnlyConfig = new(false)
	cfg.ServiceSettings.ListenAddress = new("127.0.0.1:0")
	cfg.ServiceSettings.EnableLocalMode = new(false)
	cfg.PluginSettings.Enable = new(false)
	cfg.LogSettings.EnableFile = new(false)
	cfg1, cfg2 := cfg.Clone(), cfg.Clone()
	cfg1.ClusterSettings.GossipPort = new(freePort(t))
	cfg2.ClusterSettings.GossipPort = new(freePort(t))
	first, second := startClusterTestProcess(t, cfg1), startClusterTestProcess(t, cfg2)
	require.NotEqual(t, first.cmd.Process.Pid, second.cmd.Process.Pid)
	require.Eventually(t, func() bool {
		a, ea := first.status(http.MethodGet)
		b, eb := second.status(http.MethodGet)
		return ea == nil && eb == nil && a.Peers == 2 && b.Peers == 2 && a.Leader != b.Leader
	}, 15*time.Second, 100*time.Millisecond)
	clientFor := func(node *clusterTestProcess) *model.Client4 {
		client := model.NewAPIv4Client(node.url)
		client.SetToken(th.Client.AuthToken)
		client.HTTPClient.Timeout = 10 * time.Second
		return client
	}
	firstClient, secondClient := clientFor(first), clientFor(second)
	connect := func(node *clusterTestProcess) *model.WebSocketClient {
		ws, err := model.NewWebSocketClient4(strings.Replace(node.url, "http://", "ws://", 1), th.Client.AuthToken)
		require.NoError(t, err)
		ws.Listen()
		t.Cleanup(ws.Close)
		select {
		case response := <-ws.ResponseChannel:
			require.Equal(t, model.StatusOk, response.Status)
		case <-time.After(5 * time.Second):
			t.Fatal("test process websocket did not authenticate")
		}
		return ws
	}
	firstWS, secondWS := connect(first), connect(second)
	createAndReceive := func(sender *model.Client4, receiver *model.WebSocketClient, text string) *model.Post {
		t.Helper()
		post, _, err := sender.CreatePost(th.Context.Context(), &model.Post{ChannelId: th.BasicChannel.Id, Message: text})
		require.NoError(t, err)
		deadline := time.After(8 * time.Second)
		for {
			select {
			case event := <-receiver.EventChannel:
				require.NotNil(t, event, "websocket closed during delivery")
				if event.EventType() != model.WebsocketEventPosted {
					continue
				}
				var received model.Post
				if data, ok := event.GetData()["post"].(string); ok && json.Unmarshal([]byte(data), &received) == nil && received.Id == post.Id {
					require.Equal(t, text, received.Message)
					return post
				}
			case <-deadline:
				t.Fatal("post did not reach websocket across test processes")
			}
		}
	}
	post := createAndReceive(firstClient, secondWS, "First process to second")
	createAndReceive(secondClient, firstWS, "Second process to first")
	read, _, err := secondClient.GetPost(th.Context.Context(), post.Id, "")
	require.NoError(t, err)
	require.Equal(t, post.Message, read.Message)
	files, _, err := firstClient.UploadFile(th.Context.Context(), []byte("shared process file"), th.BasicChannel.Id, "process.txt")
	require.NoError(t, err)
	require.Len(t, files.FileInfos, 1)
	_, _, err = firstClient.CreatePost(th.Context.Context(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "Shared file", FileIds: []string{files.FileInfos[0].Id}})
	require.NoError(t, err)
	content, _, err := secondClient.GetFile(th.Context.Context(), files.FileInfos[0].Id)
	require.NoError(t, err)
	require.Equal(t, "shared process file", string(content))
	// Prime the second process's user cache, then invalidate it from the first.
	_, _, err = secondClient.GetUser(th.Context.Context(), th.BasicUser.Id, "")
	require.NoError(t, err)
	nickname := "process-" + model.NewId()
	_, _, err = firstClient.PatchUser(th.Context.Context(), th.BasicUser.Id, &model.UserPatch{Nickname: &nickname})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		user, _, err := secondClient.GetUser(th.Context.Context(), th.BasicUser.Id, "")
		return err == nil && user.Nickname == nickname
	}, 5*time.Second, 100*time.Millisecond)
	// Block the shared lease row. Both actual processes must independently
	// relinquish leadership when their SQL renewal reaches its deadline.
	lock, err := th.Store.GetInternalMasterDB().BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer lock.Rollback()
	_, err = lock.Exec(`SELECT OwnerId FROM WorkspaceClusterLeases WHERE ClusterName=$1 FOR UPDATE`, *cfg.ClusterSettings.ClusterName)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		a, ea := first.status(http.MethodGet)
		b, eb := second.status(http.MethodGet)
		return ea == nil && eb == nil && !a.Leader && !b.Leader
	}, 9*time.Second, 100*time.Millisecond)
	require.NoError(t, lock.Rollback())
	var leader, survivor *clusterTestProcess
	require.Eventually(t, func() bool {
		a, ea := first.status(http.MethodGet)
		b, eb := second.status(http.MethodGet)
		if ea != nil || eb != nil || a.Leader == b.Leader {
			return false
		}
		leader, survivor = first, second
		if b.Leader {
			leader, survivor = second, first
		}
		return true
	}, 12*time.Second, 100*time.Millisecond)
	var owner string
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT OwnerId FROM WorkspaceClusterLeases WHERE ClusterName=$1`, *cfg.ClusterSettings.ClusterName).Scan(&owner))
	require.Equal(t, leader.id, owner)
	start := time.Now()
	leader.killed = true
	require.NoError(t, leader.cmd.Process.Kill())
	select {
	case <-leader.done:
		require.Error(t, leader.err, "SIGKILL must not look like a graceful exit")
	case <-time.After(5 * time.Second):
		t.Fatal("killed test process was not reaped")
	}
	survivorClient, survivorWS := secondClient, secondWS
	restartConfig := cfg1
	if survivor == first {
		survivorClient, survivorWS, restartConfig = firstClient, firstWS, cfg2
	}
	createAndReceive(survivorClient, survivorWS, "Surviving process keeps its websocket")
	require.Eventually(t, func() bool {
		status, err := survivor.status(http.MethodGet)
		return err == nil && status.Leader
	}, 15*time.Second, 100*time.Millisecond)
	t.Logf("leader crash failover: previous_pid=%d survivor_pid=%d elapsed_ms=%d", leader.cmd.Process.Pid, survivor.cmd.Process.Pid, time.Since(start).Milliseconds())
	require.NoError(t, th.Store.GetInternalMasterDB().QueryRow(`SELECT OwnerId FROM WorkspaceClusterLeases WHERE ClusterName=$1`, *cfg.ClusterSettings.ClusterName).Scan(&owner))
	require.Equal(t, survivor.id, owner)
	restarted := startClusterTestProcess(t, restartConfig)
	require.NotEqual(t, leader.id, restarted.id)
	require.Eventually(t, func() bool {
		a, ea := survivor.status(http.MethodGet)
		b, eb := restarted.status(http.MethodGet)
		return ea == nil && eb == nil && a.Peers == 2 && b.Peers == 2 && a.Leader && !b.Leader
	}, 20*time.Second, 100*time.Millisecond)
	createAndReceive(clientFor(restarted), survivorWS, "Restarted process rejoins transport")
	// Revoking the current leader's license prevents further local leadership;
	// its peer must acquire the lease through the ordinary expiry path.
	status, err := survivor.status(http.MethodDelete)
	require.NoError(t, err)
	require.False(t, status.Leader)
	require.Eventually(t, func() bool {
		status, err := restarted.status(http.MethodGet)
		return err == nil && status.Leader
	}, 15*time.Second, 100*time.Millisecond)
	status, err = restarted.status(http.MethodDelete)
	require.NoError(t, err)
	require.False(t, status.Leader)
}
