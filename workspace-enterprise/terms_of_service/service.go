package terms_of_service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

type Service struct {
	app *app.App
}

func Register(a *app.App) {
	service := &Service{app: a}
	registerRoute(a, http.MethodGet, service.getLatest)
	registerRoute(a, http.MethodPost, service.create)
}

func registerRoute(a *app.App, method string, handler func(*web.Context, http.ResponseWriter, *http.Request)) {
	a.Srv().Router.Handle(model.APIURLSuffix+"/terms_of_service", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(method)
}

func (s *Service) getLatest(c *web.Context, w http.ResponseWriter, _ *http.Request) {
	terms, err := latestFromMaster(c.AppContext.Context(), c.App.Srv().Store().GetInternalMasterDB())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) && s.app.License() != nil {
			if err := json.NewEncoder(w).Encode(&model.TermsOfService{}); err != nil {
				c.Logger.Warn("Error while writing response", mlog.Err(err))
			}
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			c.Err = model.NewAppError("GetLatestTermsOfService", app.ErrorTermsOfServiceNoRowsFound, nil, "", http.StatusNotFound)
			return
		}
		c.Logger.Error("Unable to read latest terms of service", mlog.Err(err))
		c.Err = model.NewAppError("GetLatestTermsOfService", "app.terms_of_service.get.app_error", nil, "", http.StatusInternalServerError)
		return
	}

	if err := json.NewEncoder(w).Encode(terms); err != nil {
		c.Logger.Warn("Error while writing response", mlog.Err(err))
	}
}

func latestFromMaster(ctx context.Context, db *sql.DB) (*model.TermsOfService, error) {
	if db == nil {
		return nil, errors.New("master database unavailable")
	}
	var terms model.TermsOfService
	err := db.QueryRowContext(ctx, `SELECT Id,CreateAt,UserId,Text FROM TermsOfService
		ORDER BY CreateAt DESC,Id DESC LIMIT 1`).Scan(&terms.Id, &terms.CreateAt, &terms.UserId, &terms.Text)
	if err != nil {
		return nil, err
	}
	return &terms, nil
}

func (s *Service) create(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionManageSystem) {
		c.SetPermissionError(model.PermissionManageSystem)
		return
	}
	license := s.app.License()
	if license == nil {
		c.Err = model.NewAppError("CreateTermsOfService", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	if license.Features == nil || !model.SafeDereference(license.Features.CustomTermsOfService) {
		c.Err = model.NewAppError("CreateTermsOfService", "api.create_terms_of_service.custom_terms_of_service_disabled.app_error", nil, "", http.StatusBadRequest)
		return
	}

	auditRec := c.MakeAuditRecord(model.AuditEventCreateTermsOfService, model.AuditStatusFail)
	defer c.LogAuditRec(auditRec)

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	var input struct {
		Text string `json:"text"`
	}
	err := decoder.Decode(&input)
	if err == nil {
		var trailing any
		if extraErr := decoder.Decode(&trailing); extraErr != io.EOF {
			err = extraErr
			if err == nil {
				err = errors.New("unexpected data after terms of service")
			}
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("CreateTermsOfService", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("text", err)
		return
	}
	text := input.Text
	if text == "" {
		c.Err = model.NewAppError("CreateTermsOfService", "api.create_terms_of_service.empty_text.app_error", nil, "", http.StatusBadRequest)
		return
	}

	if _, appErr := c.App.GetUser(c.AppContext, c.AppContext.Session().UserId); appErr != nil {
		c.Err = appErr
		return
	}
	current, err := publishLatest(r.Context(), c.App.Srv().Store().GetInternalMasterDB(), text, c.AppContext.Session().UserId)
	if err != nil {
		var appErr *model.AppError
		if errors.As(err, &appErr) {
			c.Err = appErr
		} else {
			c.Logger.Error("Unable to publish terms of service", mlog.Err(err))
			c.Err = model.NewAppError("CreateTermsOfService", "app.terms_of_service.create.app_error", nil, "", http.StatusInternalServerError)
		}
		return
	}

	// publishLatest writes directly through SQL instead of the store's Save
	// method, so invalidate its latest-version cache on every node.
	if cache, ok := c.App.Srv().Store().TermsOfService().(interface{ ClearCaches() }); ok {
		cache.ClearCaches()
	}
	auditRec.Success()
	if err := json.NewEncoder(w).Encode(current); err != nil {
		c.Logger.Warn("Error while writing response", mlog.Err(err))
	}
}

// Use the master database and a transaction-scoped lock so two server nodes
// cannot publish the same text after both read an older revision.
func publishLatest(ctx context.Context, db *sql.DB, text, userID string) (*model.TermsOfService, error) {
	if db == nil {
		return nil, errors.New("master database unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(11283544, 1)`); err != nil {
		return nil, err
	}
	var latest model.TermsOfService
	err = tx.QueryRowContext(ctx, `SELECT Id,CreateAt,UserId,Text FROM TermsOfService
		ORDER BY CreateAt DESC,Id DESC LIMIT 1`).Scan(&latest.Id, &latest.CreateAt, &latest.UserId, &latest.Text)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && latest.Text == text {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return &latest, nil
	}
	created := &model.TermsOfService{Text: text, UserId: userID}
	created.PreSave()
	if created.CreateAt <= latest.CreateAt {
		created.CreateAt = latest.CreateAt + 1
	}
	if appErr := created.IsValid(); appErr != nil {
		return nil, appErr
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO TermsOfService (Id,CreateAt,UserId,Text) VALUES ($1,$2,$3,$4)`,
		created.Id, created.CreateAt, created.UserId, created.Text); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return created, nil
}
