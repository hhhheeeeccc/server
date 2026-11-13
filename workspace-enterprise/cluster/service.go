// Package cluster implements the server's native clustering interface. Membership
// and transport use memberlist; a PostgreSQL lease coordinates background jobs.
package cluster

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app/platform"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

const leaseDuration = 8 * time.Second
const heartbeatInterval = 2 * time.Second
const maxMessageBytes = 32 << 20

type envelope struct {
	From                   string                `json:"from"`
	ID                     string                `json:"id,omitempty"`
	Method                 string                `json:"method,omitempty"`
	Reply                  bool                  `json:"reply,omitempty"`
	Error                  string                `json:"error,omitempty"`
	Data                   json.RawMessage       `json:"data,omitempty"`
	Message                *model.ClusterMessage `json:"message,omitempty"`
	CacheVersion           uint64                `json:"cache_version,omitempty"`
	cacheState             *cacheState
	cacheRecoveryRequested bool
}

type Service struct {
	ps              *platform.PlatformService
	id              string
	lifecycle       sync.Mutex
	databaseMu      sync.Mutex
	database        *sql.DB
	databaseConfig  model.SqlSettings
	mu              sync.RWMutex
	list            *memberlist.Memberlist
	info            model.ClusterInfo
	clusterName     string
	stop            chan struct{}
	done            chan struct{}
	leaseUntil      time.Time
	leaderNotified  bool
	leaseTimer      *time.Timer
	leaseGeneration uint64
	runCtx          context.Context
	cancelRun       context.CancelFunc
	workers         sync.WaitGroup
	// Protected by lifecycle; independent of queues replaced by start retries.
	localCacheRecovery *localCacheRecovery
	rpcWorkers         sync.WaitGroup
	handlers           map[model.ClusterEvent]einterfaces.ClusterMessageHandler
	pending            map[string]chan envelope
	incoming           chan envelope
	outgoing           chan envelope
	cacheRecovery      chan struct{}
	// Keep missing-writer knowledge across transport restarts: application
	// caches outlive the transport, and discovery can find an unseen writer.
	cacheMissingPeers        map[string]struct{}
	cacheMissingPeerOverflow bool
	cacheMembers             map[string]cacheMember
	cacheVersions            cacheVersions
	rpcSlots                 chan struct{}
}

var _ einterfaces.ClusterInterface = (*Service)(nil)

func New(ps *platform.PlatformService) einterfaces.ClusterInterface {
	return &Service{ps: ps, id: model.NewId(), databaseConfig: ps.Config().Clone().SqlSettings, handlers: make(map[model.ClusterEvent]einterfaces.ClusterMessageHandler), pending: make(map[string]chan envelope), rpcSlots: make(chan struct{}, 16)}
}

func (s *Service) enabled() bool {
	l := s.ps.License()
	return l != nil && l.Features != nil && model.SafeDereference(l.Features.Cluster) && model.SafeDereference(s.ps.Config().ClusterSettings.Enable)
}

func (s *Service) StartInterNodeCommunication() {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if !s.enabled() {
		return
	}
	s.mu.RLock()
	running := s.list != nil
	s.mu.RUnlock()
	if running {
		return
	}
	if err := s.start(); err != nil {
		s.closeCoordinationDatabase()
		s.ps.Log().Error("Unable to start Enterprise cluster", mlog.Err(err))
		s.startLocalCacheRecovery()
	}
}

func (s *Service) start() error {
	cfg := s.ps.Config().ClusterSettings
	name := model.SafeDereference(cfg.ClusterName)
	if name == "" {
		return errors.New("ClusterSettings.ClusterName is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := s.coordinationDatabase()
	if err != nil {
		return err
	}
	if err := prepareSchema(ctx, db); err != nil {
		return err
	}
	mlcfg := memberlist.DefaultLANConfig()
	mlcfg.Name = s.id
	mlcfg.BindPort = *cfg.GossipPort
	if *cfg.BindAddress != "" {
		mlcfg.BindAddr = *cfg.BindAddress
	}
	if *cfg.AdvertiseAddress != "" {
		mlcfg.AdvertiseAddr = *cfg.AdvertiseAddress
	}
	mlcfg.AdvertisePort = *cfg.GossipPort
	mlcfg.EnableCompression = *cfg.EnableGossipCompression
	mlcfg.TCPTimeout = 3 * time.Second
	label := sha256.Sum256([]byte(name))
	mlcfg.Label = "workspace-" + hex.EncodeToString(label[:8])
	mlcfg.LogOutput = io.Discard // Member errors are reported through the server logger below.
	mlcfg.Delegate = s
	mlcfg.Events = s
	if *cfg.EnableGossipEncryption {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO Systems (Name, Value) VALUES ('ClusterEncryptionKey', $1) ON CONFLICT (Name) DO NOTHING`, base64.StdEncoding.EncodeToString(key)); err != nil {
			return err
		}
		var encoded string
		if err := db.QueryRowContext(ctx, `SELECT Value FROM Systems WHERE Name='ClusterEncryptionKey'`).Scan(&encoded); err != nil {
			return err
		}
		var err error
		mlcfg.SecretKey, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil || (len(mlcfg.SecretKey) != 16 && len(mlcfg.SecretKey) != 24 && len(mlcfg.SecretKey) != 32) {
			return errors.New("invalid ClusterEncryptionKey")
		}
	}
	host := model.SafeDereference(cfg.OverrideHostname)
	if host == "" && *cfg.UseIPAddress {
		host = model.GetServerIPAddress(*cfg.NetworkInterface)
		if *cfg.AdvertiseAddress != "" {
			host = *cfg.AdvertiseAddress
		}
	}
	if host == "" {
		host, _ = os.Hostname()
	}
	var schema int
	if err := db.QueryRowContext(ctx, `SELECT Version FROM db_migrations ORDER BY Version DESC LIMIT 1`).Scan(&schema); err != nil {
		return err
	}
	s.mu.Lock()
	s.clusterName = name
	s.info = model.ClusterInfo{Id: s.id, Version: model.CurrentVersion, SchemaVersion: strconv.Itoa(schema), ConfigHash: s.ps.ClientConfigHash(), Hostname: host, IPAddress: host}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	s.runCtx, s.cancelRun = context.WithCancel(s.ps.GoContext())
	s.incoming = make(chan envelope, 4096)
	s.outgoing = make(chan envelope, 4096)
	s.cacheRecovery = make(chan struct{}, 1)
	s.cacheMembers = make(map[string]cacheMember)
	s.mu.Unlock()
	if len(s.NodeMeta(memberlist.MetaMaxSize)) == 0 {
		s.cancelRun()
		return errors.New("cluster hostname or metadata exceeds gossip limit")
	}
	ml, err := memberlist.Create(mlcfg)
	if err != nil {
		s.cancelRun()
		return err
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO ClusterDiscovery (Id,Type,ClusterName,Hostname,GossipPort,Port,CreateAt,LastPingAt) VALUES ($1,$2,$3,$4,$5,0,$6,$6)`, s.id, model.CDSTypeApp, name, host, *cfg.GossipPort, model.GetMillis()); err != nil {
		s.cancelRun()
		_ = ml.Shutdown()
		return err
	}
	// Complete an in-flight local purge before publishing the transport or
	// starting its receiver. The native cache handlers must stay serialized.
	s.stopLocalCacheRecovery()
	s.mu.Lock()
	s.list = ml
	s.mu.Unlock()
	s.workers.Add(4)
	go func() { defer s.workers.Done(); s.receiveLoop(s.stop, s.incoming) }()
	go func() { defer s.workers.Done(); s.sendLoop(s.stop, s.outgoing) }()
	go func() { defer s.workers.Done(); s.discoveryLoop(s.runCtx, s.stop, ml) }()
	go func() { defer s.workers.Done(); s.cacheRecoveryLoop(s.stop, s.cacheRecovery, s.incoming) }()
	s.refresh()
	go s.heartbeat(s.stop, s.done)
	s.ps.Log().Info("Enterprise cluster transport started", mlog.String("node_id", s.id), mlog.String("cluster_name", name))
	return nil
}

func prepareSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(6492261402)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS WorkspaceClusterLeases (ClusterName text PRIMARY KEY, OwnerId varchar(26) NOT NULL, ExpiresAt timestamptz NOT NULL)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) heartbeat(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.refresh()
		}
	}
}

func (s *Service) refresh() {
	s.mu.RLock()
	ml, name, parent := s.list, s.clusterName, s.runCtx
	s.mu.RUnlock()
	if ml == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, heartbeatInterval)
	defer cancel()
	db, err := s.coordinationDatabase()
	if err == nil {
		_, err = db.ExecContext(ctx, `UPDATE ClusterDiscovery SET LastPingAt=$1 WHERE Id=$2`, model.GetMillis(), s.id)
	}
	if err != nil {
		s.setLease(time.Time{})
		s.ps.Log().Warn("Cluster heartbeat failed", mlog.Err(err))
		return
	}
	if !s.enabled() {
		s.setLease(time.Time{})
		return
	}
	started := time.Now()
	var owner string
	err = db.QueryRowContext(ctx, `INSERT INTO WorkspaceClusterLeases (ClusterName,OwnerId,ExpiresAt) VALUES ($1,$2,clock_timestamp()+interval '8 seconds') ON CONFLICT (ClusterName) DO UPDATE SET OwnerId=EXCLUDED.OwnerId,ExpiresAt=EXCLUDED.ExpiresAt WHERE WorkspaceClusterLeases.OwnerId=$2 OR WorkspaceClusterLeases.ExpiresAt<clock_timestamp() RETURNING OwnerId`, name, s.id).Scan(&owner)
	if err == nil && owner == s.id && ctx.Err() == nil && s.currentCoordinationDatabase(db) {
		s.setLease(started.Add(leaseDuration - heartbeatInterval))
	} else {
		s.setLease(time.Time{})
		if !errors.Is(err, sql.ErrNoRows) {
			s.ps.Log().Warn("Cluster leader lease failed", mlog.Err(err))
		}
	}
	// Peer dialing runs in its own worker. An unreachable peer must never delay
	// renewal of the database lease and spuriously stop the leader's jobs.
}

func (s *Service) discoveryLoop(parent context.Context, stop <-chan struct{}, ml *memberlist.Memberlist) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		if s.enabled() {
			s.discover(parent, stop, ml)
		}
		select {
		case <-stop:
			return
		case <-parent.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) discover(parent context.Context, stop <-chan struct{}, ml *memberlist.Memberlist) {
	ctx, cancel := context.WithTimeout(parent, heartbeatInterval)
	defer cancel()
	s.mu.RLock()
	name := s.clusterName
	s.mu.RUnlock()
	db, err := s.coordinationDatabase()
	if err != nil {
		return
	}
	rows, err := db.QueryContext(ctx, `SELECT Hostname,GossipPort FROM ClusterDiscovery WHERE Type=$1 AND ClusterName=$2 AND Id<>$3 AND LastPingAt>$4 ORDER BY LastPingAt DESC LIMIT 257`, model.CDSTypeApp, name, s.id, model.GetMillis()-60000)
	if err != nil {
		return
	}
	var addresses []string
	for rows.Next() {
		var host string
		var port int
		if rows.Scan(&host, &port) == nil {
			addresses = append(addresses, net.JoinHostPort(host, strconv.Itoa(port)))
		}
	}
	readErr := rows.Err()
	_ = rows.Close()
	if readErr != nil {
		return
	}
	known := s.observeDiscoveredCachePeers(addresses)
	for _, address := range addresses {
		select {
		case <-stop:
			return
		case <-parent.Done():
			return
		default:
		}
		if known[address] {
			continue
		}
		if _, err = ml.Join([]string{address}); err != nil {
			s.ps.Log().Debug("Cluster peer discovery retry", mlog.Err(err))
		}
	}
}

func (s *Service) setLease(until time.Time) {
	s.mu.Lock()
	was := s.leaderNotified
	if s.leaseTimer != nil {
		s.leaseTimer.Stop()
		s.leaseTimer = nil
	}
	s.leaseGeneration++
	s.leaseUntil = until
	now := time.Now().Before(until)
	s.leaderNotified = now
	if now {
		generation := s.leaseGeneration
		s.leaseTimer = time.AfterFunc(time.Until(until), func() { s.expireLease(generation) })
	}
	s.mu.Unlock()
	if was != now {
		s.ps.InvokeClusterLeaderChangedListeners()
	}
}

// Database cancellation can itself stall when the connection is silent.
// Schedulers subscribe to leader changes rather than polling IsLeader(), so
// revocation must not depend on the blocked heartbeat returning. A generation
// guard prevents an expired timer from revoking a subsequently renewed lease.
func (s *Service) expireLease(generation uint64) {
	s.mu.Lock()
	if generation != s.leaseGeneration || !s.leaderNotified || time.Now().Before(s.leaseUntil) {
		s.mu.Unlock()
		return
	}
	s.leaseUntil = time.Time{}
	s.leaderNotified = false
	s.leaseTimer = nil
	s.mu.Unlock()
	s.ps.InvokeClusterLeaderChangedListeners()
}

func (s *Service) StopInterNodeCommunication() {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	defer s.closeCoordinationDatabase()
	s.stopLocalCacheRecovery()
	s.mu.Lock()
	ml := s.list
	if ml == nil {
		s.mu.Unlock()
		return
	}
	close(s.stop)
	s.cancelRun()
	done := s.done
	s.list = nil
	name := s.clusterName
	s.mu.Unlock()
	s.setLease(time.Time{})
	_ = ml.Leave(time.Second)
	_ = ml.Shutdown()
	<-done
	s.workers.Wait()
	s.rpcWorkers.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s.databaseMu.Lock()
	db := s.database
	s.databaseMu.Unlock()
	if db != nil {
		_, _ = db.ExecContext(ctx, `DELETE FROM WorkspaceClusterLeases WHERE ClusterName=$1 AND OwnerId=$2`, name, s.id)
		_, _ = db.ExecContext(ctx, `DELETE FROM ClusterDiscovery WHERE Id=$1`, s.id)
	}
}

func (s *Service) Shutdown()            { s.StopInterNodeCommunication() }
func (s *Service) GetClusterId() string { return s.id }
func (s *Service) IsLeader() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled() && s.list != nil && time.Now().Before(s.leaseUntil)
}
func (s *Service) HealthScore() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.list == nil {
		return 1
	}
	return s.list.GetHealthScore()
}
func (s *Service) GetMyClusterInfo() *model.ClusterInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.list == nil {
		return nil
	}
	info := s.info
	return &info
}
func (s *Service) GetClusterInfos() ([]*model.ClusterInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*model.ClusterInfo{}
	if s.list == nil {
		return out, nil
	}
	for _, member := range s.cacheMembers {
		var info model.ClusterInfo
		if err := json.Unmarshal([]byte(member.metadata), &info); err != nil {
			return nil, err
		}
		out = append(out, &info)
	}
	return out, nil
}
func (s *Service) NodeMeta(limit int) []byte {
	s.mu.RLock()
	info := s.info
	s.mu.RUnlock()
	b, _ := json.Marshal(info)
	if len(b) > limit {
		return nil
	}
	return b
}
func (*Service) GetBroadcasts(int, int) [][]byte { return nil }
func (s *Service) RegisterClusterMessageHandler(event model.ClusterEvent, h einterfaces.ClusterMessageHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[event] = h
}

func (s *Service) peers() (*memberlist.Memberlist, []*memberlist.Node) {
	s.mu.RLock()
	ml := s.list
	s.mu.RUnlock()
	if ml == nil {
		return nil, nil
	}
	var peers []*memberlist.Node
	for _, n := range ml.Members() {
		if n.Name != s.id {
			peers = append(peers, n)
		}
	}
	return ml, peers
}
func (s *Service) send(nodeID string, e envelope) error {
	if !s.enabled() {
		return errors.New("cluster is disabled")
	}
	ml, peers := s.peers()
	if ml == nil {
		return errors.New("cluster transport is stopped")
	}
	e.From = s.id
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b) > maxMessageBytes {
		return errors.New("cluster message exceeds limit")
	}
	for _, n := range peers {
		if n.Name == nodeID {
			return ml.SendReliable(n, b)
		}
	}
	return fmt.Errorf("cluster node %s unavailable", nodeID)
}
func (s *Service) SendClusterMessageToNode(nodeID string, msg *model.ClusterMessage) error {
	if msg == nil {
		return errors.New("missing cluster message")
	}
	return s.sendClusterEnvelope(nodeID, s.cacheEnvelope(msg))
}
func (s *Service) sendClusterEnvelope(nodeID string, message envelope) error {
	if message.Message.SendType == model.ClusterSendReliable {
		ctx, cancel := context.WithTimeout(s.ps.GoContext(), 10*time.Second)
		defer cancel()
		return s.exchange(ctx, nodeID, message, nil)
	}
	return s.send(nodeID, message)
}
func (s *Service) broadcast(message envelope) {
	_, peers := s.peers()
	for _, p := range peers {
		if err := s.sendClusterEnvelope(p.Name, message); err != nil {
			s.ps.Log().Warn("Cluster message delivery failed", mlog.String("event", string(message.Message.Event)), mlog.String("node_id", p.Name), mlog.Err(err))
		}
	}
}
func (s *Service) SendClusterMessage(msg *model.ClusterMessage) {
	if msg == nil || !s.enabled() {
		return
	}
	s.mu.RLock()
	queue, stop, running := s.outgoing, s.stop, s.list != nil
	s.mu.RUnlock()
	if !running {
		return
	}
	if msg.WaitForAllToSend {
		s.broadcast(s.cacheEnvelope(msg))
		return
	}
	// Snapshot caller-owned buffers before asynchronous delivery.
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	var snapshot model.ClusterMessage
	if json.Unmarshal(b, &snapshot) != nil {
		return
	}
	snapshot.SendType = msg.SendType
	message := s.cacheEnvelope(&snapshot)
	if message.CacheVersion != 0 && snapshot.SendType == model.ClusterSendBestEffort {
		// Native cache invalidation is synchronous with application requests
		// and workers. A full transport queue must not strand those callers.
		// The version was recorded before enqueueing, so push/pull state sync
		// (or a later version gap) can purge the affected cache on each peer.
		select {
		case queue <- message:
		case <-stop:
		default:
		}
		return
	}
	select {
	case queue <- message:
	case <-stop:
	}
}
func (s *Service) sendLoop(stop <-chan struct{}, q <-chan envelope) {
	for {
		select {
		case <-stop:
			return
		case msg := <-q:
			s.broadcast(msg)
		}
	}
}
func (s *Service) NotifyMsg(buf []byte) {
	if len(buf) > maxMessageBytes || !s.enabled() {
		return
	}
	var e envelope
	if json.Unmarshal(buf, &e) != nil {
		return
	}
	s.mu.RLock()
	q, stop := s.incoming, s.stop
	pending := s.pending[e.ID]
	s.mu.RUnlock()
	if e.Reply {
		if pending != nil {
			select {
			case pending <- e:
			default:
			}
		}
		return
	}
	if q == nil {
		return
	}
	select {
	case q <- e:
	case <-stop:
	default:
		s.ps.Log().Error("Cluster receive queue is full")
	}
}
func (s *Service) receiveLoop(stop <-chan struct{}, q <-chan envelope) {
	for {
		select {
		case <-stop:
			return
		case e := <-q:
			if !s.enabled() {
				continue
			}
			if e.cacheRecoveryRequested {
				s.recoverMembershipCaches()
				continue
			}
			if e.cacheState != nil {
				s.mergeCacheState(*e.cacheState)
				continue
			}
			if e.Method != "" {
				select {
				case s.rpcSlots <- struct{}{}:
					s.rpcWorkers.Add(1)
					go func() { defer s.rpcWorkers.Done(); defer func() { <-s.rpcSlots }(); s.handleRPC(e) }()
				default:
					_ = s.send(e.From, envelope{ID: e.ID, Reply: true, Error: "cluster RPC capacity exceeded"})
				}
				continue
			}
			if e.Message != nil {
				s.mu.RLock()
				h := s.handlers[e.Message.Event]
				s.mu.RUnlock()
				handled := h != nil && s.applyCacheEnvelope(e, h)
				if e.ID != "" {
					response := envelope{ID: e.ID, Reply: true}
					if h == nil {
						response.Error = "unregistered cluster event"
					} else if !handled {
						response.Error = "cluster cache recovery handler unavailable"
					}
					_ = s.send(e.From, response)
				}
			}
		}
	}
}
