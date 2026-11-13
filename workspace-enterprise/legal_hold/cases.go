package legal_hold

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

var ErrCaseForbidden = errors.New("legal hold case administration is not permitted")
var ErrCaseScopeNotFound = errors.New("legal hold scope does not exist")
var ErrCaseNotActive = errors.New("legal hold export requires an active case")

type CaseScope struct {
	Kind     string
	TargetID string
}

type CaseRequest struct {
	Title  string
	Scopes []CaseScope
}

type CaseStatus struct {
	ID    string
	State string
}

// CaseManager is not registered with any HTTP route. A future authenticated
// handler must supply its real request. Existing holds keep preserving data
// if the license expires; all administrative operations still require a license.
type CaseManager struct {
	app     *app.App
	journal *CaseJournal
}

func NewCaseManager(a *app.App, journal *CaseJournal) (*CaseManager, error) {
	if a == nil || journal == nil || a.Srv().Store().GetInternalMasterDB() != journal.coordinator.fence.db {
		return nil, errors.New("legal hold case manager requires the matching application and journal")
	}
	return &CaseManager{app: a, journal: journal}, nil
}

func (m *CaseManager) authorize(rctx request.CTX) (string, error) {
	license := m.app.License()
	if license == nil || !model.SafeDereference(license.Features.Compliance) || rctx == nil || rctx.Session() == nil || !model.IsValidId(rctx.Session().UserId) {
		return "", ErrCaseForbidden
	}
	actor := rctx.Session().UserId
	user, appErr := m.app.GetUser(rctx, actor)
	if appErr != nil || user.DeleteAt != 0 || !m.app.HasPermissionTo(rctx, actor, model.PermissionManageSystem) {
		return "", ErrCaseForbidden
	}
	return actor, nil
}

// StartCapture atomically fixes the user/channel scope and snapshots extant
// posts, edit-history rows, temporary bodies, identity metadata and FileInfo.
// Scope includes all extant matching rows (including soft-deleted rows) and
// future matching writes. A failed byte copy returns the committed case ID in
// state preparing; it never announces activation or removes its SQL protection.
func (m *CaseManager) StartCapture(rctx request.CTX, input CaseRequest) (CaseStatus, error) {
	return m.startCapture(rctx, input, "")
}

func normalizeCaseRequest(input CaseRequest) (CaseRequest, error) {
	input.Title = strings.TrimSpace(input.Title)
	if !utf8.ValidString(input.Title) || utf8.RuneCountInString(input.Title) == 0 || utf8.RuneCountInString(input.Title) > 256 || len(input.Scopes) == 0 || len(input.Scopes) > 256 {
		return CaseRequest{}, errors.New("legal hold requires a title and one to 256 distinct user/channel scopes")
	}
	seen := map[CaseScope]bool{}
	for _, scope := range input.Scopes {
		if (scope.Kind != "users" && scope.Kind != "channels") || !model.IsValidId(scope.TargetID) || seen[scope] {
			return CaseRequest{}, errors.New("invalid or duplicated legal hold case scope")
		}
		seen[scope] = true
	}
	return input, nil
}

func (m *CaseManager) startCapture(rctx request.CTX, input CaseRequest, key string) (CaseStatus, error) {
	actor, err := m.authorize(rctx)
	if err != nil {
		return CaseStatus{}, err
	}
	input, err = normalizeCaseRequest(input)
	if err != nil {
		return CaseStatus{}, err
	}
	ctx, cancel := context.WithTimeout(rctx.Context(), 2*time.Minute)
	defer cancel()
	if key != "" {
		if err = validateCaseStartJournal(ctx, m.journal.coordinator.fence.db); err != nil {
			return CaseStatus{}, err
		}
	}
	c := m.journal.coordinator
	lease, err := c.vault.mutationLease(ctx)
	if err != nil {
		return CaseStatus{}, err
	}
	defer lease.Close()
	if err = c.recoverLocked(ctx); err != nil {
		return CaseStatus{}, err
	}
	if key != "" {
		status, exists, err := m.existingCaseStart(ctx, actor, key, input)
		if err != nil {
			return CaseStatus{}, err
		}
		if exists {
			if err = m.reauthorize(rctx, actor); err != nil {
				return CaseStatus{}, err
			}
			if status.State == "preparing" {
				return m.sealLocked(ctx, status, actor, rctx)
			}
			return status, nil
		}
	}
	tx, err := c.fence.begin(ctx)
	if err != nil {
		return CaseStatus{}, err
	}
	defer tx.Rollback()
	status, err := m.prepareCaptureTx(ctx, tx, input, actor, key)
	if err != nil {
		return CaseStatus{}, err
	}
	if err = m.reauthorize(rctx, actor); err != nil {
		return CaseStatus{}, err
	}
	if err = tx.Commit(); err != nil {
		return CaseStatus{}, err
	}
	return m.sealLocked(ctx, status, actor, rctx)
}

// prepareCaptureTx is called with the archive lease and SQL control barrier.
// Its caller authenticates and commits the case, scope, and request together.
func (m *CaseManager) prepareCaptureTx(ctx context.Context, tx *sql.Tx, input CaseRequest, actor, key string) (CaseStatus, error) {
	var err error
	for _, scope := range input.Scopes {
		var exists bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+m.journal.sources[scope.Kind]+" WHERE id=$1)", scope.TargetID).Scan(&exists); err != nil {
			return CaseStatus{}, err
		}
		if !exists {
			return CaseStatus{}, ErrCaseScopeNotFound
		}
	}
	status := CaseStatus{ID: model.NewId(), State: "preparing"}
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.cases(id,title,created_by) VALUES($1,$2,$3)`, status.ID, input.Title, actor); err != nil {
		return CaseStatus{}, err
	}
	for _, scope := range input.Scopes {
		if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_scopes VALUES($1,$2,$3)`, status.ID, scope.Kind, scope.TargetID); err != nil {
			return CaseStatus{}, err
		}
		if _, err = tx.ExecContext(ctx, "SELECT workspace_legal_hold.capture_case_object($1,$2,id,to_jsonb(src),'snapshot') FROM "+m.journal.sources[scope.Kind]+" src WHERE id=$3", status.ID, scope.Kind, scope.TargetID); err != nil {
			return CaseStatus{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_events(case_id,state,actor_id) VALUES($1,'preparing',$2)`, status.ID, actor); err != nil {
		return CaseStatus{}, err
	}
	if _, err = tx.ExecContext(ctx, "SELECT workspace_legal_hold.capture_case_post($1,to_jsonb(p),'snapshot') FROM "+m.journal.sources["posts"]+` p WHERE EXISTS(
		SELECT 1 FROM workspace_legal_hold.case_scopes s WHERE s.case_id=$1 AND
		((s.kind='users' AND s.target_id=p.userid) OR (s.kind='channels' AND s.target_id=p.channelid)))`, status.ID); err != nil {
		return CaseStatus{}, err
	}
	if _, err = tx.ExecContext(ctx, "SELECT workspace_legal_hold.capture_case_file($1,to_jsonb(f),true) FROM "+m.journal.sources["fileinfo"]+` f
		WHERE EXISTS(SELECT 1 FROM workspace_legal_hold.file_cases(to_jsonb(f)) c WHERE c.case_id=$1)`, status.ID); err != nil {
		return CaseStatus{}, err
	}
	if key != "" {
		payload, err := caseStartPayload(input)
		if err != nil {
			return CaseStatus{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_start_keys(actor_id,request_key,case_id,request_payload) VALUES($1,$2,$3,$4::jsonb)`, actor, key, status.ID, string(payload)); err != nil {
			return CaseStatus{}, err
		}
	}
	return status, nil
}

// ResumeCapture retries an already committed preparation. It verifies existing
// archive objects as well as copying pending files; it never fabricates success
// from a missing file or a failed previous attempt.
func (m *CaseManager) ResumeCapture(rctx request.CTX, caseID string) (CaseStatus, error) {
	actor, err := m.authorize(rctx)
	if err != nil {
		return CaseStatus{}, err
	}
	status, err := m.status(rctx.Context(), caseID)
	if err != nil {
		return CaseStatus{}, err
	}
	ctx, cancel := context.WithTimeout(rctx.Context(), 2*time.Minute)
	defer cancel()
	c := m.journal.coordinator
	lease, err := c.vault.mutationLease(ctx)
	if err != nil {
		return status, err
	}
	defer lease.Close()
	if err = c.recoverLocked(ctx); err != nil {
		return status, err
	}
	return m.sealLocked(ctx, status, actor, rctx)
}

func (m *CaseManager) status(ctx context.Context, caseID string) (CaseStatus, error) {
	status := CaseStatus{ID: caseID}
	if !model.IsValidId(caseID) {
		return CaseStatus{}, errors.New("invalid legal hold case ID")
	}
	err := m.journal.coordinator.fence.db.QueryRowContext(ctx, `SELECT state FROM workspace_legal_hold.case_events WHERE case_id=$1 ORDER BY id DESC LIMIT 1`, caseID).Scan(&status.State)
	if err == nil {
		var released bool
		released, err = caseReleased(ctx, m.journal.coordinator.fence.db, caseID)
		if released {
			status.State = "released"
		}
	}
	return status, err
}

func (m *CaseManager) sealLocked(ctx context.Context, status CaseStatus, actor string, rctx request.CTX) (CaseStatus, error) {
	c := m.journal.coordinator
	tx, err := c.fence.begin(ctx)
	if err != nil {
		return status, err
	}
	defer tx.Rollback()
	released, err := caseReleased(ctx, tx, status.ID)
	if err != nil {
		return status, err
	}
	if released {
		return CaseStatus{ID: status.ID, State: "released"}, ErrCaseReleased
	}
	if err = m.sealCaseFilesTx(ctx, tx, status.ID); err != nil {
		return status, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.case_events(case_id,state,actor_id) VALUES($1,'active',$2) ON CONFLICT(case_id,state) DO NOTHING`, status.ID, actor); err != nil {
		return status, err
	}
	if err = m.reauthorize(rctx, actor); err != nil {
		return status, err
	}
	if err = tx.Commit(); err != nil {
		return status, err
	}
	status.State = "active"
	return status, nil
}

func (m *CaseManager) sealCaseFilesTx(ctx context.Context, tx *sql.Tx, caseID string) error {
	c := m.journal.coordinator
	installed, err := lifecycleInstalled(ctx, tx)
	if err != nil {
		return err
	}
	members := "workspace_legal_hold.case_members"
	if scoped, scopeErr := scopeJournalInstalled(ctx, tx); scopeErr != nil {
		return scopeErr
	} else if scoped {
		members = "workspace_legal_hold.active_case_scope_members"
	}
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT entity_id FROM "+members+" WHERE case_id=$1 AND kind='fileinfo' ORDER BY entity_id", caseID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range ids {
		var reserved bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_legal_hold.file_reservations WHERE file_id=$1)`, id).Scan(&reserved); err != nil {
			return err
		}
		if !reserved {
			if err = c.fence.reserveFileOriginTx(ctx, tx, id, false); err != nil {
				return errors.Join(ErrPreservationPending, err)
			}
		}
		if installed {
			if _, err = tx.ExecContext(ctx, `SELECT workspace_legal_hold.refresh_case_file_metadata($1)`, id); err != nil {
				return err
			}
		}
		if _, err = preservePendingFileTx(ctx, tx, id, c.source, c.vault); err != nil {
			return err
		}
	}
	return nil
}
