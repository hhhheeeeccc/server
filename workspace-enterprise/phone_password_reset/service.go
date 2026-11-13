package phone_password_reset

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/mattermost/enterprise/nova_sms"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const (
	basePath          = model.APIURLSuffix + "/workspace/password/phone"
	codeLifetime      = 5 * time.Minute
	codeCooldown      = time.Minute
	maxCodeAttempts   = 5
	maxHourlyRequests = 10
)

var validPhone = regexp.MustCompile(`^7[1378][0-9]{7}$`)
var validCode = regexp.MustCompile(`^[0-9]{6}$`)

type Service struct {
	app     *app.App
	db      *sql.DB
	nova    *nova_sms.Service
	initErr error
}

func Register(a *app.App, nova *nova_sms.Service) {
	s := &Service{app: a, db: a.Srv().Store().GetInternalMasterDB(), nova: nova}
	s.initErr = s.init(context.Background())
	if s.initErr != nil {
		a.Log().Error("Unable to initialize phone password recovery", mlog.Err(s.initErr))
	}
	s.register(http.MethodPost, basePath+"/request", s.requestCode)
	s.register(http.MethodPost, basePath+"/complete", s.complete)
}

func (s *Service) init(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS WorkspacePhonePasswordReset (
			Phone varchar(9) PRIMARY KEY, ChallengeId varchar(26) NOT NULL,
			UserId varchar(26) NOT NULL, CodeHash text NOT NULL,
			ExpiresAt bigint NOT NULL, RequestedAt bigint NOT NULL,
			Attempts integer NOT NULL DEFAULT 0, Used boolean NOT NULL DEFAULT false)`,
		`CREATE TABLE IF NOT EXISTS WorkspacePhonePasswordResetRate (
			IpHash varchar(64) PRIMARY KEY, WindowStart bigint NOT NULL,
			RequestCount integer NOT NULL)`,
	} {
		if _, err := s.db.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) register(method, path string, handler func(*web.Context, http.ResponseWriter, *http.Request)) {
	s.app.Srv().Router.Handle(path, &web.Handler{
		Srv: s.app.Srv(), HandleFunc: handler, HandlerName: web.GetHandlerName(handler),
	}).Methods(method)
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return false
	}
	return decoder.Decode(&struct{}{}) == io.EOF
}

func appError(id string, status int) *model.AppError {
	return model.NewAppError("PhonePasswordReset", id, nil, "", status)
}

func (s *Service) available(c *web.Context) bool {
	if s.app.License() == nil || s.initErr != nil {
		c.Err = appError("workspace.password.phone_unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (s *Service) allowRequest(ctx context.Context, ip string) (bool, error) {
	hash := sha256.Sum256([]byte(ip))
	now := time.Now().UnixMilli()
	var count int
	err := s.db.QueryRowContext(ctx, `INSERT INTO WorkspacePhonePasswordResetRate (IpHash, WindowStart, RequestCount)
		VALUES ($1,$2,1) ON CONFLICT (IpHash) DO UPDATE SET
		WindowStart=CASE WHEN WorkspacePhonePasswordResetRate.WindowStart<$3 THEN $2 ELSE WorkspacePhonePasswordResetRate.WindowStart END,
		RequestCount=CASE WHEN WorkspacePhonePasswordResetRate.WindowStart<$3 THEN 1 ELSE WorkspacePhonePasswordResetRate.RequestCount+1 END
		RETURNING RequestCount`, hex.EncodeToString(hash[:]), now, now-int64(time.Hour/time.Millisecond)).Scan(&count)
	return count <= maxHourlyRequests, err
}

func (s *Service) localUserForPhone(ctx context.Context, phone string) (string, error) {
	return localUserForPhoneFrom(ctx, s.db, phone)
}

// Both the pool and an existing transaction can provide this read. Completion
// must use its transaction rather than reserving another pool connection while
// it holds the challenge row lock.
func localUserForPhoneFrom(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, phone string) (string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT u.Id,
		COALESCE(u.RemoteId,'')='' AND NOT EXISTS (SELECT 1 FROM Bots b WHERE b.UserId=u.Id) AS Eligible
		FROM Users u
		JOIN PropertyValues pv ON pv.TargetId=u.Id AND pv.TargetType='user' AND pv.DeleteAt=0
		JOIN PropertyFields pf ON pf.Id=pv.FieldId AND pf.DeleteAt=0 AND pf.ObjectType='user'
		JOIN PropertyGroups pg ON pg.Id=pf.GroupId AND pg.Name='access_control'
		WHERE u.DeleteAt=0 AND u.AuthService=''
		AND pf.Type='text' AND pf.Attrs->>'value_type'='phone' AND pv.Value #>> '{}'=$1 LIMIT 2`, phone)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	var eligible bool
	for rows.Next() {
		var id string
		if err := rows.Scan(&id, &eligible); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(ids) != 1 || !eligible {
		return "", nil
	}
	return ids[0], nil
}

func randomCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func isLocalPasswordUser(user *model.User) bool {
	return user != nil && user.AuthService == "" && !user.IsBot && !user.IsRemote() && !user.IsMagicLinkEnabled()
}

func (s *Service) requestCode(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !s.available(c) {
		return
	}
	var input struct {
		Phone string `json:"phone"`
	}
	if !decode(w, r, &input) || !validPhone.MatchString(input.Phone) {
		c.Err = appError("workspace.password.invalid_phone", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	allowed, err := s.allowRequest(ctx, c.AppContext.IPAddress())
	if err != nil {
		c.Err = appError("workspace.password.phone_unavailable", http.StatusServiceUnavailable)
		return
	}
	if !allowed {
		c.Err = appError("workspace.password.rate_limited", http.StatusTooManyRequests)
		return
	}
	userID, err := s.localUserForPhone(ctx, input.Phone)
	if err != nil {
		c.Err = appError("workspace.password.phone_unavailable", http.StatusServiceUnavailable)
		return
	}
	if userID != "" {
		user, userErr := s.app.GetUser(c.AppContext, userID)
		if userErr == nil && isLocalPasswordUser(user) {
			s.issueCode(ctx, c, input.Phone, userID)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"OK"}`))
}

func (s *Service) issueCode(ctx context.Context, c *web.Context, phone, userID string) {
	code, err := randomCode()
	if err != nil {
		c.Logger.Warn("Unable to generate password recovery code", mlog.Err(err))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
	if err != nil {
		c.Logger.Warn("Unable to hash password recovery code", mlog.Err(err))
		return
	}
	now := time.Now().UnixMilli()
	challengeID := model.NewId()
	var storedID string
	err = s.db.QueryRowContext(ctx, `INSERT INTO WorkspacePhonePasswordReset (Phone,ChallengeId,UserId,CodeHash,ExpiresAt,RequestedAt)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (Phone) DO UPDATE SET
		ChallengeId=$2,UserId=$3,CodeHash=$4,ExpiresAt=$5,RequestedAt=$6,Attempts=0,Used=false
		WHERE WorkspacePhonePasswordReset.RequestedAt <= $7
		RETURNING ChallengeId`, phone, challengeID, userID, string(hash), now+int64(codeLifetime/time.Millisecond), now, now-int64(codeCooldown/time.Millisecond)).Scan(&storedID)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		c.Logger.Warn("Unable to store password recovery code", mlog.Err(err))
		return
	}
	if err := s.nova.SendPasswordResetCode(ctx, userID, "+967"+phone, code, challengeID); err != nil {
		c.Logger.Warn("Unable to request Nova password recovery SMS", mlog.Err(err))
		_, _ = s.db.ExecContext(ctx, `DELETE FROM WorkspacePhonePasswordReset WHERE Phone=$1 AND ChallengeId=$2`, phone, challengeID)
	}
}

func (s *Service) complete(c *web.Context, w http.ResponseWriter, r *http.Request) {
	if !s.available(c) {
		return
	}
	var input struct {
		Phone       string `json:"phone"`
		Code        string `json:"code"`
		NewPassword string `json:"new_password"`
	}
	if !decode(w, r, &input) || !validPhone.MatchString(input.Phone) || !validCode.MatchString(input.Code) {
		c.Err = appError("workspace.password.invalid_code", http.StatusBadRequest)
		return
	}
	if err := s.app.IsPasswordValid(c.AppContext, input.NewPassword); err != nil {
		c.Err = err
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	// Load the native profile before reserving the challenge transaction's
	// connection. Recheck this ID under the row lock in case the challenge was
	// replaced while the profile was being loaded.
	var candidateID string
	if err := s.db.QueryRowContext(ctx, `SELECT UserId FROM WorkspacePhonePasswordReset WHERE Phone=$1`, input.Phone).Scan(&candidateID); err != nil {
		c.Err = appError("workspace.password.invalid_code", http.StatusBadRequest)
		return
	}
	user, userErr := s.app.GetUser(c.AppContext.WithContext(ctx), candidateID)
	if userErr != nil || !isLocalPasswordUser(user) {
		c.Err = appError("workspace.password.invalid_code", http.StatusBadRequest)
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		c.Err = appError("workspace.password.phone_unavailable", http.StatusServiceUnavailable)
		return
	}
	defer tx.Rollback()
	var userID, hash string
	var expiresAt int64
	var attempts int
	var used bool
	err = tx.QueryRowContext(ctx, `SELECT UserId,CodeHash,ExpiresAt,Attempts,Used FROM WorkspacePhonePasswordReset WHERE Phone=$1 FOR UPDATE`, input.Phone).Scan(&userID, &hash, &expiresAt, &attempts, &used)
	if err != nil || userID != user.Id || used || expiresAt < time.Now().UnixMilli() || attempts >= maxCodeAttempts {
		c.Err = appError("workspace.password.invalid_code", http.StatusBadRequest)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(input.Code)) != nil {
		_, err = tx.ExecContext(ctx, `UPDATE WorkspacePhonePasswordReset SET Attempts=Attempts+1 WHERE Phone=$1`, input.Phone)
		if err != nil {
			c.Err = appError("workspace.password.phone_unavailable", http.StatusServiceUnavailable)
			return
		}
		if err = tx.Commit(); err != nil {
			c.Err = appError("workspace.password.phone_unavailable", http.StatusServiceUnavailable)
			return
		}
		c.Err = appError("workspace.password.invalid_code", http.StatusBadRequest)
		return
	}
	currentID, err := localUserForPhoneFrom(ctx, tx, input.Phone)
	if err != nil || currentID != userID {
		c.Err = appError("workspace.password.invalid_code", http.StatusBadRequest)
		return
	}
	if _, err = tx.ExecContext(ctx, `UPDATE WorkspacePhonePasswordReset SET Used=true WHERE Phone=$1`, input.Phone); err != nil {
		c.Err = appError("workspace.password.phone_unavailable", http.StatusServiceUnavailable)
		return
	}
	if err = tx.Commit(); err != nil {
		c.Err = appError("workspace.password.phone_unavailable", http.StatusServiceUnavailable)
		return
	}
	if appErr := s.app.UpdatePassword(c.AppContext, user, input.NewPassword); appErr != nil {
		c.Err = appErr
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"OK"}`))
}
