package user_no_email

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mattermost/mattermost/server/public/model"
)

var errStoredEmailChanged = errors.New("user email changed before temporary validation")

type storedEmailState struct {
	email    string
	verified bool
}

func (s storedEmailState) apply(user *model.User) *model.User {
	result := user.DeepCopy()
	result.Email = s.email
	result.EmailVerified = s.verified
	return result
}

// The caller read an empty email before invoking the native validation path.
// A real address saved since that read must not be overwritten by preparation.
func (s *Service) prepareStoredEmail(parent context.Context, userID, placeholder string) error {
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return errors.New("master database connection is unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, schemaTimeout)
	defer cancel()
	result, err := db.ExecContext(ctx, `UPDATE users SET email = $2, emailverified = FALSE WHERE id = $1 AND email = ''`, userID, placeholder)
	if err != nil {
		return fmt.Errorf("prepare temporary email: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read prepared email row count: %w", err)
	}
	if rows != 1 {
		return errStoredEmailChanged
	}
	return nil
}

// Cleanup outlives HTTP cancellation, has a bounded lifetime, and only removes
// the address owned by this operation. Preserve a concurrently committed real
// address and its verification state, including in the API's returned user.
func (s *Service) clearStoredEmail(userID, placeholder string) (storedEmailState, error) {
	var state storedEmailState
	db := s.app.Srv().Store().GetInternalMasterDB()
	if db == nil {
		return state, errors.New("master database connection is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), schemaTimeout)
	defer cancel()
	err := db.QueryRowContext(ctx, `UPDATE users SET email = '', emailverified = FALSE
        WHERE id = $1 AND email = $2 RETURNING email,emailverified`, userID, placeholder).Scan(&state.email, &state.verified)
	if err == nil {
		return state, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return state, fmt.Errorf("clear temporary email: %w", err)
	}
	// Use a separate statement after the conditional UPDATE has completed, so
	// a concurrent commit is visible rather than reusing its older snapshot.
	err = db.QueryRowContext(ctx, `SELECT email,emailverified FROM users WHERE id=$1`, userID).Scan(&state.email, &state.verified)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	} // A deleted user has no placeholder to restore.
	if err != nil {
		return state, fmt.Errorf("inspect restored email: %w", err)
	}
	return state, nil
}
