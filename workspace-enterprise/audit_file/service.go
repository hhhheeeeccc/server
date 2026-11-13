package audit_file

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
	"github.com/mattermost/mattermost/server/v8/config"
)

const (
	defaultRecords = 200
	maxRecords     = 1000
	maxScanBytes   = 8 << 20
)

type Status struct {
	Enabled    bool `json:"enabled"`
	HasRecords bool `json:"has_records"`
}

type Service struct {
	app *app.App
}

func Register(a *app.App) {
	service := &Service{app: a}
	service.register(model.APIURLSuffix+"/audits/file/status", service.getStatus)
	service.register(model.APIURLSuffix+"/audits/file", service.getRecords)
	service.register(model.APIURLSuffix+"/audits/file/page", service.getPage)
}

func (s *Service) register(path string, handler func(*web.Context, http.ResponseWriter, *http.Request)) {
	s.app.Srv().Router.Handle(path, &web.Handler{
		Srv:            s.app.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodGet)
}

func (s *Service) getStatus(c *web.Context, w http.ResponseWriter, _ *http.Request) {
	if !authorize(c) {
		return
	}

	cfg := c.App.Config().ExperimentalAuditSettings
	if cfg.FileEnabled == nil || !*cfg.FileEnabled || cfg.FileName == nil || strings.TrimSpace(*cfg.FileName) == "" {
		writeJSON(c, w, &Status{})
		return
	}

	status := &Status{Enabled: true}
	path, err := validatedPath(*cfg.FileName)
	if err != nil {
		c.Logger.Warn("Rejected audit log path", mlog.Err(err))
		c.Err = fileReadError("WorkspaceEnterprise.GetAuditFileStatus", http.StatusForbidden)
		return
	}
	if err := c.App.Srv().Audit.Flush(); err != nil {
		c.Logger.Warn("Failed to flush audit log before checking its status", mlog.Err(err))
	}

	status.HasRecords, err = auditFileHasRecords(path)
	if err != nil {
		c.Logger.Warn("Failed to inspect audit log", mlog.Err(err))
		c.Err = fileReadError("WorkspaceEnterprise.GetAuditFileStatus", http.StatusInternalServerError)
		return
	}

	writeJSON(c, w, status)
}

func auditFileHasRecords(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("audit log is not a regular file")
	}
	return info.Size() > 0, nil
}

func (s *Service) getRecords(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !authorize(c) {
		return
	}

	cfg := c.App.Config().ExperimentalAuditSettings
	if cfg.FileEnabled == nil || !*cfg.FileEnabled || cfg.FileName == nil || strings.TrimSpace(*cfg.FileName) == "" {
		c.Err = featureDisabledError()
		return
	}
	path, err := validatedPath(*cfg.FileName)
	if err != nil {
		c.Logger.Warn("Rejected audit log path", mlog.Err(err))
		c.Err = fileReadError("WorkspaceEnterprise.GetAuditFileRecords", http.StatusForbidden)
		return
	}

	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		limit = defaultRecords
	}
	if limit > maxRecords {
		limit = maxRecords
	}

	if err := c.App.Srv().Audit.Flush(); err != nil {
		c.Logger.Warn("Failed to flush audit log before reading it", mlog.Err(err))
	}

	records, err := readRecords(path, limit)
	if err != nil {
		c.Logger.Warn("Failed to read audit log", mlog.Err(err))
		c.Err = fileReadError("WorkspaceEnterprise.GetAuditFileRecords", http.StatusInternalServerError)
		return
	}

	writeJSON(c, w, records)
}

func (s *Service) getPage(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !authorizePage(c) {
		return
	}
	cfg := c.App.Config().ExperimentalAuditSettings
	if cfg.FileEnabled == nil || !*cfg.FileEnabled || cfg.FileName == nil || strings.TrimSpace(*cfg.FileName) == "" {
		c.Err = featureDisabledError()
		return
	}
	path, err := validatedPath(*cfg.FileName)
	if err != nil {
		c.Logger.Warn("Rejected audit log path", mlog.Err(err))
		c.Err = fileReadError("WorkspaceEnterprise.GetAuditFilePage", http.StatusForbidden)
		return
	}
	limit := pageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxPageSize {
			c.Err = model.NewAppError("WorkspaceEnterprise.GetAuditFilePage", "api.context.invalid_param.app_error", nil, "invalid limit", http.StatusBadRequest)
			return
		}
	}
	if err := c.App.Srv().Audit.Flush(); err != nil {
		c.Logger.Warn("Failed to flush audit log before reading it", mlog.Err(err))
	}
	page, err := readFilteredPage(path, limit, r.URL.Query().Get("cursor"), r.URL.Query().Get("q"))
	if err != nil {
		if errors.Is(err, errInvalidCursor) {
			c.Err = model.NewAppError("WorkspaceEnterprise.GetAuditFilePage", "api.context.invalid_param.app_error", nil, "invalid cursor", http.StatusBadRequest)
		} else if errors.Is(err, errStaleCursor) {
			c.Err = model.NewAppError("WorkspaceEnterprise.GetAuditFilePage", "workspace.enterprise.audit_file.stale.app_error", nil, "audit file changed", http.StatusConflict)
		} else if errors.Is(err, errRecordTooLarge) {
			c.Logger.Warn("Audit record exceeds page response limit", mlog.Err(err))
			c.Err = fileReadError("WorkspaceEnterprise.GetAuditFilePage", http.StatusRequestEntityTooLarge)
		} else {
			c.Logger.Warn("Failed to read audit log page", mlog.Err(err))
			c.Err = fileReadError("WorkspaceEnterprise.GetAuditFilePage", http.StatusInternalServerError)
		}
		return
	}
	enrichPage(c, page)
	writeJSON(c, w, page)
}

func authorize(c *web.Context) bool {
	if !model.MinimumEnterpriseLicense(c.App.License()) {
		c.Err = model.NewAppError("WorkspaceEnterprise.AuditFile", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return false
	}
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionReadAudits) {
		c.SetPermissionError(model.PermissionReadAudits)
		return false
	}
	return true
}

func authorizePage(c *web.Context) bool {
	if !authorize(c) {
		return false
	}
	if !c.App.SessionHasPermissionToAndNotRestrictedAdmin(*c.AppContext.Session(), model.PermissionGetLogs) {
		c.SetPermissionError(model.PermissionGetLogs)
		return false
	}
	return true
}

func validatedPath(path string) (string, error) {
	if err := config.ValidateLogFilePath(path, config.GetLogRootPath()); err != nil {
		return "", err
	}
	return path, nil
}

func fileReadError(where string, status int) *model.AppError {
	return model.NewAppError(where, "api.admin.file_read_error", nil, "", status)
}

func featureDisabledError() *model.AppError {
	err := model.NewAppError("WorkspaceEnterprise.GetAuditFileRecords", "workspace.enterprise.audit_file.disabled.app_error", nil, "", http.StatusNotImplemented)
	err.Message = "Audit file logging is disabled."
	err.SkipTranslation = true
	return err
}

func readRecords(path string, limit int) ([]json.RawMessage, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 {
		return []json.RawMessage{}, nil
	}

	readSize := info.Size()
	if readSize > maxScanBytes {
		readSize = maxScanBytes
	}

	buf := make([]byte, readSize)
	if _, err = file.ReadAt(buf, info.Size()-readSize); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	lines := strings.Split(string(buf), "\n")
	if readSize < info.Size() && len(lines) > 0 {
		lines = lines[1:]
	}

	records := make([]json.RawMessage, 0, limit)
	for i := len(lines) - 1; i >= 0 && len(records) < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || !json.Valid([]byte(line)) {
			continue
		}
		records = append(records, json.RawMessage(line))
	}

	return records, nil
}

func writeJSON(c *web.Context, w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		c.Logger.Warn("Failed to write workspace Enterprise response", mlog.Err(err))
	}
}
