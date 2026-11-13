package autotranslation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
)

type postTranslationRead struct {
	translation *model.Translation
	sourceText  string
}

const postTranslationColumns = `t.objectid,p.channelid,t.providerid,t.normhash,t.text,t.confidence,t.meta,t.state,t.updateat,p.message`

func readPostTranslation(row interface{ Scan(...any) error }, language string) (*postTranslationRead, error) {
	translation := &model.Translation{ObjectType: model.TranslationObjectTypePost, Lang: language, Type: model.TranslationTypeString}
	read := &postTranslationRead{translation: translation}
	var metadata []byte
	err := row.Scan(&translation.ObjectID, &translation.ChannelID, &translation.Provider, &translation.NormHash,
		&translation.Text, &translation.Confidence, &metadata, &translation.State, &translation.UpdateAt, &read.sourceText)
	if err != nil {
		return nil, err
	}
	// PostgreSQL pads the legacy CHAR(64) column with spaces for an empty
	// hash. Treat that as the pending sentinel, without refreshing its age.
	translation.NormHash = strings.TrimSpace(translation.NormHash)
	// As with the core batch store, omit malformed metadata without hiding
	// other valid rows. Never return undecodable payloads as plain post text.
	if len(metadata) != 0 && json.Unmarshal(metadata, &translation.Meta) != nil {
		return nil, nil
	}
	if kind, ok := translation.Meta["type"].(string); ok {
		translation.Type = model.TranslationType(kind)
	}
	if translation.Type == model.TranslationTypeObject {
		translation.ObjectJSON, translation.Text = json.RawMessage(translation.Text), ""
	}
	if translation.Provider == "" {
		translation.Provider = providerName
	}
	return read, nil
}

func (read *postTranslationRead) current() bool {
	digest := sha256.Sum256([]byte(strings.TrimSpace(read.sourceText)))
	return read.translation.NormHash == hex.EncodeToString(digest[:])
}

func (read *postTranslationRead) pending() bool {
	t := read.translation
	return t.State == model.TranslationStateProcessing && t.NormHash == "" && t.Text == "" && len(t.ObjectJSON) == 0
}

// Use one primary snapshot for source and payload. A paused read replica must
// neither resurrect deleted translations nor replace the primary's result.
func (s *Service) getPostTranslations(ids []string, language string) (map[string]*model.Translation, error) {
	result := make(map[string]*model.Translation)
	if len(ids) == 0 {
		return result, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), foregroundStoreTimeout)
	defer cancel()
	rows, err := s.server.Store().GetInternalMasterDB().QueryContext(ctx, `SELECT `+postTranslationColumns+`
		FROM translations t JOIN posts p ON p.id=t.objectid AND p.deleteat=0
		WHERE t.objecttype='post' AND t.objectid=ANY($1) AND t.dstlang=$2`, pq.Array(ids), language)
	if err != nil {
		return nil, err
	}
	var reads []*postTranslationRead
	for rows.Next() {
		read, scanErr := readPostTranslation(rows, language)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		if read != nil {
			reads = append(reads, read)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, read := range reads {
		if !read.current() && !read.pending() {
			read, err = s.reconcilePostTranslation(ctx, read)
			if err != nil {
				return nil, err
			}
		}
		if read != nil {
			result[read.translation.ObjectID] = read.translation
		}
	}
	return result, nil
}

// Historical rows may predate the edit trigger. Recheck under the source lock
// before resetting one to the durable retry queue, taking source then payload
// locks like foreground saves and deletion. A concurrent current result wins.
func (s *Service) reconcilePostTranslation(ctx context.Context, old *postTranslationRead) (*postTranslationRead, error) {
	tx, err := s.server.Store().GetInternalMasterDB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err := lockPostSource(ctx, tx, old.translation.ObjectID, old.translation.ChannelID, old.sourceText)
	if err != nil || !current {
		return nil, err
	}
	read, err := readPostTranslation(tx.QueryRowContext(ctx, `SELECT `+postTranslationColumns+`
		FROM translations t JOIN posts p ON p.id=t.objectid AND p.deleteat=0
		WHERE t.objecttype='post' AND t.objectid=$1 AND t.dstlang=$2 FOR UPDATE OF t`,
		old.translation.ObjectID, old.translation.Lang), old.translation.Lang)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || read == nil {
		return nil, err
	}
	if read.current() || read.pending() {
		return read, tx.Commit()
	}
	t := read.translation
	var metadata []byte
	err = tx.QueryRowContext(ctx, `UPDATE translations SET text='',normhash='',state='processing',confidence=NULL,
		updateat=floor(extract(epoch FROM clock_timestamp())*1000)::bigint,
		meta=CASE WHEN jsonb_typeof(meta)='object' THEN meta-'workspace_retry'-'error'-'src_lang'
			ELSE jsonb_build_object('type','string') END
		WHERE objecttype='post' AND objectid=$1 AND dstlang=$2 RETURNING updateat,meta`,
		t.ObjectID, t.Lang).Scan(&t.UpdateAt, &metadata)
	if err != nil {
		return nil, err
	}
	t.Meta = nil
	if err = json.Unmarshal(metadata, &t.Meta); err != nil {
		return nil, err
	}
	t.Text, t.ObjectJSON, t.NormHash, t.Confidence, t.State = "", nil, "", nil, model.TranslationStateProcessing
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.server.Store().AutoTranslation().InvalidatePostTranslationEtag(t.ChannelID)
	return read, nil
}
