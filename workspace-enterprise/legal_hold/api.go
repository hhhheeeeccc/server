package legal_hold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/web"
	"golang.org/x/text/language"
)

const CaseAPIPath = "/api/v4/legal_hold"

// CaseAPI is explicitly constructed and mounted by its caller. Production
// main.go does not import/register it. Native session, CSRF and MFA checks run
// before licensed administration; every admitted operation has durable audit.
type CaseAPI struct {
	manager  *CaseManager
	exporter *CaseExporter
	audit    caseAPIAudit
	router   *mux.Router
}

func NewCaseAPI(manager *CaseManager, exporter *CaseExporter) (*CaseAPI, error) {
	if manager == nil || manager.app == nil || exporter == nil || exporter.manager != manager {
		return nil, errors.New("legal hold API requires matching manager and exporter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db := manager.journal.coordinator.fence.db
	if err := validateCaseAPIAudit(ctx, db); err != nil {
		return nil, err
	}
	api := &CaseAPI{manager: manager, exporter: exporter, audit: caseAPIAudit{db}, router: mux.NewRouter()}
	for _, route := range []struct{ operation, method, path string }{
		{"list_cases", "GET", "/cases"}, {"start_case", "POST", "/cases"},
		{"get_case", "GET", "/cases/{case_id}"}, {"resume_case", "POST", "/cases/{case_id}/resume"},
		{"change_scope", "POST", "/cases/{case_id}/scope"}, {"resume_scope", "POST", "/scope_requests/{request_id}/resume"},
		{"release_case", "POST", "/cases/{case_id}/release"},
		{"request_export", "POST", "/cases/{case_id}/exports"}, {"list_exports", "GET", "/cases/{case_id}/exports"},
		{"get_export", "GET", "/exports/{export_id}"}, {"resume_export", "POST", "/exports/{export_id}/resume"},
		{"download", "GET", "/exports/{export_id}/download"}, {"list_audit", "GET", "/audit"},
	} {
		api.router.Handle(CaseAPIPath+route.path, &web.Handler{Srv: manager.app.Srv(), HandleFunc: api.handle, HandlerName: "legal_hold_" + route.operation, RequireSession: true, RequireMfa: true}).Methods(route.method).Name(route.operation)
	}
	return api, nil
}

type caseAPIContextKey struct{}
type caseAPIRequest struct {
	id, operation, outcome, code string
	caseID, exportID, scopeID    string
	download                     *caseAPIDownload
}
type caseAPIDownload struct {
	file   *os.File
	result CaseExportResult
	rctx   request.CTX
}

// Native errors and JSON success are buffered until the final audit write.
// Streaming bytes require a separate, committed download admission instead.
func (a *CaseAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s := &caseAPIRequest{id: model.NewId(), operation: "unmatched", outcome: "succeeded"}
	var match mux.RouteMatch
	if a.router.Match(r, &match) && match.Route != nil {
		s.operation = match.Route.GetName()
	}
	method := r.Method
	if len(method) > 16 {
		method = "invalid"
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Legal-Hold-Request-Id", s.id)
	if err := a.audit.begin(r.Context(), s.id, s.operation, method); err != nil {
		a.writeError(w, r, s, 503, "audit_unavailable", nil)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), caseAPIContextKey{}, s))
	buffer := &caseAPIBuffer{header: make(http.Header)}
	a.router.ServeHTTP(buffer, r)
	if s.download != nil {
		defer s.download.file.Close()
		a.stream(w, r, buffer, s)
		return
	}
	status := buffer.status
	if status == 0 {
		status = 200
	}
	if buffer.overflow {
		status = 500
		s.code = "internal"
		s.outcome = "failed"
	}
	if status >= 400 && s.code == "" {
		s.code = "request_rejected"
		if status >= 500 {
			s.outcome = "failed"
		} else {
			s.outcome = "rejected"
		}
		buffer.body.Reset()
		a.writeError(buffer, r, s, status, s.code, nil)
	}
	if err := a.audit.finish(r.Context(), s, status, 0); err != nil {
		a.writeError(w, r, s, 503, "audit_unavailable", nil)
		return
	}
	for key, values := range buffer.header {
		w.Header()[key] = values
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buffer.body.Bytes())
}

func (a *CaseAPI) handle(c *web.Context, w http.ResponseWriter, r *http.Request) {
	s := r.Context().Value(caseAPIContextKey{}).(*caseAPIRequest)
	// web.Handler creates its request context from Background. Preserve native
	// authentication, while explicitly carrying the actual HTTP cancellation.
	rctx := c.AppContext.WithContext(r.Context())
	actor := ""
	if session := rctx.Session(); session != nil && model.IsValidId(session.UserId) {
		actor = session.UserId
	}
	_, authErr := a.manager.authorize(rctx)
	if err := a.audit.identity(r.Context(), s.id, actor, authErr == nil); err != nil {
		a.writeError(w, r, s, 503, "audit_unavailable", nil)
		return
	}
	if authErr != nil {
		a.writeError(w, r, s, 403, "forbidden", nil)
		return
	}
	for name, id := range mux.Vars(r) {
		if !model.IsValidId(id) {
			a.writeError(w, r, s, 400, "invalid_request", nil)
			return
		}
		switch name {
		case "case_id":
			s.caseID = id
		case "export_id":
			s.exportID = id
		case "request_id":
			s.scopeID = id
		}
	}
	a.dispatch(rctx, w, r, s)
}

func (a *CaseAPI) admit(w http.ResponseWriter, r *http.Request, s *caseAPIRequest, key string) bool {
	resource := s.exportID
	if resource == "" {
		resource = s.scopeID
	}
	if err := a.audit.action(r.Context(), s, key, s.caseID, resource); err != nil {
		a.writeError(w, r, s, 503, "audit_unavailable", nil)
		return false
	}
	return true
}

func (a *CaseAPI) stream(w http.ResponseWriter, r *http.Request, buffer *caseAPIBuffer, s *caseAPIRequest) {
	d := s.download
	if _, err := a.manager.authorize(d.rctx); err != nil {
		a.writeError(buffer, r, s, 403, "forbidden", nil)
		if auditErr := a.audit.finish(r.Context(), s, 403, 0); auditErr != nil {
			a.writeError(w, r, s, 503, "audit_unavailable", nil)
		} else {
			a.writeError(w, r, s, 403, "forbidden", nil)
		}
		return
	}
	if err := a.audit.exec(r.Context(), `INSERT INTO workspace_legal_hold.api_downloads(request_id,export_id,sha256,size) VALUES($1,$2,$3,$4)`, s.id, d.result.ID, d.result.SHA256, d.result.Size); err != nil {
		a.writeError(buffer, r, s, 503, "audit_unavailable", nil)
		_ = a.audit.finish(r.Context(), s, 503, 0)
		a.writeError(w, r, s, 503, "audit_unavailable", nil)
		return
	}
	for key, values := range buffer.header {
		w.Header()[key] = values
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="legal-hold-`+d.result.ID+`.zip"`)
	w.Header().Set("ETag", `"`+d.result.SHA256+`"`)
	writer := &caseAPIStream{ResponseWriter: w, authorize: func() error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		_, err := a.manager.authorize(d.rctx)
		return err
	}}
	http.ServeContent(writer, r, "legal-hold-"+d.result.ID+".zip", time.Time{}, d.file)
	if writer.status == 0 {
		writer.status = 200
	}
	if writer.err != nil {
		s.outcome, s.code = "interrupted", "stream_interrupted"
	} else if writer.status >= 400 {
		s.outcome, s.code = "rejected", "invalid_range"
	}
	if err := a.audit.finish(r.Context(), s, writer.status, writer.written); err != nil {
		// Bytes already written cannot be retracted. Leave the durable start
		// without a result and report the audit gap; never append JSON to ZIP.
		a.manager.app.Log().Error("Unable to record legal hold download result", mlog.String("audit_request_id", s.id))
	}
}

type caseAPIStream struct {
	http.ResponseWriter
	status    int
	written   int64
	err       error
	authorize func() error
}

func (w *caseAPIStream) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *caseAPIStream) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if err := w.authorize(); err != nil {
		w.err = err
		return 0, err
	}
	if w.status == 0 {
		w.WriteHeader(200)
	}
	n, err := w.ResponseWriter.Write(p)
	w.written += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}

type caseAPIBuffer struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func (w *caseAPIBuffer) Header() http.Header { return w.header }
func (w *caseAPIBuffer) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *caseAPIBuffer) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if w.body.Len()+len(p) > 2<<20 {
		w.overflow = true
		return 0, io.ErrShortWrite
	}
	return w.body.Write(p)
}

func (a *CaseAPI) writeError(w http.ResponseWriter, r *http.Request, s *caseAPIRequest, status int, code string, data any) {
	s.code = code
	switch {
	case code == "cancelled":
		s.outcome = "interrupted"
	case status == 202:
		s.outcome = "pending"
	case status >= 500:
		s.outcome = "failed"
	default:
		s.outcome = "rejected"
	}
	message := map[string][2]string{
		"forbidden":         {"لا تملك صلاحية إدارة الحفظ القانوني أو الترخيص المطلوب.", "Legal hold administration requires an active license and permission."},
		"invalid_request":   {"بيانات الطلب غير صحيحة.", "Invalid request."},
		"not_found":         {"العنصر المطلوب غير موجود.", "The requested item was not found."},
		"conflict":          {"يتعارض الطلب مع الحالة الحالية أو مفتاح طلب سابق.", "The request conflicts with the current state or a previous request key."},
		"pending":           {"حُفظ الطلب، ويحتاج إلى استئناف إكمال الحفظ.", "The request was saved; resume it to finish preservation."},
		"integrity":         {"تعذر التحقق من سلامة التصدير أو الأدلة.", "Export or evidence integrity verification failed."},
		"limit":             {"تجاوز الطلب الحد المسموح.", "The request exceeds the allowed limit."},
		"cancelled":         {"انقطع الطلب قبل اكتمال المحاولة.", "The request was interrupted."},
		"audit_unavailable": {"تعذر حفظ سجل التدقيق. أعد المحاولة بالمفتاح نفسه.", "The audit record could not be saved. Retry with the same request key."},
		"request_rejected":  {"تعذر قبول الطلب؛ تحقق من الجلسة والصلاحيات وطريقة الطلب.", "The request was rejected. Check the session, permissions and request method."},
		"internal":          {"تعذر إكمال الطلب. أعد المحاولة بالمفتاح نفسه.", "The request could not be completed. Retry with the same request key."},
	}[code]
	index := 0
	preferred := language.Arabic
	if model.SafeDereference(a.manager.app.Config().LocalizationSettings.DefaultClientLocale) == "en" {
		preferred = language.English
	}
	tags, _, _ := language.ParseAcceptLanguage(r.Header.Get("Accept-Language"))
	tag, _, _ := language.NewMatcher([]language.Tag{preferred, language.Arabic, language.English}).Match(tags...)
	base, _ := tag.Base()
	if base.String() == "en" {
		index = 1
	}
	w.Header().Del("Content-Length")
	w.Header().Del("Content-Encoding")
	w.Header().Set("Content-Type", "application/json")
	if index == 0 {
		w.Header().Set("Content-Language", "ar")
	} else {
		w.Header().Set("Content-Language", "en")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		ID        string `json:"id"`
		Message   string `json:"message"`
		Status    int    `json:"status_code"`
		RequestID string `json:"request_id"`
		Data      any    `json:"data,omitempty"`
	}{"enterprise.legal_hold." + code, message[index], status, s.id, data})
}

func apiJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
func apiPageLimit(r *http.Request) (int, error) {
	if value := r.URL.Query().Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 100 {
			return 0, errors.New("invalid limit")
		}
		return n, nil
	}
	return 50, nil
}
