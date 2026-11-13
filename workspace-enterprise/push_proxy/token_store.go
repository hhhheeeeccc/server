package push_proxy

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/mattermost/mattermost/server/public/model"
)

// SQLTokenStore shares one push credential across cluster nodes. The caller
// supplies a stable 256-bit encryption key from outside the database.
type SQLTokenStore struct {
	db   *sql.DB
	aead cipher.AEAD
}

var errRotationPending = errors.New("previous push token is still awaiting rotation confirmation")
var errNoRotationPending = errors.New("no push token rotation is awaiting confirmation")

func NewSQLTokenStore(db *sql.DB, key []byte) (*SQLTokenStore, error) {
	if db == nil || len(key) != 32 {
		return nil, errors.New("push token storage requires a database and a 32-byte encryption key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SQLTokenStore{db: db, aead: aead}, nil
}

func (s *SQLTokenStore) Init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS WorkspacePushProxyTokens (
		Id integer PRIMARY KEY CHECK (Id=1),
		Ciphertext bytea, Nonce bytea, CurrentHash bytea,
		PreviousHash bytea,
		CHECK ((Ciphertext IS NULL AND Nonce IS NULL AND CurrentHash IS NULL) OR
		       (Ciphertext IS NOT NULL AND Nonce IS NOT NULL AND CurrentHash IS NOT NULL))
	)`)
	if err != nil {
		return fmt.Errorf("initialize push token table: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO WorkspacePushProxyTokens (Id) VALUES (1) ON CONFLICT (Id) DO NOTHING`)
	return err
}

// Generate creates the first token or starts a rotation. During a rotation,
// the old token remains accepted until ConfirmRotation succeeds. A second
// rotation cannot overwrite that previous token.
func (s *SQLTokenStore) Generate(ctx context.Context) (string, error) {
	return s.generate(ctx, false)
}

// EnsureInitialToken creates a token only while the locked database row is
// empty. Concurrent workers must never interpret another worker's first token
// as a request to rotate it.
func (s *SQLTokenStore) EnsureInitialToken(ctx context.Context) error {
	if _, err := s.generate(ctx, true); err != nil {
		return err
	}
	// Verify the shared key even when another worker created the token.
	token, err := s.Current(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("push token was removed during initialization")
	}
	return nil
}

func (s *SQLTokenStore) generate(ctx context.Context, onlyIfMissing bool) (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	newCiphertext := s.aead.Seal(nil, nonce, []byte(token), []byte("workspace-push-proxy-token-v1"))
	hash := sha256.Sum256([]byte(token))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var ciphertext, storedNonce, currentHash, previousHash []byte
	if err = tx.QueryRowContext(ctx, `SELECT Ciphertext, Nonce, CurrentHash, PreviousHash FROM WorkspacePushProxyTokens WHERE Id=1 FOR UPDATE`).
		Scan(&ciphertext, &storedNonce, &currentHash, &previousHash); err != nil {
		return "", err
	}
	if len(currentHash) != 0 {
		if _, err = s.decryptToken(ciphertext, storedNonce, currentHash); err != nil {
			return "", err
		}
	}
	if onlyIfMissing && len(currentHash) != 0 {
		return "", nil
	}
	if len(previousHash) != 0 {
		return "", errRotationPending
	}
	if _, err = tx.ExecContext(ctx, `UPDATE WorkspacePushProxyTokens
		SET Ciphertext=$1, Nonce=$2, CurrentHash=$3, PreviousHash=$4 WHERE Id=1`,
		newCiphertext, nonce, hash[:], nullableHash(currentHash)); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}

func nullableHash(hash []byte) any {
	if len(hash) == 0 {
		return nil
	}
	return hash
}

func (s *SQLTokenStore) Current(ctx context.Context) (string, error) {
	var ciphertext, nonce, storedHash []byte
	if err := s.db.QueryRowContext(ctx, `SELECT Ciphertext, Nonce, CurrentHash
		FROM WorkspacePushProxyTokens WHERE Id=1`).Scan(&ciphertext, &nonce, &storedHash); err != nil {
		return "", err
	}
	if len(storedHash) == 0 {
		return "", nil
	}
	return s.decryptToken(ciphertext, nonce, storedHash)
}

func (s *SQLTokenStore) decryptToken(ciphertext, nonce, storedHash []byte) (string, error) {
	if len(nonce) != s.aead.NonceSize() {
		return "", errors.New("stored push token nonce has an invalid size")
	}
	plain, err := s.aead.Open(nil, nonce, ciphertext, []byte("workspace-push-proxy-token-v1"))
	if err != nil {
		return "", fmt.Errorf("decrypt push token: %w", err)
	}
	hash := sha256.Sum256(plain)
	if subtle.ConstantTimeCompare(hash[:], storedHash) != 1 {
		return "", errors.New("push token hash does not match ciphertext")
	}
	return string(plain), nil
}

func (s *SQLTokenStore) Verify(ctx context.Context, serverID, token string) (bool, error) {
	if !model.IsValidId(serverID) || token == "" {
		return false, nil
	}
	var currentHash, previousHash []byte
	if err := s.db.QueryRowContext(ctx, `SELECT CurrentHash, PreviousHash
		FROM WorkspacePushProxyTokens WHERE Id=1`).Scan(&currentHash, &previousHash); err != nil {
		return false, err
	}
	hash := sha256.Sum256([]byte(token))
	currentMatch := subtle.ConstantTimeCompare(hash[:], currentHash)
	previousMatch := subtle.ConstantTimeCompare(hash[:], previousHash)
	return currentMatch|previousMatch == 1, nil
}

func (s *SQLTokenStore) ConfirmRotation(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ciphertext, nonce, currentHash, previousHash []byte
	if err = tx.QueryRowContext(ctx, `SELECT Ciphertext, Nonce, CurrentHash, PreviousHash
		FROM WorkspacePushProxyTokens WHERE Id=1 FOR UPDATE`).
		Scan(&ciphertext, &nonce, &currentHash, &previousHash); err != nil {
		return err
	}
	if len(currentHash) == 0 {
		return errNoRotationPending
	}
	if _, err = s.decryptToken(ciphertext, nonce, currentHash); err != nil {
		return err
	}
	if len(previousHash) == 0 {
		return errNoRotationPending
	}
	if _, err = tx.ExecContext(ctx, `UPDATE WorkspacePushProxyTokens SET PreviousHash=NULL WHERE Id=1`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLTokenStore) Delete(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ciphertext, nonce, currentHash []byte
	if err = tx.QueryRowContext(ctx, `SELECT Ciphertext, Nonce, CurrentHash
		FROM WorkspacePushProxyTokens WHERE Id=1 FOR UPDATE`).
		Scan(&ciphertext, &nonce, &currentHash); err != nil {
		return err
	}
	if len(currentHash) != 0 {
		if _, err = s.decryptToken(ciphertext, nonce, currentHash); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE WorkspacePushProxyTokens
		SET Ciphertext=NULL, Nonce=NULL, CurrentHash=NULL, PreviousHash=NULL WHERE Id=1`); err != nil {
		return err
	}
	return tx.Commit()
}
