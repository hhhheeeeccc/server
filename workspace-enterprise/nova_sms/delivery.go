package nova_sms

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

const (
	deliveryInterval = 5 * time.Second
	deliveryTimeout  = 8 * time.Second
)

var localPhone = regexp.MustCompile(`^7[1378][0-9]{7}$`)

type deliverySettings struct {
	endpoint, workflow, secret string
	enabledAt                  int64
}

type delivery struct {
	postID, userID string
	enabledAt      int64
	attempts       int
}

func (s *Service) initDelivery(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, settingsTimeout)
	defer cancel()
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS WorkspaceNovaCursor (Id integer PRIMARY KEY CHECK (Id=1), CreateAt bigint NOT NULL DEFAULT 0, PostId varchar(26) NOT NULL DEFAULT '')`,
		`INSERT INTO WorkspaceNovaCursor (Id) VALUES (1) ON CONFLICT (Id) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS WorkspaceNovaDelivery (
			PostId varchar(26) NOT NULL, UserId varchar(26) NOT NULL, EnabledAt bigint NOT NULL,
			Attempts integer NOT NULL DEFAULT 0, RetryAt bigint NOT NULL DEFAULT 0,
			Done boolean NOT NULL DEFAULT false, PRIMARY KEY (PostId, UserId))`,
		`CREATE INDEX IF NOT EXISTS idx_workspace_nova_delivery_retry ON WorkspaceNovaDelivery (RetryAt) WHERE NOT Done`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) runDelivery() {
	ctx := s.app.Srv().Platform().GoContext()
	ticker := time.NewTicker(deliveryInterval)
	defer ticker.Stop()
	for {
		if s.app.License() != nil {
			if err := s.deliveryPass(ctx); err != nil && ctx.Err() == nil {
				s.app.Log().Warn("Nova SMS delivery pass failed", mlog.Err(err))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) deliveryPass(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if s.app.License() == nil {
		return nil
	}
	var cfg deliverySettings
	var enabled bool
	if err := s.db.QueryRowContext(ctx, `SELECT Endpoint, WorkflowIdentifier, SecretKey, Enabled, EnabledAt
		FROM WorkspaceNovaSettings WHERE Id=1`).Scan(&cfg.endpoint, &cfg.workflow, &cfg.secret, &enabled, &cfg.enabledAt); err != nil {
		return err
	}
	if !enabled || cfg.workflow == "" || cfg.secret == "" || !validEndpoint(cfg.endpoint) {
		return nil
	}
	if err := s.enqueueUrgentDirectPosts(ctx, cfg.enabledAt); err != nil {
		return err
	}
	for i := 0; i < 20; i++ {
		if s.app.License() == nil {
			return nil
		}
		job, err := s.claimDelivery(ctx, cfg.enabledAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = s.sendDelivery(ctx, cfg, job); err != nil {
			s.app.Log().Warn("Nova SMS delivery failed", mlog.String("post_id", job.postID), mlog.Err(err))
		}
		if err = s.finishDelivery(ctx, job, err); err != nil {
			return err
		}
	}
	return nil
}

// Discover eligible recipients not already queued. A CreateAt watermark can
// permanently skip a post or priority whose transaction commits out of order.
// Keep the legacy cursor row only as a cross-node discovery lock; its stored
// watermark no longer excludes messages. The queue's primary key deduplicates
// scans and restarts, including discovery after a delayed commit.
func (s *Service) enqueueUrgentDirectPosts(ctx context.Context, enabledAt int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var lockID int
	if err = tx.QueryRowContext(ctx, `SELECT Id FROM WorkspaceNovaCursor WHERE Id=1 FOR UPDATE`).Scan(&lockID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.Id,cm.UserId FROM Posts p
		JOIN PostsPriority pp ON pp.PostId=p.Id AND pp.Priority='urgent'
		JOIN Channels ch ON ch.Id=p.ChannelId AND ch.Type='D' AND ch.DeleteAt=0
		JOIN ChannelMembers cm ON cm.ChannelId=p.ChannelId AND cm.UserId<>p.UserId
		JOIN Users recipient ON recipient.Id=cm.UserId AND recipient.DeleteAt=0
		WHERE p.DeleteAt=0 AND p.CreateAt >= $1 AND p.CreateAt <= $2
		AND NOT EXISTS (SELECT 1 FROM WorkspaceNovaDelivery d WHERE d.PostId=p.Id AND d.UserId=cm.UserId)
		ORDER BY p.CreateAt,p.Id,cm.UserId LIMIT 100`, enabledAt, time.Now().Add(-2*time.Second).UnixMilli())
	if err != nil {
		return err
	}
	type found struct {
		post, user string
	}
	var pending []found
	for rows.Next() {
		var item found
		if err = rows.Scan(&item.post, &item.user); err != nil {
			break
		}
		pending = append(pending, item)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, item := range pending {
		if _, err = tx.ExecContext(ctx, `INSERT INTO WorkspaceNovaDelivery (PostId,UserId,EnabledAt)
			VALUES ($1,$2,$3) ON CONFLICT (PostId,UserId) DO NOTHING`, item.post, item.user, enabledAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) claimDelivery(ctx context.Context, enabledAt int64) (delivery, error) {
	var job delivery
	err := s.db.QueryRowContext(ctx, `UPDATE WorkspaceNovaDelivery d SET Attempts=Attempts+1, RetryAt=$1
		WHERE (d.PostId,d.UserId) IN (SELECT PostId,UserId FROM WorkspaceNovaDelivery
			WHERE NOT Done AND EnabledAt=$2 AND RetryAt <= $3 AND Attempts < 5
			ORDER BY RetryAt LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING d.PostId,d.UserId,d.EnabledAt,d.Attempts`,
		time.Now().Add(time.Minute).UnixMilli(), enabledAt, time.Now().UnixMilli()).Scan(&job.postID, &job.userID, &job.enabledAt, &job.attempts)
	return job, err
}

func (s *Service) sendDelivery(ctx context.Context, cfg deliverySettings, job delivery) error {
	if s.app.License() == nil {
		return errors.New("Nova SMS requires an active Enterprise license")
	}
	if job.enabledAt != cfg.enabledAt {
		return nil
	}
	status, appErr := s.app.GetStatus(job.userID)
	if appErr != nil {
		return appErr
	}
	if status == nil || status.Status != model.StatusOffline {
		return nil
	}
	var message, phone string
	err := s.db.QueryRowContext(ctx, `SELECT p.Message,COALESCE((SELECT pv.Value #>> '{}'
		FROM PropertyValues pv JOIN PropertyFields pf ON pf.Id=pv.FieldId
		JOIN PropertyGroups pg ON pg.Id=pf.GroupId
		WHERE pv.TargetId=$2 AND pv.TargetType='user' AND pv.DeleteAt=0
		AND pf.DeleteAt=0 AND pf.ObjectType='user' AND pf.Type='text'
		AND pf.Attrs->>'value_type'='phone' AND pg.Name='access_control'
		ORDER BY pv.CreateAt LIMIT 1),'')
		FROM Posts p
		JOIN PostsPriority pp ON pp.PostId=p.Id AND pp.Priority='urgent'
		JOIN Channels ch ON ch.Id=p.ChannelId AND ch.Type='D' AND ch.DeleteAt=0
		JOIN ChannelMembers cm ON cm.ChannelId=p.ChannelId AND cm.UserId=$2 AND cm.UserId<>p.UserId
		JOIN Users recipient ON recipient.Id=cm.UserId AND recipient.DeleteAt=0
		JOIN WorkspaceNovaSettings settings ON settings.Id=1 AND settings.Enabled AND settings.EnabledAt=$3
		WHERE p.Id=$1 AND p.DeleteAt=0 AND p.Message<>'' AND p.CreateAt >= $3`, job.postID, job.userID, job.enabledAt).Scan(&message, &phone)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !localPhone.MatchString(phone) {
		return nil
	}
	if s.app.License() == nil {
		return errors.New("Nova SMS requires an active Enterprise license")
	}
	return triggerNova(ctx, cfg, job, message, "+967"+phone)
}

func triggerNova(ctx context.Context, cfg deliverySettings, job delivery, message, phone string) error {
	return sendNovaEvent(ctx, cfg, job.userID, message, phone, "urgent-"+job.postID+"-"+job.userID)
}

// SendPasswordResetCode sends a code through the configured SMS workflow.
// The urgent-message enabled switch does not control password recovery.
func (s *Service) SendPasswordResetCode(ctx context.Context, userID, phone, code, challengeID string) error {
	if s.initErr != nil || s.app.License() == nil {
		return errors.New("Nova SMS is unavailable")
	}
	var cfg deliverySettings
	if err := s.db.QueryRowContext(ctx, `SELECT Endpoint,WorkflowIdentifier,SecretKey FROM WorkspaceNovaSettings WHERE Id=1`).Scan(&cfg.endpoint, &cfg.workflow, &cfg.secret); err != nil {
		return err
	}
	if !validEndpoint(cfg.endpoint) || cfg.workflow == "" || cfg.secret == "" {
		return errors.New("Nova SMS is not configured")
	}
	message := "رمز إعادة تعيين كلمة المرور: " + code + ". صالح لمدة 5 دقائق."
	return sendNovaEvent(ctx, cfg, userID, message, phone, "password-reset-"+challengeID)
}

func sendNovaEvent(ctx context.Context, cfg deliverySettings, userID, message, phone, transactionID string) error {
	requestBody := struct {
		Name string `json:"name"`
		To   struct {
			SubscriberID string `json:"subscriberId"`
			Phone        string `json:"phone"`
		} `json:"to"`
		Payload       map[string]string `json:"payload"`
		TransactionID string            `json:"transactionId"`
	}{Name: cfg.workflow, Payload: map[string]string{"message": message, "text": message, "phone": phone}, TransactionID: transactionID}
	requestBody.To.SubscriberID, requestBody.To.Phone = userID, phone
	body, err := json.Marshal(requestBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.endpoint, "/")+"/v1/events/trigger", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "ApiKey "+cfg.secret)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: deliveryTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		// Redirecting a POST can forward the message, phone and API key to a
		// different service. Configure the provider's final endpoint explicitly.
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Nova returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (s *Service) finishDelivery(ctx context.Context, job delivery, sendErr error) error {
	if sendErr == nil {
		_, err := s.db.ExecContext(ctx, `UPDATE WorkspaceNovaDelivery SET Done=true WHERE PostId=$1 AND UserId=$2`, job.postID, job.userID)
		return err
	}
	backoff := time.Duration(job.attempts*job.attempts) * time.Minute
	_, err := s.db.ExecContext(ctx, `UPDATE WorkspaceNovaDelivery SET RetryAt=$3 WHERE PostId=$1 AND UserId=$2`, job.postID, job.userID, time.Now().Add(backoff).UnixMilli())
	return err
}
