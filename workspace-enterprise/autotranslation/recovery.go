package autotranslation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
)

const (
	recoverySweepTimeout = 2 * time.Minute
	recoveryCallTimeout  = time.Minute
	recoveryMaxBackoff   = 6 * time.Hour
)

type recoveryWorker struct {
	*jobs.SimpleWorker
	service *Service
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
}

func (w *recoveryWorker) Run() {
	w.mu.Lock()
	if w.ctx.Err() != nil {
		w.ctx, w.cancel = w.service.newWorkContext()
	}
	w.mu.Unlock()
	w.SimpleWorker.Run()
}

func (w *recoveryWorker) Stop() {
	w.mu.Lock()
	w.cancel()
	w.mu.Unlock()
	w.SimpleWorker.Stop()
}

type recoveryClaim struct {
	objectID, language, channelID, message, lease string
	meta                                          map[string]any
	attempts                                      int
}

// The source of a retry is the current post on the primary, never a retained
// copy of its text. Recovery only handles ordinary string posts from our provider.
// The same predicate gates claiming, each network request and the final write.
const recoveryEligiblePost = `
	JOIN posts p ON p.id=t.objectid AND p.type='' AND p.deleteat=0
	JOIN channels c ON c.id=p.channelid AND c.deleteat=0 AND c.autotranslation=true
	WHERE t.objecttype='post' AND t.providerid='libretranslate'
	AND EXISTS (SELECT 1 FROM channelmembers cm JOIN users u ON u.id=cm.userid
		WHERE cm.channelid=p.channelid AND cm.autotranslationdisabled=false
		AND u.deleteat=0 AND u.locale=t.dstlang)`

// SKIP LOCKED and the claim UPDATE are one statement. No database lock or
// transaction is held while inference runs, and another node sees the lease.
const recoveryClaimSQL = `WITH clock AS (
		SELECT floor(extract(epoch FROM clock_timestamp())*1000)::bigint AS now
	), candidate AS (
		SELECT t.objectid,t.objecttype,t.dstlang,p.channelid,p.message,clock.now
		FROM translations t CROSS JOIN clock ` + recoveryEligiblePost + `
		AND COALESCE(t.meta->>'type','string')='string'
		AND (t.meta IS NULL OR jsonb_typeof(t.meta)='object')
		AND (t.meta->'workspace_retry' IS NULL OR jsonb_typeof(t.meta->'workspace_retry')='object')
		AND t.updateat<clock.now-$1::bigint
		AND (t.state='processing' OR (t.state='unavailable' AND
			CASE WHEN jsonb_typeof(t.meta#>'{workspace_retry,next_at}')='number'
			THEN (t.meta#>>'{workspace_retry,next_at}')::numeric ELSE 0 END <= clock.now))
		ORDER BY t.updateat,t.objectid,t.dstlang FOR UPDATE OF t SKIP LOCKED LIMIT 1
	)
	UPDATE translations t SET state='processing',updateat=q.now,channelid=q.channelid,
		meta=jsonb_set(COALESCE(t.meta,'{}'::jsonb),'{workspace_retry}',
			jsonb_build_object('lease',$2::text,'attempts',
				LEAST(32,GREATEST(0,CASE WHEN jsonb_typeof(t.meta#>'{workspace_retry,attempts}')='number'
				THEN (t.meta#>>'{workspace_retry,attempts}')::numeric ELSE 0 END))::int+1),true)
	FROM candidate q WHERE t.objectid=q.objectid AND t.objecttype=q.objecttype AND t.dstlang=q.dstlang
	RETURNING t.objectid,t.dstlang,q.channelid,q.message,t.meta`

func (s *Service) claimRecovery(ctx context.Context) (*recoveryClaim, error) {
	lease := model.NewId()
	claim := &recoveryClaim{lease: lease}
	var meta []byte
	err := s.server.Store().GetInternalMasterDB().QueryRowContext(ctx, recoveryClaimSQL, recoveryAge.Milliseconds(), lease).
		Scan(&claim.objectID, &claim.language, &claim.channelID, &claim.message, &meta)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim translation recovery: %w", err)
	}
	if err = json.Unmarshal(meta, &claim.meta); err != nil {
		return nil, fmt.Errorf("read translation recovery metadata: %w", err)
	}
	var envelope struct {
		Retry struct {
			Attempts int `json:"attempts"`
		} `json:"workspace_retry"`
	}
	if err = json.Unmarshal(meta, &envelope); err != nil {
		return nil, fmt.Errorf("read translation recovery attempt: %w", err)
	}
	claim.attempts = envelope.Retry.Attempts
	return claim, nil
}

func (s *Service) recoveryAllowed(ctx context.Context, claim *recoveryClaim) (bool, error) {
	if !s.IsFeatureAvailable() || ctx.Err() != nil {
		return false, ctx.Err()
	}
	var allowed bool
	err := s.server.Store().GetInternalMasterDB().QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM translations t `+recoveryEligiblePost+`
		AND t.objectid=$1 AND t.dstlang=$2 AND t.state='processing'
		AND t.meta#>>'{workspace_retry,lease}'=$3 AND p.message=$4)`,
		claim.objectID, claim.language, claim.lease, claim.message).Scan(&allowed)
	return allowed, err
}

func recoveryDelay(attempts int) time.Duration {
	delay := recoveryAge
	for i := 1; i < attempts && delay < recoveryMaxBackoff; i++ {
		delay *= 2
	}
	return min(delay, recoveryMaxBackoff)
}

func (s *Service) recoverTranslations(parent context.Context, logger mlog.LoggerIFace) error {
	ctx, cancel := context.WithTimeout(parent, recoverySweepTimeout)
	defer cancel()
	if !s.IsFeatureAvailable() {
		return nil
	}
	if err := ensureRecoveryIndex(ctx, s.server.Store().GetInternalMasterDB()); err != nil {
		return err
	}
	if err := ensurePublicationIndex(ctx, s.server.Store().GetInternalMasterDB()); err != nil {
		return err
	}
	// Dispatch committed results before starting any new inference. The queue is
	// durable across services and processes, including a crash after result save.
	if err := s.drainTranslationPublications(ctx); err != nil {
		return err
	}
	for range recoveryBatchSize {
		if !s.IsFeatureAvailable() || ctx.Err() != nil {
			return ctx.Err()
		}
		claim, err := s.claimRecovery(ctx)
		if err != nil || claim == nil {
			return err
		}
		requestCtx, requestCancel := context.WithTimeout(ctx, recoveryCallTimeout)
		err = s.retryClaim(requestCtx, claim)
		requestCancel()
		if err != nil {
			return err
		}
	}
	logger.Debug("Translation recovery reached its batch limit")
	return nil
}

func (s *Service) retryClaim(ctx context.Context, claim *recoveryClaim) error {
	allowed, err := s.recoveryAllowed(ctx, claim)
	if err != nil || !allowed {
		return err
	}
	text := strings.TrimSpace(claim.message)
	digest := sha256.Sum256([]byte(text))
	translation := &model.Translation{ObjectType: model.TranslationObjectTypePost, ObjectID: claim.objectID,
		ChannelID: claim.channelID, Lang: claim.language, Provider: providerName, Type: model.TranslationTypeString,
		NormHash: hex.EncodeToString(digest[:]), State: model.TranslationStateSkipped, Meta: claim.meta}
	translation.Meta["type"] = string(model.TranslationTypeString)
	var remoteErr *model.AppError
	if text != "" {
		var source string
		source, translation.Confidence, remoteErr = s.DetectRemote(ctx, text)
		translation.Meta["src_lang"] = source
		if remoteErr == nil && !sameLanguage(source, claim.language) {
			allowed, err = s.recoveryAllowed(ctx, claim)
			if err != nil || !allowed {
				return err
			}
			var detected string
			var confidence *float64
			translation.Text, detected, confidence, remoteErr = s.translateRemote(ctx, text, claim.language)
			if detected != "" {
				translation.Meta["src_lang"] = detected
			}
			if confidence != nil {
				translation.Confidence = confidence
			}
			translation.State = model.TranslationStateReady
		}
	}
	delete(translation.Meta, "error")
	retry := map[string]any{"attempts": claim.attempts}
	if remoteErr != nil {
		translation.State = model.TranslationStateUnavailable
		translation.Meta["error"] = remoteErr.DetailedError
	}
	translation.Meta["workspace_retry"] = retry
	delete(translation.Meta, publicationMetaKey)
	if translation.State == model.TranslationStateReady || translation.State == model.TranslationStateSkipped {
		translation.Meta[publicationMetaKey] = map[string]any{"id": model.NewId()}
	}
	allowed, err = s.recoveryAllowed(ctx, claim)
	if err != nil || !allowed {
		return err
	}
	meta, err := json.Marshal(translation.Meta)
	if err != nil {
		return err
	}
	// A foreground edit, another lease, removal or opt-out can win while the
	// provider runs. Recheck the canonical source and eligibility inside the write.
	result, err := s.server.Store().GetInternalMasterDB().ExecContext(ctx, `WITH clock AS (
		SELECT floor(extract(epoch FROM clock_timestamp())*1000)::bigint AS now
	) UPDATE translations t
		SET state=$5::text,text=$6,confidence=$7,normhash=$8,updateat=clock.now,
		meta=CASE WHEN $5::text='unavailable' THEN jsonb_set($9::jsonb,'{workspace_retry,next_at}',
			to_jsonb(clock.now+$10::bigint),true) ELSE $9::jsonb END
		FROM clock
		WHERE t.objecttype='post' AND t.objectid=$1 AND t.dstlang=$2
		AND t.state='processing' AND t.meta#>>'{workspace_retry,lease}'=$3
		AND EXISTS (SELECT 1 FROM posts p JOIN channels c ON c.id=p.channelid
			WHERE p.id=t.objectid AND p.type='' AND p.deleteat=0 AND p.message=$4
			AND c.deleteat=0 AND c.autotranslation=true
			AND EXISTS (SELECT 1 FROM channelmembers cm JOIN users u ON u.id=cm.userid
				WHERE cm.channelid=p.channelid AND cm.autotranslationdisabled=false AND u.deleteat=0 AND u.locale=t.dstlang))`,
		claim.objectID, claim.language, claim.lease, claim.message, translation.State, translation.Text,
		translation.Confidence, translation.NormHash, string(meta), recoveryDelay(claim.attempts).Milliseconds())
	if err != nil {
		return fmt.Errorf("save recovered translation: %w", err)
	}
	count, err := result.RowsAffected()
	if err == nil && count > 0 {
		s.server.Store().AutoTranslation().InvalidatePostTranslationEtag(claim.channelID)
		if translation.State == model.TranslationStateReady || translation.State == model.TranslationStateSkipped {
			return s.publishRecoveredTranslation(ctx, claim, translation.NormHash)
		}
	}
	return err
}
