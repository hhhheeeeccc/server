package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

type rpcArgs struct {
	Page         int                         `json:"page"`
	PerPage      int                         `json:"per_page"`
	UserID       string                      `json:"user_id"`
	ConnectionID string                      `json:"connection_id"`
	Seq          int64                       `json:"seq"`
	Options      *model.SupportPacketOptions `json:"options,omitempty"`
	Config       *model.Config               `json:"config,omitempty"`
}

func clusterError(err error) *model.AppError {
	if err == nil {
		return nil
	}
	return model.NewAppError("EnterpriseCluster", "ent.cluster.request.app_error", nil, "Cluster request failed", http.StatusServiceUnavailable).Wrap(err)
}

func (s *Service) call(ctx context.Context, node, method string, args rpcArgs, out any) error {
	data, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return s.exchange(ctx, node, envelope{Method: method, Data: data}, out)
}

func (s *Service) exchange(ctx context.Context, node string, message envelope, out any) error {
	id := model.NewId()
	ch := make(chan envelope, 1)
	s.mu.Lock()
	s.pending[id] = ch
	stop := s.stop
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
	message.ID = id
	if err := s.send(node, message); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-stop:
		return errors.New("cluster stopped")
	case result := <-ch:
		if result.From != node {
			return errors.New("cluster response sender mismatch")
		}
		if result.Error != "" {
			return fmt.Errorf("node %s: %s", node, result.Error)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(result.Data, out)
	}
}

func (s *Service) handleRPC(e envelope) {
	var args rpcArgs
	var result any
	var err error
	if json.Unmarshal(e.Data, &args) != nil {
		err = errors.New("invalid cluster request")
	} else {
		s.mu.RLock()
		parent := s.runCtx
		s.mu.RUnlock()
		ctx, cancel := context.WithTimeout(parent, 60*time.Second)
		defer cancel()
		rctx := request.EmptyContext(s.ps.Log()).WithContext(ctx)
		switch e.Method {
		case "stats":
			result = &model.ClusterStats{Id: s.id, TotalWebsocketConnections: s.ps.TotalWebsocketConnections(), TotalReadDbConnections: s.ps.Store.TotalReadDbConnections(), TotalMasterDbConnections: s.ps.Store.TotalMasterDbConnections()}
		case "logs":
			if args.Page < 0 || args.PerPage < 1 || args.PerPage > 10000 {
				err = errors.New("invalid log pagination")
				break
			}
			var appErr *model.AppError
			result, appErr = s.ps.GetLogsSkipSend(rctx, args.Page, args.PerPage, nil)
			if appErr != nil {
				err = appErr
			}
		case "support":
			result, err = s.ps.GenerateSupportPacket(rctx, args.Options)
		case "plugins":
			var appErr *model.AppError
			result, appErr = s.ps.GetPluginStatuses()
			if appErr != nil {
				err = appErr
			}
		case "webconn":
			if !model.IsValidId(args.UserID) {
				err = errors.New("invalid user")
				break
			}
			result = s.ps.WebConnCountForUser(args.UserID)
		case "queues":
			if !model.IsValidId(args.UserID) {
				err = errors.New("invalid user")
				break
			}
			result, err = s.ps.GetWSQueues(args.UserID, args.ConnectionID, args.Seq)
		case "config":
			if args.Config == nil {
				err = errors.New("missing config")
				break
			}
			// Preserve each node's bind/advertise addresses and transport port.
			args.Config.ClusterSettings = s.ps.Config().Clone().ClusterSettings
			var appErr *model.AppError
			_, _, appErr = s.ps.SaveConfig(args.Config, false)
			if appErr != nil {
				err = appErr
			}
		default:
			err = errors.New("unknown cluster RPC")
		}
	}
	resp := envelope{ID: e.ID, Reply: true}
	if err == nil {
		resp.Data, err = json.Marshal(result)
	}
	if err != nil {
		resp.Error = err.Error()
	}
	if sendErr := s.send(e.From, resp); sendErr != nil && len(resp.Data) > 0 {
		// Large support packets must fail visibly rather than report empty success.
		_ = s.send(e.From, envelope{ID: e.ID, Reply: true, Error: "unable to transfer cluster response"})
	}
}

func queryPeers[T any](s *Service, ctx context.Context, method string, args rpcArgs) (map[string]T, error) {
	out := make(map[string]T)
	if !s.enabled() {
		return out, nil
	}
	ml, peers := s.peers()
	if ml == nil {
		return nil, errors.New("cluster is enabled but transport is unavailable")
	}
	for _, node := range peers {
		var v T
		if err := s.call(ctx, node.Name, method, args, &v); err != nil {
			return nil, err
		}
		out[node.Name] = v
	}
	return out, nil
}

func (s *Service) GetClusterStats(rctx request.CTX) ([]*model.ClusterStats, *model.AppError) {
	ctx, cancel := context.WithTimeout(rctx.Context(), 10*time.Second)
	defer cancel()
	stats, err := queryPeers[*model.ClusterStats](s, ctx, "stats", rpcArgs{})
	if err != nil {
		return nil, clusterError(err)
	}
	out := make([]*model.ClusterStats, 0, len(stats))
	for _, v := range stats {
		out = append(out, v)
	}
	return out, nil
}
func (s *Service) QueryLogs(rctx request.CTX, page, perPage int) (map[string][]string, *model.AppError) {
	ctx, cancel := context.WithTimeout(rctx.Context(), 15*time.Second)
	defer cancel()
	out, err := queryPeers[[]string](s, ctx, "logs", rpcArgs{Page: page, PerPage: perPage})
	return out, clusterError(err)
}
func (s *Service) GetLogs(rctx request.CTX, page, perPage int) ([]string, *model.AppError) {
	logs, err := s.QueryLogs(rctx, page, perPage)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, v := range logs {
		out = append(out, v...)
	}
	return out, nil
}
func (s *Service) GenerateSupportPacket(rctx request.CTX, opts *model.SupportPacketOptions) (map[string][]model.FileData, error) {
	ctx, cancel := context.WithTimeout(rctx.Context(), 90*time.Second)
	defer cancel()
	return queryPeers[[]model.FileData](s, ctx, "support", rpcArgs{Options: opts})
}
func (s *Service) GetPluginStatuses() (model.PluginStatuses, *model.AppError) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stats, err := queryPeers[model.PluginStatuses](s, ctx, "plugins", rpcArgs{})
	if err != nil {
		return nil, clusterError(err)
	}
	out := model.PluginStatuses{}
	for _, v := range stats {
		out = append(out, v...)
	}
	return out, nil
}
func (s *Service) ConfigChanged(_, newConfig *model.Config, send bool) *model.AppError {
	if !s.enabled() {
		return nil
	}
	s.mu.RLock()
	running := s.list != nil
	s.mu.RUnlock()
	// Startup migrations can save the config before the cluster listener starts.
	// There are no reachable peers to notify until transport is running.
	if send && running {
		// Native leader-change listeners can synchronously save configuration.
		// A silent peer must not hold up the remaining leadership listeners for
		// longer than the coordination interval. Report the failed propagation.
		ctx, cancel := context.WithTimeout(s.ps.GoContext(), heartbeatInterval)
		defer cancel()
		if _, err := queryPeers[any](s, ctx, "config", rpcArgs{Config: newConfig}); err != nil {
			return clusterError(err)
		}
	}
	s.mu.Lock()
	s.info.ConfigHash = s.ps.ClientConfigHash()
	ml := s.list
	s.mu.Unlock()
	if ml != nil {
		_ = ml.UpdateNode(time.Second)
	}
	return nil
}
func (s *Service) WebConnCountForUser(userID string) (int, *model.AppError) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	counts, err := queryPeers[int](s, ctx, "webconn", rpcArgs{UserID: userID})
	if err != nil {
		return 0, clusterError(err)
	}
	total := 0
	for _, v := range counts {
		total += v
	}
	return total, nil
}
func (s *Service) GetWSQueues(userID, connectionID string, seqNum int64) (map[string]*model.WSQueues, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return queryPeers[*model.WSQueues](s, ctx, "queues", rpcArgs{UserID: userID, ConnectionID: connectionID, Seq: seqNum})
}
