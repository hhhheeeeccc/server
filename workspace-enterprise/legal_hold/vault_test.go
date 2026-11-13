package legal_hold

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func openTestVault(t *testing.T) (*LocalVault, string) {
	t.Helper()
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	vault, err := OpenLocalVault(directory)
	require.NoError(t, err)
	t.Cleanup(func() { _ = vault.Close() })
	return vault, directory
}

func readVaultBlob(t *testing.T, vault *LocalVault, ref BlobRef) []byte {
	t.Helper()
	file, err := vault.OpenVerified(ref)
	require.NoError(t, err)
	defer file.Close()
	data, err := io.ReadAll(file)
	require.NoError(t, err)
	return data
}

func TestVaultPreservesExactBytesAcrossReopenAndDeduplicates(t *testing.T) {
	vault, directory := openTestVault(t)
	for _, data := range [][]byte{{}, []byte("نص مرفق عربي\x00"), bytes.Repeat([]byte("large binary attachment\x00"), 150000)} {
		ref, err := vault.Put(context.Background(), bytes.NewReader(data), int64(len(data)))
		require.NoError(t, err)
		hash := sha256.Sum256(data)
		require.Equal(t, hex.EncodeToString(hash[:]), ref.SHA256)
		require.EqualValues(t, len(data), ref.Size)
		require.Equal(t, data, readVaultBlob(t, vault, ref))
		path, err := blobPath(ref)
		require.NoError(t, err)
		first, err := os.Stat(filepath.Join(directory, path))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o400), first.Mode().Perm())
		again, err := vault.Put(context.Background(), bytes.NewReader(data), int64(len(data)))
		require.NoError(t, err)
		require.Equal(t, ref, again)
		second, err := os.Stat(filepath.Join(directory, path))
		require.NoError(t, err)
		require.True(t, os.SameFile(first, second), "deduplication must never replace a committed object")
		reopened, err := OpenLocalVault(directory)
		require.NoError(t, err)
		require.Equal(t, data, readVaultBlob(t, reopened, ref))
		require.NoError(t, reopened.Close())
	}
	staging, err := os.ReadDir(filepath.Join(directory, "staging"))
	require.NoError(t, err)
	require.Empty(t, staging)
}

type failingVaultSource struct{ sent bool }

func (r *failingVaultSource) Read(data []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(data, []byte("partial")), nil
	}
	return 0, errors.New("source storage disconnected")
}

func TestVaultRejectsPartialCanceledAndSizeChangedCopies(t *testing.T) {
	for _, mode := range []string{"short", "long", "read-failure", "canceled", "cancel-after-final-byte"} {
		t.Run(mode, func(t *testing.T) {
			vault, directory := openTestVault(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var source io.Reader = strings.NewReader("correct bytes")
			size := int64(len("correct bytes"))
			switch mode {
			case "short":
				size++
			case "long":
				size--
			case "read-failure":
				source = &failingVaultSource{}
			case "canceled":
				cancel()
			case "cancel-after-final-byte":
				source = cancelAfterRead{Reader: source, cancel: cancel}
			}
			ref, err := vault.Put(ctx, source, size)
			require.Error(t, err)
			require.Empty(t, ref)
			if strings.HasPrefix(mode, "cancel") {
				require.ErrorIs(t, err, context.Canceled)
			}
			require.NoError(t, filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
				require.NoError(t, err)
				require.True(t, entry.IsDir(), "failed copies must publish neither objects nor staging files: %s", path)
				return nil
			}))
		})
	}
}

type cancelAfterRead struct {
	io.Reader
	cancel context.CancelFunc
}

func (r cancelAfterRead) Read(data []byte) (int, error) {
	n, err := r.Reader.Read(data)
	r.cancel()
	return n, err
}

func TestVaultConcurrentPublicationAndCorruptionDetection(t *testing.T) {
	vault, directory := openTestVault(t)
	data := bytes.Repeat([]byte("same evidence\x00"), 10000)
	var group sync.WaitGroup
	results := make(chan error, 8)
	refs := make(chan BlobRef, 8)
	for range 8 {
		group.Go(func() {
			ref, err := vault.Put(context.Background(), bytes.NewReader(data), int64(len(data)))
			results <- err
			refs <- ref
		})
	}
	group.Wait()
	close(results)
	close(refs)
	for err := range results {
		require.NoError(t, err)
	}
	var ref BlobRef
	for next := range refs {
		if ref.SHA256 != "" {
			require.Equal(t, ref, next)
		}
		ref = next
	}
	path, err := blobPath(ref)
	require.NoError(t, err)
	// Simulate modification by a privileged filesystem operator. Ordinary vault
	// callers have no overwrite API. Same-length corruption must still fail.
	fullPath := filepath.Join(directory, path)
	require.NoError(t, os.Chmod(fullPath, 0o600))
	data[0] ^= 1
	require.NoError(t, os.WriteFile(fullPath, data, 0o400))
	_, err = vault.OpenVerified(ref)
	require.ErrorIs(t, err, ErrIntegrity)
	data[0] ^= 1
	_, err = vault.Put(context.Background(), bytes.NewReader(data), int64(len(data)))
	require.ErrorIs(t, err, ErrIntegrity, "a corrupt existing hash path must not be overwritten or silently accepted")
}

func TestVaultRejectsOverlappingRootsReferencesAndSymlinkEscapes(t *testing.T) {
	vault, directory := openTestVault(t)
	for _, ref := range []BlobRef{{SHA256: "../../outside", Size: 1}, {SHA256: strings.Repeat("G", 64)}, {SHA256: strings.Repeat("a", 64), Size: -1}} {
		_, err := vault.OpenVerified(ref)
		require.ErrorIs(t, err, ErrIntegrity)
	}
	live := t.TempDir()
	child := filepath.Join(live, "archive")
	require.NoError(t, os.Mkdir(child, 0o700))
	_, err := OpenLocalVault(child, live)
	require.ErrorContains(t, err, "separate")
	_, err = OpenLocalVault(live, child)
	require.ErrorContains(t, err, "separate")
	_, err = OpenLocalVault(live, live)
	require.ErrorContains(t, err, "separate")
	alias := filepath.Join(t.TempDir(), "live-alias")
	require.NoError(t, os.Symlink(live, alias))
	_, err = OpenLocalVault(alias, live)
	require.ErrorContains(t, err, "separate")
	public := t.TempDir()
	require.NoError(t, os.Chmod(public, 0o755))
	_, err = OpenLocalVault(public)
	require.ErrorContains(t, err, "private")
	data := []byte("symlink fixture")
	hash := sha256.Sum256(data)
	ref := BlobRef{SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data))}
	path, err := blobPath(ref)
	require.NoError(t, err)
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(directory, filepath.Dir(path))))
	_, err = vault.Put(context.Background(), bytes.NewReader(data), int64(len(data)))
	require.Error(t, err)
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Empty(t, entries, "archive operations must never follow a shard symlink outside the root")
}
