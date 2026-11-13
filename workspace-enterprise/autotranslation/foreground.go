package autotranslation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

const foregroundStoreTimeout = 5 * time.Second

// Edits can overtake an in-flight translation. Do not send superseded text
// again, reuse its cached result, or return a translation for the old source.
func (s *Service) sourceCurrent(ctx context.Context, objectType, objectID, channelID, text string) (bool, *model.AppError) {
	if objectType != model.TranslationObjectTypePost {
		return true, nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, foregroundStoreTimeout)
	defer cancel()
	var current bool
	err := s.server.Store().GetInternalMasterDB().QueryRowContext(queryCtx, `SELECT EXISTS (
		SELECT 1 FROM posts WHERE id=$1 AND channelid=$2 AND deleteat=0 AND message=$3)`,
		objectID, channelID, text).Scan(&current)
	if err != nil {
		if ctx.Err() != nil {
			return false, canceledTranslationError()
		}
		return false, providerError("AutoTranslation.Source", http.StatusInternalServerError, err)
	}
	return current, nil
}

func lockPostSource(ctx context.Context, tx *sql.Tx, objectID, channelID, text string) (bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM posts
		WHERE id=$1 AND channelid=$2 AND deleteat=0 AND message=$3 FOR SHARE`, objectID, channelID, text).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Lock the live source before touching the translation, in the same order as
// edits/deletion. No lock is held during inference. PostgreSQL re-evaluates the
// source predicate if this SELECT has to wait for a concurrent source edit.
func (s *Service) savePostTranslation(parent context.Context, translation *model.Translation, sourceText string) (*model.Translation, error) {
	if err := translation.IsValid(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, foregroundStoreTimeout)
	defer cancel()
	tx, err := s.server.Store().GetInternalMasterDB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err := lockPostSource(ctx, tx, translation.ObjectID, translation.ChannelID, sourceText)
	if err != nil || !current {
		return nil, err
	}
	meta := maps.Clone(translation.Meta)
	if meta == nil {
		meta = make(map[string]any)
	}
	meta["type"] = string(translation.Type)
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO translations AS t
		(objectid,objecttype,channelid,dstlang,providerid,normhash,text,confidence,meta,state,updateat)
		VALUES ($1,'post',$2,$3,$4,$5,$6,$7,$8::jsonb,$9,floor(extract(epoch FROM clock_timestamp())*1000)::bigint)
		ON CONFLICT (objectid,objecttype,dstlang) DO UPDATE SET
		channelid=EXCLUDED.channelid,providerid=EXCLUDED.providerid,normhash=EXCLUDED.normhash,text=EXCLUDED.text,
		confidence=EXCLUDED.confidence,meta=EXCLUDED.meta,state=EXCLUDED.state,updateat=EXCLUDED.updateat
		WHERE t.normhash IS DISTINCT FROM EXCLUDED.normhash OR t.state NOT IN ('ready','skipped')`,
		translation.ObjectID, translation.ChannelID, translation.Lang, translation.Provider, translation.NormHash,
		translation.Text, translation.Confidence, string(encoded), translation.State)
	if err != nil {
		return nil, fmt.Errorf("save current post translation: %w", err)
	}
	// A ready result for the same text wins over a concurrent failure. Read the
	// actual winning row on the primary while locks hold, including on no-op.
	stored := &model.Translation{ObjectID: translation.ObjectID, ObjectType: model.TranslationObjectTypePost, Lang: translation.Lang}
	var metadata []byte
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(channelid,''),providerid,normhash,text,confidence,meta,state,updateat
		FROM translations WHERE objectid=$1 AND objecttype='post' AND dstlang=$2`, translation.ObjectID, translation.Lang).
		Scan(&stored.ChannelID, &stored.Provider, &stored.NormHash, &stored.Text, &stored.Confidence, &metadata, &stored.State, &stored.UpdateAt)
	if err != nil {
		return nil, err
	}
	if len(metadata) != 0 {
		if err = json.Unmarshal(metadata, &stored.Meta); err != nil {
			return nil, err
		}
	}
	stored.Type = model.TranslationTypeString
	if kind, ok := stored.Meta["type"].(string); ok {
		stored.Type = model.TranslationType(kind)
	}
	if stored.Type == model.TranslationTypeObject {
		stored.ObjectJSON, stored.Text = json.RawMessage(stored.Text), ""
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return stored, nil
}

// A blank edit can retain an attachment but has no translatable message. Finish
// its invalidated rows instead of leaving an unclaimable processing item.
func (s *Service) skipBlankPostTranslations(parent context.Context, objectID, channelID, sourceText string) error {
	ctx, cancel := context.WithTimeout(parent, foregroundStoreTimeout)
	defer cancel()
	tx, err := s.server.Store().GetInternalMasterDB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := lockPostSource(ctx, tx, objectID, channelID, sourceText)
	if err != nil || !current {
		return err
	}
	digest := sha256.Sum256(nil)
	_, err = tx.ExecContext(ctx, `UPDATE translations SET state='skipped',text='',normhash=$2,
		updateat=floor(extract(epoch FROM clock_timestamp())*1000)::bigint
		WHERE objecttype='post' AND objectid=$1 AND state='processing' AND normhash=''`, objectID, hex.EncodeToString(digest[:]))
	if err != nil {
		return err
	}
	return tx.Commit()
}
