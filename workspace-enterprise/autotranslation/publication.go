package autotranslation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

const publicationMetaKey = "workspace_publish_v1"
const publicationLease = 30 * time.Second

type publicationClaim struct {
	recoveryClaim
	normHash, revision, token string
}

// The saved result and its pending revision share one translation row. Claims
// only change metadata: updateat remains the version sent to clients and ETags.
// PostgreSQL time, not node clocks, controls expiry. No lock spans Publish.
const publicationClaimSQL = `WITH clock AS (
		SELECT floor(extract(epoch FROM clock_timestamp())*1000)::bigint AS now
	), candidate AS (
		SELECT t.objectid,t.objecttype,t.dstlang,p.channelid,p.message,clock.now
		FROM translations t CROSS JOIN clock ` + recoveryEligiblePost + `
		AND t.state IN ('ready','skipped') AND t.meta ? 'workspace_publish_v1'
		AND COALESCE(t.meta->>'type','string')='string'
		AND jsonb_typeof(t.meta->'workspace_publish_v1')='object'
		AND jsonb_typeof(t.meta#>'{workspace_publish_v1,id}')='string'
		AND length(t.meta#>>'{workspace_publish_v1,id}')=26
		AND CASE WHEN jsonb_typeof(t.meta#>'{workspace_publish_v1,lease_until}')='number'
			THEN (t.meta#>>'{workspace_publish_v1,lease_until}')::numeric ELSE 0 END <= clock.now
		AND ($3::text='' OR (t.objectid=$3 AND t.dstlang=$4))
		ORDER BY t.updateat,t.objectid,t.dstlang FOR UPDATE OF t SKIP LOCKED LIMIT 1
	)
	UPDATE translations t SET meta=jsonb_set(t.meta,'{workspace_publish_v1}',
		(t.meta->'workspace_publish_v1') || jsonb_build_object('lease',$2::text,'lease_until',q.now+$1::bigint),false)
	FROM candidate q WHERE t.objectid=q.objectid AND t.objecttype=q.objecttype AND t.dstlang=q.dstlang
	RETURNING t.objectid,t.dstlang,q.channelid,q.message,t.normhash,t.meta#>>'{workspace_publish_v1,id}'`

func (s *Service) claimPublication(ctx context.Context, objectID, language string) (*publicationClaim, error) {
	if !s.IsFeatureAvailable() || ctx.Err() != nil {
		return nil, ctx.Err()
	}
	claim := &publicationClaim{token: model.NewId()}
	err := s.server.Store().GetInternalMasterDB().QueryRowContext(ctx, publicationClaimSQL,
		publicationLease.Milliseconds(), claim.token, objectID, language).
		Scan(&claim.objectID, &claim.language, &claim.channelID, &claim.message, &claim.normHash, &claim.revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim translation publication: %w", err)
	}
	return claim, nil
}

func (s *Service) acknowledgePublication(ctx context.Context, claim *publicationClaim) error {
	if !s.IsFeatureAvailable() || ctx.Err() != nil {
		return ctx.Err()
	}
	// An old dispatcher cannot acknowledge a newer result or a replacement
	// lease. Losing the acknowledgement leaves a retryable record, not a loss.
	_, err := s.server.Store().GetInternalMasterDB().ExecContext(ctx, `UPDATE translations
		SET meta=meta-'workspace_publish_v1'
		WHERE objecttype='post' AND providerid='libretranslate' AND objectid=$1 AND dstlang=$2
		AND state IN ('ready','skipped') AND normhash=$3
		AND meta#>>'{workspace_publish_v1,id}'=$4 AND meta#>>'{workspace_publish_v1,lease}'=$5`,
		claim.objectID, claim.language, claim.normHash, claim.revision, claim.token)
	if err != nil {
		return fmt.Errorf("acknowledge translation publication: %w", err)
	}
	return nil
}

func (s *Service) dispatchPublication(ctx context.Context, claim *publicationClaim) error {
	sent, err := s.emitRecoveredTranslation(ctx, claim)
	if err != nil || !sent {
		return err
	}
	// Publish acknowledges local dispatch only, not delivery to every browser.
	// A crash before this SQL can duplicate dispatch after lease expiry.
	return s.acknowledgePublication(ctx, claim)
}

func (s *Service) publishRecoveredTranslation(ctx context.Context, recovered *recoveryClaim, normHash string) error {
	claim, err := s.claimPublication(ctx, recovered.objectID, recovered.language)
	if err != nil || claim == nil {
		return err
	}
	if claim.normHash != normHash || claim.message != recovered.message || claim.channelID != recovered.channelID {
		return nil
	}
	return s.dispatchPublication(ctx, claim)
}

func (s *Service) drainTranslationPublications(ctx context.Context) error {
	for range recoveryBatchSize {
		claim, err := s.claimPublication(ctx, "", "")
		if err != nil || claim == nil {
			return err
		}
		if err := s.dispatchPublication(ctx, claim); err != nil {
			return err
		}
	}
	return nil
}
