package autotranslation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mattermost/mattermost/server/public/model"
)

// Re-read the winning result on the primary after persistence. An edit, delete,
// opt-out or another worker can win before publication. A source fence also
// travels with the event so clients reject it if an edit overtakes delivery.
// No provider request or database transaction is held while publishing.
func (s *Service) emitRecoveredTranslation(ctx context.Context, claim *publicationClaim) (bool, error) {
	if !s.IsFeatureAvailable() || ctx.Err() != nil {
		return false, ctx.Err()
	}
	var text, state, sourceLanguage string
	var updatedAt int64
	err := s.server.Store().GetInternalMasterDB().QueryRowContext(ctx, `
		SELECT t.text,t.state,t.updateat,COALESCE(t.meta->>'src_lang','')
		FROM translations t `+recoveryEligiblePost+`
		AND t.objectid=$1 AND t.dstlang=$2 AND p.message=$3 AND t.normhash=$4
		AND p.channelid=$5 AND t.state IN ('ready','skipped')
		AND COALESCE(t.meta->>'type','string')='string'
		AND t.meta#>>'{workspace_publish_v1,id}'=$6
		AND t.meta#>>'{workspace_publish_v1,lease}'=$7
		AND CASE WHEN jsonb_typeof(t.meta#>'{workspace_publish_v1,lease_until}')='number'
			THEN (t.meta#>>'{workspace_publish_v1,lease_until}')::numeric ELSE 0 END
			> floor(extract(epoch FROM clock_timestamp())*1000)`,
		claim.objectID, claim.language, claim.message, claim.normHash, claim.channelID, claim.revision, claim.token).
		Scan(&text, &state, &updatedAt, &sourceLanguage)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read recovered translation for publication: %w", err)
	}
	if !s.IsFeatureAvailable() || ctx.Err() != nil {
		return false, ctx.Err()
	}
	event := model.NewWebSocketEvent(model.WebsocketEventPostTranslationUpdated, "", claim.channelID, "", nil, "")
	event.GetBroadcast().ReliableClusterSend = true
	event.Add("object_id", claim.objectID)
	event.Add("channel_id", claim.channelID)
	event.Add("source_text", claim.message)
	event.Add("translations", map[string]any{claim.language: map[string]any{
		"state": state, "translation": text, "translation_type": "string",
		"src_lang": sourceLanguage, "update_at": updatedAt,
	}})
	s.server.Platform().Publish(event)
	return true, nil
}
