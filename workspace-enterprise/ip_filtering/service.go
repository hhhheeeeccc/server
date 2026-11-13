package ip_filtering

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

const policyTimeout = 5 * time.Second

type Service struct {
	app     *app.App
	db      *sql.DB
	trusted []netip.Prefix
	header  string
	initErr error
}

var _ einterfaces.IPFilteringInterface = (*Service)(nil)

func New(a *app.App) einterfaces.IPFilteringInterface {
	s := &Service{app: a, db: a.Srv().Store().GetInternalMasterDB()}
	s.trusted, s.header, s.initErr = trustedProxyConfig()
	if s.initErr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), policyTimeout)
		s.initErr = s.initPolicy(ctx)
		cancel()
	}
	if s.initErr != nil {
		a.Log().Error("Unable to initialize IP filtering", mlog.Err(s.initErr))
	}
	a.Srv().Router.Use(s.enforce)
	return s
}

func (s *Service) enabled() bool {
	license := s.app.License()
	return license != nil && license.IsCloud() && model.MinimumEnterpriseLicense(license)
}

func (s *Service) initPolicy(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(6492261407)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS WorkspaceIPFilters (
		Id integer PRIMARY KEY CHECK (Id=1), Policy jsonb NOT NULL, Version bigint NOT NULL DEFAULT 0)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `ALTER TABLE WorkspaceIPFilters ADD COLUMN IF NOT EXISTS Version bigint NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO WorkspaceIPFilters (Id,Policy) VALUES (1,'[]'::jsonb) ON CONFLICT (Id) DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) readPolicy(ctx context.Context) (*model.AllowedIPRanges, error) {
	if s.initErr != nil {
		return nil, s.initErr
	}
	var raw []byte
	if err := s.db.QueryRowContext(ctx, `SELECT Policy FROM WorkspaceIPFilters WHERE Id=1`).Scan(&raw); err != nil {
		return nil, err
	}
	var policy model.AllowedIPRanges
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, err
	}
	if policy == nil {
		return nil, errors.New("IP filter policy is null")
	}
	// The policy can be changed outside this service (for example during a
	// restore). Never treat an invalid stored range as an empty allow-all list.
	return normalizePolicy(&policy)
}

func (s *Service) GetIPFilters() (*model.AllowedIPRanges, error) {
	if !s.enabled() {
		return nil, errors.New("Cloud Enterprise IP filtering is not licensed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), policyTimeout)
	defer cancel()
	return s.readPolicy(ctx)
}

func (s *Service) ApplyIPFilters(requested *model.AllowedIPRanges) (*model.AllowedIPRanges, error) {
	if !s.enabled() {
		return nil, errors.New("Cloud Enterprise IP filtering is not licensed")
	}
	if s.initErr != nil {
		return nil, s.initErr
	}
	policy, err := normalizePolicy(requested)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), policyTimeout)
	defer cancel()
	result, err := s.db.ExecContext(ctx, `UPDATE WorkspaceIPFilters SET Policy=$1, Version=Version+1 WHERE Id=1`, string(raw))
	if err != nil {
		return nil, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return nil, errors.New("IP filter policy row is missing")
	}
	return policy, nil
}

func normalizePolicy(requested *model.AllowedIPRanges) (*model.AllowedIPRanges, error) {
	if requested == nil {
		return nil, errors.New("missing IP ranges")
	}
	seen := make(map[string]bool, len(*requested))
	policy := make(model.AllowedIPRanges, 0, len(*requested))
	for _, item := range *requested {
		prefix, err := canonicalPrefix(item.CIDRBlock)
		if err != nil {
			return nil, err
		}
		item.CIDRBlock = prefix.String()
		if seen[item.CIDRBlock] {
			return nil, errors.New("duplicate IP range: " + item.CIDRBlock)
		}
		seen[item.CIDRBlock] = true
		policy = append(policy, item)
	}
	sort.Slice(policy, func(i, j int) bool { return policy[i].CIDRBlock < policy[j].CIDRBlock })
	return &policy, nil
}

func canonicalPrefix(value string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
	if err != nil {
		return netip.Prefix{}, err
	}
	if prefix.Addr().Is4In6() {
		if prefix.Bits() < 96 {
			return netip.Prefix{}, errors.New("IPv4-mapped CIDR prefix is broader than IPv4 address space")
		}
		prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
	}
	return prefix.Masked(), nil
}

func trustedProxyConfig() ([]netip.Prefix, string, error) {
	raw := strings.TrimSpace(os.Getenv("WORKSPACE_IP_FILTER_TRUSTED_PROXIES"))
	header := strings.TrimSpace(os.Getenv("WORKSPACE_IP_FILTER_CLIENT_HEADER"))
	if raw == "" {
		if header != "" {
			return nil, "", errors.New("client IP header requires trusted proxy ranges")
		}
		return nil, "", nil
	}
	if header == "" || strings.ContainsAny(header, "\r\n:") {
		return nil, "", errors.New("trusted proxies require one valid client IP header")
	}
	var trusted []netip.Prefix
	for _, value := range strings.Split(raw, ",") {
		prefix, err := canonicalPrefix(value)
		if err != nil {
			return nil, "", err
		}
		trusted = append(trusted, prefix)
	}
	return trusted, header, nil
}
