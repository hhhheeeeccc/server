package browser_sentry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const (
	configRoute   = model.APIURLSuffix + "/workspace/browser_sentry/config"
	policyRoute   = model.APIURLSuffix + "/workspace/browser_sentry/policy"
	policyTimeout = 5 * time.Second
)

type Policy struct {
	Enabled             bool    `json:"enabled"`
	DSN                 string  `json:"dsn"`
	Environment         string  `json:"environment"`
	TracesSampleRate    float64 `json:"traces_sample_rate"`
	EnableSessionReplay bool    `json:"enable_session_replay"`
	AttachStacktrace    bool    `json:"attach_stacktrace"`
	SendDefaultPII      bool    `json:"send_default_pii"`
	Version             int64   `json:"version"`
}

type Service struct {
	app     *app.App
	db      *sql.DB
	initErr error
}

func Register(a *app.App) {
	s := &Service{app: a, db: a.Srv().Store().GetInternalMasterDB()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	s.initErr = s.initPolicy(ctx)
	cancel()
	if s.initErr != nil {
		a.Log().Error("Unable to initialize browser Sentry policy", mlog.Err(s.initErr))
	}
	for _, route := range []struct {
		path    string
		method  string
		handler func(*web.Context, http.ResponseWriter, *http.Request)
		public  bool
	}{
		{configRoute, http.MethodGet, s.getConfig, true},
		{policyRoute, http.MethodGet, s.getPolicy, false},
		{policyRoute, http.MethodPut, s.updatePolicy, false},
	} {
		a.Srv().Router.Handle(route.path, &web.Handler{
			Srv:            a.Srv(),
			HandleFunc:     route.handler,
			HandlerName:    web.GetHandlerName(route.handler),
			RequireSession: !route.public,
			RequireMfa:     !route.public,
		}).Methods(route.method)
	}
}

func (s *Service) initPolicy(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(6492261411)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS WorkspaceBrowserSentryPolicy (
		Id integer PRIMARY KEY CHECK (Id=1), Policy jsonb NOT NULL, Version bigint NOT NULL DEFAULT 0)`); err != nil {
		return err
	}
	defaults := Policy{Environment: "production"}
	if value := strings.TrimSpace(os.Getenv("WORKSPACE_BROWSER_SENTRY_ENVIRONMENT")); value != "" {
		defaults.Environment = value
	}
	defaults.DSN = strings.TrimSpace(os.Getenv("WORKSPACE_BROWSER_SENTRY_DSN"))
	defaults.Enabled = strings.EqualFold(os.Getenv("WORKSPACE_BROWSER_SENTRY_ENABLED"), "true")
	if rate, err := strconv.ParseFloat(os.Getenv("WORKSPACE_BROWSER_SENTRY_TRACES_SAMPLE_RATE"), 64); err == nil {
		defaults.TracesSampleRate = rate
	}
	if err := validate(defaults); err != nil {
		s.app.Log().Warn("Ignoring invalid initial browser Sentry settings", mlog.Err(err))
		defaults = Policy{Environment: "production"}
	}
	raw, err := json.Marshal(defaults)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO WorkspaceBrowserSentryPolicy (Id, Policy) VALUES (1, $1)
		ON CONFLICT (Id) DO NOTHING`, string(raw))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func validate(policy Policy) error {
	if policy.TracesSampleRate < 0 || policy.TracesSampleRate > 1 || math.IsNaN(policy.TracesSampleRate) || math.IsInf(policy.TracesSampleRate, 0) {
		return errors.New("invalid traces sample rate")
	}
	if len(policy.Environment) == 0 || len(policy.Environment) > 64 || strings.TrimSpace(policy.Environment) != policy.Environment {
		return errors.New("invalid environment")
	}
	if policy.DSN == "" {
		if policy.Enabled {
			return errors.New("enabled browser Sentry requires a DSN")
		}
		return nil
	}
	parsed, err := url.Parse(policy.DSN)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User == nil || parsed.User.Username() == "" || parsed.Path == "" {
		return errors.New("invalid browser Sentry DSN")
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		return errors.New("browser Sentry DSN must not contain a password")
	}
	return nil
}

func (s *Service) read(ctx context.Context) (Policy, error) {
	if s.initErr != nil {
		return Policy{}, s.initErr
	}
	ctx, cancel := context.WithTimeout(ctx, policyTimeout)
	defer cancel()
	var raw []byte
	var policy Policy
	var version int64
	if err := s.db.QueryRowContext(ctx, `SELECT Policy, Version FROM WorkspaceBrowserSentryPolicy WHERE Id=1`).Scan(&raw, &version); err != nil {
		return Policy{}, err
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		return Policy{}, err
	}
	policy.Version = version
	if err := validate(policy); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

func (s *Service) getConfig(c *web.Context, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.app.License() == nil {
		writeJSON(c, w, Policy{})
		return
	}
	policy, err := s.read(r.Context())
	if err != nil {
		c.Err = unavailable(err)
		return
	}
	if !policy.Enabled {
		policy = Policy{}
	}
	writeJSON(c, w, policy)
}

func (s *Service) getPolicy(c *web.Context, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.authorize(c, false) {
		return
	}
	policy, err := s.read(r.Context())
	if err != nil {
		c.Err = unavailable(err)
		return
	}
	writeJSON(c, w, policy)
}

func (s *Service) updatePolicy(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !s.authorize(c, true) {
		return
	}
	var requested Policy
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&requested); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("UpdateBrowserSentryPolicy", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("policy", err)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("UpdateBrowserSentryPolicy", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParam("policy")
		return
	}
	if requested.Version < 0 || validate(requested) != nil {
		c.SetInvalidParam("policy")
		return
	}
	audit := c.MakeAuditRecord("updateBrowserSentryPolicy", model.AuditStatusFail)
	defer c.LogAuditRec(audit)
	audit.AddMeta("enabled", requested.Enabled)
	audit.AddMeta("replay", requested.EnableSessionReplay)
	saved, err := s.savePolicy(r.Context(), requested)
	if errors.Is(err, sql.ErrNoRows) {
		c.Err = model.NewAppError("UpdateBrowserSentryPolicy", "ent.browser_sentry.policy_conflict", nil, "", http.StatusConflict)
		return
	}
	if err != nil {
		c.Err = unavailable(err)
		return
	}
	audit.Success()
	writeJSON(c, w, saved)
}

func (s *Service) savePolicy(ctx context.Context, requested Policy) (Policy, error) {
	ctx, cancel := context.WithTimeout(ctx, policyTimeout)
	defer cancel()
	raw, err := json.Marshal(requested)
	if err != nil {
		return Policy{}, err
	}
	var version int64
	err = s.db.QueryRowContext(ctx, `UPDATE WorkspaceBrowserSentryPolicy
		SET Policy=$1, Version=Version+1 WHERE Id=1 AND Version=$2 RETURNING Version`, string(raw), requested.Version).Scan(&version)
	if err != nil {
		return Policy{}, err
	}
	requested.Version = version
	return requested, nil
}

func (s *Service) authorize(c *web.Context, write bool) bool {
	if s.app.License() == nil {
		c.Err = model.NewAppError("BrowserSentryPolicy", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return false
	}
	permission := model.PermissionSysconsoleReadEnvironmentLogging
	if write {
		permission = model.PermissionSysconsoleWriteEnvironmentLogging
	}
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), permission) {
		c.SetPermissionError(permission)
		return false
	}
	return true
}

func unavailable(err error) *model.AppError {
	return model.NewAppError("BrowserSentryPolicy", "ent.browser_sentry.policy_unavailable", nil, "", http.StatusServiceUnavailable).Wrap(err)
}

func writeJSON(c *web.Context, w http.ResponseWriter, value Policy) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		c.Logger.Warn("Unable to write browser Sentry policy", mlog.Err(err))
	}
}
