// Package legal_hold contains the preservation primitives for Legal Hold.
// They are not registered in the application until the case, SQL and lifecycle
// controls can enforce the complete hold contract.
package legal_hold

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
)

var ErrIntegrity = errors.New("legal hold object integrity check failed")

type BlobRef struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// LocalVault has no overwrite or delete API. The operator must provision an
// existing private archive directory on durable storage, separate from every
// live local file directory passed here. This is application append-only
// storage, not a claim of protection against a filesystem administrator.
type LocalVault struct{ root *os.Root }

func OpenLocalVault(directory string, liveDirectories ...string) (*LocalVault, error) {
	archive, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, fmt.Errorf("open legal hold archive: %w", err)
	}
	archive, err = filepath.Abs(archive)
	if err != nil {
		return nil, err
	}
	for _, live := range liveDirectories {
		source, err := filepath.EvalSymlinks(live)
		if err != nil {
			return nil, fmt.Errorf("validate live file directory: %w", err)
		}
		source, err = filepath.Abs(source)
		if err != nil {
			return nil, err
		}
		if containsDirectory(source, archive) || containsDirectory(archive, source) {
			return nil, errors.New("legal hold archive must be separate from live file directories")
		}
	}
	root, err := os.OpenRoot(archive)
	if err != nil {
		return nil, err
	}
	vault := &LocalVault{root: root}
	ok := false
	defer func() {
		if !ok {
			_ = root.Close()
		}
	}()
	info, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("legal hold archive must have private directory permissions")
	}
	for _, path := range []string{"staging", "objects", "objects/sha256"} {
		if err := root.MkdirAll(path, 0o700); err != nil {
			return nil, err
		}
	}
	for _, path := range []string{"objects/sha256", "objects", "staging", "."} {
		if err := vault.syncDirectory(path); err != nil {
			return nil, err
		}
	}
	ok = true
	return vault, nil
}

func containsDirectory(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func (v *LocalVault) Close() error { return v.root.Close() }

func (v *LocalVault) syncDirectory(path string) error {
	dir, err := v.root.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func blobPath(ref BlobRef) (string, error) {
	if ref.Size < 0 || len(ref.SHA256) != 64 || strings.ToLower(ref.SHA256) != ref.SHA256 {
		return "", ErrIntegrity
	}
	if _, err := hex.DecodeString(ref.SHA256); err != nil {
		return "", ErrIntegrity
	}
	return "objects/sha256/" + ref.SHA256[:2] + "/" + ref.SHA256, nil
}

type vaultContextReader struct {
	ctx context.Context
	src io.Reader
}

func (r vaultContextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.src.Read(data)
}

// Put copies exact bytes to a private temporary file, syncs the bytes and
// publishes with a no-replace hard link, then syncs the directory. An existing
// object must pass full hash/size verification before deduplication succeeds.
// The caller owns interruption of a blocked source Reader; ctx is also checked
// between streamed chunks and immediately before publication.
func (v *LocalVault) Put(ctx context.Context, src io.Reader, expectedSize int64) (BlobRef, error) {
	if err := ctx.Err(); err != nil {
		return BlobRef{}, err
	}
	if expectedSize < 0 || expectedSize == math.MaxInt64 {
		return BlobRef{}, errors.New("invalid legal hold object size")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return BlobRef{}, err
	}
	staging := "staging/" + hex.EncodeToString(random[:])
	file, err := v.root.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return BlobRef{}, err
	}
	defer file.Close()
	defer v.root.Remove(staging)
	hash := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(file, hash), io.LimitReader(vaultContextReader{ctx: ctx, src: src}, expectedSize+1), make([]byte, 64*1024))
	if err != nil {
		return BlobRef{}, err
	}
	if n != expectedSize {
		return BlobRef{}, fmt.Errorf("legal hold object size changed: expected %d, read %d", expectedSize, n)
	}
	if err := file.Chmod(0o400); err != nil {
		return BlobRef{}, err
	}
	if err := file.Sync(); err != nil {
		return BlobRef{}, err
	}
	if err := file.Close(); err != nil {
		return BlobRef{}, err
	}
	ref := BlobRef{SHA256: hex.EncodeToString(hash.Sum(nil)), Size: n}
	path, _ := blobPath(ref)
	if err := v.root.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return BlobRef{}, err
	}
	if err := v.syncDirectory("objects/sha256"); err != nil {
		return BlobRef{}, err
	}
	if err := ctx.Err(); err != nil {
		return BlobRef{}, err
	}
	if err := v.root.Link(staging, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return BlobRef{}, err
		}
		existing, err := v.OpenVerified(ref)
		if err != nil {
			return BlobRef{}, err
		}
		if err := existing.Close(); err != nil {
			return BlobRef{}, err
		}
	}
	if err := v.syncDirectory(filepath.Dir(path)); err != nil {
		return BlobRef{}, err
	}
	return ref, nil
}

// OpenVerified validates all bytes before making the object available to an
// exporter. Reference input can never name an arbitrary archive path.
func (v *LocalVault) OpenVerified(ref BlobRef) (*os.File, error) {
	path, err := blobPath(ref)
	if err != nil {
		return nil, err
	}
	file, err := v.root.Open(path)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != ref.Size {
		return nil, ErrIntegrity
	}
	hash := sha256.New()
	n, err := io.Copy(hash, file)
	if err != nil {
		return nil, err
	}
	if n != ref.Size || hex.EncodeToString(hash.Sum(nil)) != ref.SHA256 {
		return nil, ErrIntegrity
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	ok = true
	return file, nil
}
