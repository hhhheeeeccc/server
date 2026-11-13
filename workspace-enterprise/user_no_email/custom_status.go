package user_no_email

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const customStatusPlaceholderPrefix = "no-email-custom-status-"
const customStatusEmailLockKey int64 = 6492261412

var errCustomStatusEmailChanged = errors.New("user email changed during custom status update")

// These handlers retain the core custom-status validation and permissions, then
// make an email-less user temporarily valid for the core UpdateUser path.
func (s *Service) updateCustomStatus(c *web.Context, w http.ResponseWriter, r *http.Request) {
	c.RequireUserId()
	if c.Err != nil {
		return
	}
	if !model.SafeDereference(c.App.Config().TeamSettings.EnableCustomUserStatuses) {
		c.Err = model.NewAppError("updateCustomStatus", "api.custom_status.disabled", nil, "", http.StatusNotImplemented)
		return
	}
	var status model.CustomStatus
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	err := decoder.Decode(&status)
	if err == nil {
		var trailing any
		if trailingErr := decoder.Decode(&trailing); trailingErr != io.EOF {
			err = trailingErr
			if err == nil {
				err = errors.New("unexpected data after custom status")
			}
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Err = model.NewAppError("updateCustomStatus", model.PayloadParseError, nil, "", http.StatusRequestEntityTooLarge).Wrap(err)
			return
		}
		c.SetInvalidParamWithErr("custom_status", err)
		return
	}
	if (status.Emoji == "" && status.Text == "") || !status.AreDurationAndExpirationTimeValid() {
		c.SetInvalidParam("custom_status")
		return
	}
	if !c.App.SessionHasPermissionToUser(c.AppContext, *c.AppContext.Session(), c.Params.UserId) {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}
	status.PreSave()
	c.Err = s.withValidEmail(c, r, c.Params.UserId, func() *model.AppError {
		return c.App.SetCustomStatus(c.AppContext, c.Params.UserId, &status)
	})
	if c.Err == nil {
		web.ReturnStatusOK(w)
	}
}

func (s *Service) removeCustomStatus(c *web.Context, w http.ResponseWriter, r *http.Request) {
	c.RequireUserId()
	if c.Err != nil {
		return
	}
	if !model.SafeDereference(c.App.Config().TeamSettings.EnableCustomUserStatuses) {
		c.Err = model.NewAppError("removeCustomStatus", "api.custom_status.disabled", nil, "", http.StatusNotImplemented)
		return
	}
	if !c.App.SessionHasPermissionToUser(c.AppContext, *c.AppContext.Session(), c.Params.UserId) {
		c.SetPermissionError(model.PermissionEditOtherUsers)
		return
	}
	c.Err = s.withValidEmail(c, r, c.Params.UserId, func() *model.AppError {
		return c.App.RemoveCustomStatus(c.AppContext, c.Params.UserId)
	})
	if c.Err == nil {
		web.ReturnStatusOK(w)
	}
}

func (s *Service) withValidEmail(c *web.Context, r *http.Request, userID string, update func() *model.AppError) (result *model.AppError) {
	user, appErr := c.App.GetUser(c.AppContext, userID)
	if appErr != nil {
		return appErr
	}
	if strings.TrimSpace(user.Email) != "" && !isCustomStatusPlaceholder(user.Email) {
		return update()
	}
	if err := s.ensureEmailSchema(r.Context()); err != nil {
		return model.NewAppError("CustomStatusWithoutEmail", "app.user.save.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	// Hold the same database lock used by startup recovery until the temporary
	// address has been removed. A different node may start during this update.
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return model.NewAppError("CustomStatusWithoutEmail", "app.user.save.app_error", nil, "", http.StatusInternalServerError).Wrap(errors.New("master database connection is unavailable"))
	}
	conn, lockTx, err := acquireCustomStatusEmailLock(r.Context(), db)
	if err != nil {
		return model.NewAppError("CustomStatusWithoutEmail", "app.user.save.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	defer conn.Close()
	defer lockTx.Rollback()
	// The first read may have seen another request's temporary address. Re-read
	// after waiting for that request to restore the empty address.
	c.App.InvalidateCacheForUser(userID)
	user, appErr = c.App.GetUser(c.AppContext, userID)
	if appErr != nil {
		return appErr
	}
	if strings.TrimSpace(user.Email) != "" && !isCustomStatusPlaceholder(user.Email) {
		return update()
	}
	if isCustomStatusPlaceholder(user.Email) {
		if err = s.clearCustomStatusPlaceholder(r.Context(), userID, user.Email); err != nil {
			return model.NewAppError("CustomStatusWithoutEmail", "app.user.save.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		}
		c.App.InvalidateCacheForUser(userID)
	}
	placeholder := customStatusPlaceholderPrefix + model.NewId() + "@users.invalid"
	if err := s.setCustomStatusPlaceholder(r.Context(), userID, placeholder); err != nil {
		if errors.Is(err, errCustomStatusEmailChanged) {
			return model.NewAppError("CustomStatusWithoutEmail", "app.user.save.app_error", nil, "", http.StatusConflict).Wrap(err)
		}
		return model.NewAppError("CustomStatusWithoutEmail", "app.user.save.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}
	c.App.InvalidateCacheForUser(userID)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), schemaTimeout)
		defer cancel()
		if err := s.clearCustomStatusPlaceholder(cleanupCtx, userID, placeholder); err != nil {
			result = model.NewAppError("CustomStatusWithoutEmail.restore", "app.user.save.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		}
		c.App.InvalidateCacheForUser(userID)
	}()
	return update()
}

func acquireCustomStatusEmailLock(ctx context.Context, db *sql.DB) (*sql.Conn, *sql.Tx, error) {
	for {
		// Pool acquisition respects the HTTP lifetime. Once acquired, the
		// transaction must outlive cancellation so restoration keeps its lock.
		conn, err := db.Conn(ctx)
		if err != nil {
			return nil, nil, err
		}
		tx, err := conn.BeginTx(context.Background(), nil)
		if err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
		var locked bool
		err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, customStatusEmailLockKey).Scan(&locked)
		if err == nil && locked {
			return conn, tx, nil
		}
		_ = tx.Rollback()
		_ = conn.Close()
		if err != nil {
			return nil, nil, err
		}
		// A lock waiter must not occupy a pool connection: the owner needs
		// another connection for the native user update and email restoration.
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func isCustomStatusPlaceholder(email string) bool {
	return strings.HasPrefix(email, customStatusPlaceholderPrefix) && strings.HasSuffix(email, "@users.invalid")
}

func (s *Service) setCustomStatusPlaceholder(ctx context.Context, userID, placeholder string) error {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return errors.New("master database connection is unavailable")
	}
	result, err := db.ExecContext(ctx, `UPDATE users SET email=$2, emailverified=FALSE WHERE id=$1 AND email=''`, userID, placeholder)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errCustomStatusEmailChanged
	}
	return nil
}

func (s *Service) clearCustomStatusPlaceholder(ctx context.Context, userID, placeholder string) error {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return errors.New("master database connection is unavailable")
	}
	_, err := db.ExecContext(ctx, `UPDATE users SET email='', emailverified=FALSE WHERE id=$1 AND email=$2`, userID, placeholder)
	return err
}

// A process can stop between the core user update and restoration of the
// original empty email. Recover only placeholders reserved for this flow.
func (s *Service) clearStaleCustomStatusPlaceholderEmails() {
	ctx, cancel := context.WithTimeout(context.Background(), schemaTimeout)
	defer cancel()
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		s.app.Log().Warn("Unable to begin custom status placeholder recovery", mlog.Err(err))
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, customStatusEmailLockKey); err != nil {
		s.app.Log().Warn("Unable to lock custom status placeholder recovery", mlog.Err(err))
		return
	}
	rows, err := tx.QueryContext(ctx, `UPDATE users SET email='', emailverified=FALSE
		WHERE email LIKE $1 RETURNING id`, customStatusPlaceholderPrefix+"%@users.invalid")
	if err != nil {
		s.app.Log().Warn("Unable to recover custom status placeholder emails", mlog.Err(err))
		return
	}
	var recovered []string
	for rows.Next() {
		var userID string
		if err = rows.Scan(&userID); err != nil {
			rows.Close()
			s.app.Log().Warn("Unable to read recovered custom status user", mlog.Err(err))
			return
		}
		recovered = append(recovered, userID)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		s.app.Log().Warn("Unable to finish recovering custom status emails", mlog.Err(err))
		return
	}
	if err = rows.Close(); err != nil {
		s.app.Log().Warn("Unable to close custom status recovery rows", mlog.Err(err))
		return
	}
	if err = tx.Commit(); err != nil {
		s.app.Log().Warn("Unable to commit custom status placeholder recovery", mlog.Err(err))
		return
	}
	for _, userID := range recovered {
		s.app.InvalidateCacheForUser(userID)
	}
}
