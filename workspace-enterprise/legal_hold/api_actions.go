package legal_hold

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

type caseAPIScope struct {
	Kind     string `json:"kind"`
	TargetID string `json:"target_id"`
}

func apiScopes(scopes []caseAPIScope) []CaseScope {
	result := make([]CaseScope, len(scopes))
	for i, s := range scopes {
		result[i] = CaseScope{s.Kind, s.TargetID}
	}
	return result
}

type caseAPIKey struct {
	RequestKey string `json:"request_key"`
}
type caseAPIStart struct {
	RequestKey string         `json:"request_key"`
	Title      string         `json:"title"`
	Scopes     []caseAPIScope `json:"scopes"`
}
type caseAPIChange struct {
	RequestKey string         `json:"request_key"`
	Reason     string         `json:"reason"`
	Add        []caseAPIScope `json:"add"`
	Remove     []caseAPIScope `json:"remove"`
}
type caseAPIRelease struct {
	RequestKey string `json:"request_key"`
	Reason     string `json:"reason"`
}
type caseAPIStatus struct {
	ID    string `json:"id"`
	State string `json:"state"`
}
type caseAPIScopeStatus struct {
	ID     string `json:"id"`
	CaseID string `json:"case_id"`
	State  string `json:"state"`
}
type caseAPIReleaseStatus struct {
	caseAPIStatus
	ExportID string `json:"export_id"`
}

const caseAPIMaxBody = 64 << 10

func decodeCaseAPI(r *http.Request, dest any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, caseAPIMaxBody+1))
	if err != nil {
		return err
	}
	if len(body) > caseAPIMaxBody {
		return ErrExportLimit
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return errors.New("JSON object required")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(dest); err != nil {
		return err
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return errors.New("extra JSON value")
	}
	return nil
}

func (a *CaseAPI) dispatch(rctx request.CTX, w http.ResponseWriter, r *http.Request, s *caseAPIRequest) {
	// All routes reject ignored query/body fields. Request bodies and raw URLs
	// are never persisted in the audit journal.
	allowedQuery := map[string]bool{}
	switch s.operation {
	case "list_cases", "list_exports":
		allowedQuery = map[string]bool{"after": true, "limit": true}
	case "list_audit":
		allowedQuery = map[string]bool{"after": true, "limit": true, "case_id": true}
	}
	for key, values := range r.URL.Query() {
		if !allowedQuery[key] || len(values) != 1 {
			a.writeError(w, r, s, 400, "invalid_request", nil)
			return
		}
	}
	if r.Method == "GET" && (r.ContentLength > 0 || len(r.TransferEncoding) > 0) {
		a.writeError(w, r, s, 400, "invalid_request", nil)
		return
	}
	decode := func(dest any) bool {
		if err := decodeCaseAPI(r, dest); err != nil {
			if errors.Is(err, ErrExportLimit) {
				a.writeError(w, r, s, 413, "limit", nil)
			} else {
				a.writeError(w, r, s, 400, "invalid_request", nil)
			}
			return false
		}
		return true
	}
	switch s.operation {
	case "start_case":
		var body caseAPIStart
		if !decode(&body) {
			return
		}
		input := CaseRequest{Title: body.Title, Scopes: apiScopes(body.Scopes)}
		if _, err := normalizeCaseRequest(input); err != nil || !model.IsValidId(body.RequestKey) {
			a.writeError(w, r, s, 400, "invalid_request", nil)
			return
		}
		if !a.admit(w, r, s, body.RequestKey) {
			return
		}
		status, err := a.manager.StartCaptureWithKey(rctx, input, body.RequestKey)
		s.caseID = status.ID
		data := caseAPIStatus{status.ID, status.State}
		a.respond(w, r, s, data, err, status.State == "preparing")
	case "get_case":
		if !a.admit(w, r, s, "") {
			return
		}
		details, err := a.manager.Get(rctx, s.caseID)
		a.respond(w, r, s, apiCaseDetails(details), err, false)
	case "list_cases":
		limit, err := apiPageLimit(r)
		after := r.URL.Query().Get("after")
		if err != nil || after != "" && !model.IsValidId(after) {
			a.writeError(w, r, s, 400, "invalid_request", nil)
			return
		}
		if !a.admit(w, r, s, "") {
			return
		}
		items, err := a.manager.List(rctx, after, limit)
		result := make([]caseAPIOverview, 0, len(items))
		for _, item := range items {
			result = append(result, apiCaseOverview(item))
		}
		next := ""
		if len(items) == limit {
			next = items[len(items)-1].ID
		}
		a.respond(w, r, s, struct {
			Items []caseAPIOverview `json:"items"`
			Next  string            `json:"next_id"`
		}{result, next}, err, false)
	case "resume_case":
		var body struct{}
		if !decode(&body) || !a.admit(w, r, s, "") {
			return
		}
		status, err := a.manager.ResumeCapture(rctx, s.caseID)
		a.respond(w, r, s, caseAPIStatus{status.ID, status.State}, err, status.State == "preparing")
	case "change_scope":
		var body caseAPIChange
		if !decode(&body) {
			return
		}
		input := CaseScopeChangeRequest{CaseID: s.caseID, RequestKey: body.RequestKey, Reason: body.Reason, Add: apiScopes(body.Add), Remove: apiScopes(body.Remove)}
		if _, _, err := normalizeScopeChange(input); err != nil {
			a.writeError(w, r, s, 400, "invalid_request", nil)
			return
		}
		if !a.admit(w, r, s, body.RequestKey) {
			return
		}
		status, err := a.manager.ChangeScope(rctx, input)
		s.scopeID = status.ID
		a.respond(w, r, s, caseAPIScopeStatus{status.ID, status.CaseID, status.State}, err, status.State == "preparing")
	case "resume_scope":
		var body struct{}
		if !decode(&body) || !a.admit(w, r, s, "") {
			return
		}
		status, err := a.manager.ResumeScopeChange(rctx, s.scopeID)
		s.caseID = status.CaseID
		a.respond(w, r, s, caseAPIScopeStatus{status.ID, status.CaseID, status.State}, err, status.State == "preparing")
	case "release_case":
		var body caseAPIRelease
		if !decode(&body) {
			return
		}
		reason := strings.TrimSpace(body.Reason)
		if !model.IsValidId(body.RequestKey) || !utf8.ValidString(reason) || utf8.RuneCountInString(reason) < 1 || utf8.RuneCountInString(reason) > 2048 {
			a.writeError(w, r, s, 400, "invalid_request", nil)
			return
		}
		if !a.admit(w, r, s, body.RequestKey) {
			return
		}
		status, err := a.manager.Release(rctx, a.exporter, CaseReleaseRequest{CaseID: s.caseID, RequestKey: body.RequestKey, Reason: reason})
		s.exportID = status.ExportID
		a.respond(w, r, s, caseAPIReleaseStatus{caseAPIStatus{status.ID, status.State}, status.ExportID}, err, status.State == "released" && status.ExportID != "")
	case "request_export":
		var body caseAPIKey
		if !decode(&body) {
			return
		}
		if !model.IsValidId(body.RequestKey) {
			a.writeError(w, r, s, 400, "invalid_request", nil)
			return
		}
		if !a.admit(w, r, s, body.RequestKey) {
			return
		}
		result, err := a.exporter.ExportWithKey(rctx, s.caseID, body.RequestKey)
		s.exportID = result.ID
		state := "complete"
		if err != nil {
			state = "preparing"
		}
		a.respond(w, r, s, caseAPIExportStatus{ID: result.ID, CaseID: result.CaseID, State: state, Size: result.Size, SHA256: result.SHA256}, err, result.ID != "")
	case "resume_export":
		var body struct{}
		if !decode(&body) || !a.admit(w, r, s, "") {
			return
		}
		result, err := a.exporter.Resume(rctx, s.exportID)
		s.caseID = result.CaseID
		state := "complete"
		if err != nil {
			state = "preparing"
		}
		a.respond(w, r, s, caseAPIExportStatus{ID: result.ID, CaseID: result.CaseID, State: state, Size: result.Size, SHA256: result.SHA256}, err, result.CaseID != "")
	case "get_export":
		if !a.admit(w, r, s, "") {
			return
		}
		result, err := a.getAPIExport(rctx.Context(), s.exportID)
		s.caseID = result.CaseID
		a.respond(w, r, s, result, err, false)
	case "list_exports":
		limit, err := apiPageLimit(r)
		after := r.URL.Query().Get("after")
		if err != nil || after != "" && !model.IsValidId(after) {
			a.writeError(w, r, s, 400, "invalid_request", nil)
			return
		}
		if !a.admit(w, r, s, "") {
			return
		}
		data, err := a.listAPIExports(rctx.Context(), s.caseID, after, limit)
		a.respond(w, r, s, data, err, false)
	case "download":
		if !a.admit(w, r, s, "") {
			return
		}
		file, result, err := a.exporter.Open(rctx, s.exportID)
		if err != nil {
			a.respond(w, r, s, nil, err, false)
			return
		}
		s.caseID = result.CaseID
		s.download = &caseAPIDownload{file, result, rctx}
	case "list_audit":
		limit, err := apiPageLimit(r)
		if err != nil {
			a.writeError(w, r, s, 400, "invalid_request", nil)
			return
		}
		if !a.admit(w, r, s, "") {
			return
		}
		data, err := a.listAPIAudit(rctx.Context(), r.URL.Query().Get("case_id"), r.URL.Query().Get("after"), limit)
		a.respond(w, r, s, data, err, false)
	}
}

func (a *CaseAPI) respond(w http.ResponseWriter, r *http.Request, s *caseAPIRequest, data any, err error, pending bool) {
	if err == nil {
		apiJSON(w, 200, data)
		return
	}
	// database/sql may win the cancellation race by rolling back first and
	// returning ErrTxDone instead of the request's cancellation error.
	// Preserve that cause so the detached audit records an interruption.
	if contextErr := r.Context().Err(); contextErr != nil {
		err = errors.Join(err, contextErr)
	}
	status, code := 500, "internal"
	var sqlErr *pq.Error
	switch {
	case errors.Is(err, ErrCaseForbidden):
		status, code = 403, "forbidden"
	case errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrCaseScopeNotFound):
		status, code = 404, "not_found"
	case errors.Is(err, ErrCaseStartKeyConflict) || errors.Is(err, ErrCaseScopeKeyConflict) || errors.Is(err, ErrExportKeyConflict) || errors.Is(err, ErrCaseReleased) || errors.Is(err, ErrCaseScopePending) || errors.Is(err, ErrCaseNotActive):
		status, code = 409, "conflict"
	case errors.Is(err, errAPIInvalidRequest):
		status, code = 400, "invalid_request"
	case errors.Is(err, ErrIntegrity):
		status, code = 409, "integrity"
	case errors.Is(err, ErrExportLimit):
		status, code = 413, "limit"
	case errors.Is(err, context.Canceled):
		status, code = 408, "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		status, code = 504, "cancelled"
	case errors.As(err, &sqlErr):
		switch sqlErr.Code {
		case "23503":
			status, code = 404, "not_found"
		case "23514", "22P02":
			status, code = 400, "invalid_request"
		case "55000", "40001", "40P01":
			status, code = 409, "conflict"
		}
	}
	if pending && code == "internal" {
		status, code = 202, "pending"
	}
	if status != 202 {
		data = nil
	}
	a.writeError(w, r, s, status, code, data)
}
