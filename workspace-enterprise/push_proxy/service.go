package push_proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	ejobs "github.com/mattermost/mattermost/server/v8/einterfaces/jobs"
)

const storeTimeout = 10 * time.Second

type Service struct {
	app       *app.App
	publicURL string
	key       []byte
	configErr error
	initMu    sync.Mutex
	store     *SQLTokenStore
}

var _ einterfaces.PushProxyInterface = (*Service)(nil)

func New(a *app.App) einterfaces.PushProxyInterface {
	s := &Service{app: a, publicURL: strings.TrimRight(os.Getenv("WORKSPACE_PUSH_GATEWAY_PUBLIC_URL"), "/")}
	encoded := os.Getenv("WORKSPACE_PUSH_GATEWAY_KEY")
	if encoded == "" || s.publicURL == "" {
		return s
	}
	parsedURL, err := url.Parse(s.publicURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" ||
		parsedURL.User != nil || parsedURL.Path != "" || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		s.configErr = errors.New("push gateway public URL must be an HTTP(S) origin without credentials, path, query, or fragment")
		return s
	}
	s.key, s.configErr = base64.RawURLEncoding.DecodeString(encoded)
	if s.configErr == nil && len(s.key) != 32 {
		s.configErr = errors.New("push gateway key must decode to 32 bytes")
	}
	return s
}

func (s *Service) configured(cfg *model.Config) bool {
	return cfg != nil && s.publicURL != "" && len(s.key) == 32 && s.configErr == nil &&
		strings.TrimRight(model.SafeDereference(cfg.EmailSettings.PushNotificationServer), "/") == s.publicURL
}

func (s *Service) enabled(cfg *model.Config) bool {
	return s.app != nil && s.app.License().HasMHPNS() && s.configured(cfg)
}

func (s *Service) tokenStore(ctx context.Context) (*SQLTokenStore, error) {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	if s.store != nil {
		return s.store, nil
	}
	if s.configErr != nil {
		return nil, s.configErr
	}
	if len(s.key) != 32 {
		return nil, errors.New("push gateway key is not configured")
	}
	db := s.app.Srv().Store().GetInternalMasterDB()
	store, err := NewSQLTokenStore(db, s.key)
	if err != nil {
		return nil, err
	}
	if err = store.Init(ctx); err != nil {
		return nil, err
	}
	s.store = store
	return store, nil
}

func (s *Service) GetAuthToken() string {
	if !s.enabled(s.app.Config()) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	store, err := s.tokenStore(ctx)
	if err == nil {
		var token string
		token, err = store.Current(ctx)
		if err == nil {
			return token
		}
	}
	s.app.Log().Warn("Unable to read push gateway token", mlog.Err(err))
	return ""
}

func (s *Service) GenerateAuthToken() *model.AppError {
	if !s.enabled(s.app.Config()) {
		return tokenError("GenerateAuthToken", errors.New("licensed push gateway is not configured"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	store, err := s.tokenStore(ctx)
	if err == nil {
		_, err = store.Generate(ctx)
	}
	if err != nil {
		return tokenError("GenerateAuthToken", err)
	}
	return nil
}

func (s *Service) DeleteAuthToken() *model.AppError {
	if s.app == nil || !s.app.License().HasMHPNS() {
		return tokenError("DeleteAuthToken", errors.New("push gateway license is unavailable"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	store, err := s.tokenStore(ctx)
	if err == nil {
		err = store.Delete(ctx)
	}
	if err != nil {
		return tokenError("DeleteAuthToken", err)
	}
	return nil
}

func tokenError(where string, err error) *model.AppError {
	return model.NewAppError(where, "app.push_proxy_auth.app_error", nil, "", http.StatusServiceUnavailable).Wrap(err)
}

func (s *Service) MakeWorker() model.Worker {
	return &Worker{service: s, jobServer: s.app.Srv().Jobs, jobs: make(chan model.Job), stop: make(chan struct{}), done: make(chan struct{})}
}

func (s *Service) MakeScheduler() ejobs.Scheduler {
	jobServer := s.app.Srv().Jobs
	return &Scheduler{
		PeriodicScheduler: jobs.NewPeriodicScheduler(jobServer, model.JobTypePushProxyAuth, time.Minute,
			func(cfg *model.Config) bool { return s.enabled(cfg) }),
		createJob: func(rctx request.CTX) (*model.Job, *model.AppError) {
			return jobServer.CreateJobOnce(rctx, model.JobTypePushProxyAuth, nil)
		},
	}
}
