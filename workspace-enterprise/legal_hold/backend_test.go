package legal_hold

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
	"github.com/stretchr/testify/require"
)

// This fixture supplies an in-process lease and test-selected paths. It is not
// a production case policy, durable receipt journal or a multi-node SQL lock.
type fixturePreservationGuard struct {
	mu        sync.Mutex
	vault     *LocalVault
	held      map[string]bool
	refs      map[string][]BlobRef
	mutations []Mutation
	fault     error
}

func newFixtureGuard(vault *LocalVault) *fixturePreservationGuard {
	return &fixturePreservationGuard{vault: vault, held: make(map[string]bool), refs: make(map[string][]BlobRef)}
}

func (g *fixturePreservationGuard) Apply(ctx context.Context, backend filestore.FileBackend, mutation Mutation, mutate func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mutations = append(g.mutations, mutation)
	for held := range g.held {
		selected := false
		for _, target := range mutation.Paths {
			target = path.Clean(target)
			selected = selected || held == target || mutation.Recursive && (target == "." || strings.HasPrefix(held, target+"/"))
		}
		if !selected {
			continue
		}
		if g.fault != nil {
			return g.fault
		}
		exists, err := backend.FileExists(held)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		size, err := backend.FileSize(held)
		if err != nil {
			return err
		}
		reader, err := backend.Reader(held)
		if err != nil {
			return err
		}
		ref, archiveErr := g.vault.Put(ctx, reader, size)
		if err := errors.Join(archiveErr, reader.Close()); err != nil {
			return err
		}
		g.refs[held] = append(g.refs[held], ref)
	}
	return mutate()
}

func TestGuardPreservesEachDestructiveBackendOperation(t *testing.T) {
	for _, operation := range []string{"remove", "write", "write-context", "append", "copy", "move", "remove-directory"} {
		t.Run(operation, func(t *testing.T) {
			vault, _ := openTestVault(t)
			backend, err := filestore.NewFileBackend(filestore.FileBackendSettings{DriverName: model.ImageDriverLocal, Directory: t.TempDir()})
			require.NoError(t, err)
			guard := newFixtureGuard(vault)
			wrapped, err := GuardBackend(backend, guard)
			require.NoError(t, err)
			original := []byte("held original\x00")
			target := "tree/target.bin"
			_, err = backend.WriteFile(bytes.NewReader(original), target)
			require.NoError(t, err)
			guard.held[target] = true
			_, err = backend.WriteFile(strings.NewReader("replacement"), "tree/source.bin")
			require.NoError(t, err)
			if operation == "move" {
				guard.held["tree/source.bin"] = true
			}
			if operation == "remove-directory" {
				deep := path.Join("tree", strings.Repeat("nested/", 30), "deep.bin")
				_, err = backend.WriteFile(strings.NewReader("deep evidence"), deep)
				require.NoError(t, err)
				guard.held[deep] = true
			}
			switch operation {
			case "remove":
				err = wrapped.RemoveFile(target)
			case "write":
				_, err = wrapped.WriteFile(strings.NewReader("replacement"), target)
			case "write-context":
				_, err = filestore.TryWriteFileContext(context.Background(), wrapped, strings.NewReader("replacement"), target)
			case "append":
				_, err = wrapped.AppendFile(strings.NewReader("extra"), target)
			case "copy":
				err = wrapped.CopyFile("tree/source.bin", target)
			case "move":
				err = wrapped.MoveFile("tree/source.bin", target)
			case "remove-directory":
				err = wrapped.RemoveDirectory("tree")
			}
			require.NoError(t, err)
			require.Len(t, guard.refs[target], 1)
			require.Equal(t, original, readVaultBlob(t, vault, guard.refs[target][0]))
			if operation == "move" {
				require.Equal(t, []byte("replacement"), readVaultBlob(t, vault, guard.refs["tree/source.bin"][0]))
			}
			if operation == "remove-directory" {
				for name := range guard.held {
					require.Len(t, guard.refs[name], 1)
				}
			}
		})
	}
}

func TestGuardBlocksMutationOnActualVaultFailureAndPreservesContext(t *testing.T) {
	vault, _ := openTestVault(t)
	backend, err := filestore.NewFileBackend(filestore.FileBackendSettings{DriverName: model.ImageDriverLocal, Directory: t.TempDir()})
	require.NoError(t, err)
	_, err = backend.WriteFile(strings.NewReader("keep"), "held")
	require.NoError(t, err)
	guard := newFixtureGuard(vault)
	guard.held["held"] = true
	wrapped, err := GuardBackend(backend, guard)
	require.NoError(t, err)
	require.NoError(t, vault.Close())
	for _, operation := range []func() error{
		func() error { return wrapped.RemoveFile("held") },
		func() error { _, err := wrapped.WriteFile(strings.NewReader("lost"), "held"); return err },
		func() error { return wrapped.MoveFile("held", "moved") },
		func() error { return wrapped.RemoveDirectory(".") },
	} {
		require.Error(t, operation())
		data, err := backend.ReadFile("held")
		require.NoError(t, err)
		require.Equal(t, []byte("keep"), data)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = filestore.TryWriteFileContext(ctx, wrapped, strings.NewReader("new"), "unheld")
	require.ErrorIs(t, err, context.Canceled)
	exists, err := backend.FileExists("unheld")
	require.NoError(t, err)
	require.False(t, exists)
}

type optionalBackend struct {
	filestore.FileBackend
	contextSeen context.Context
}

func (b *optionalBackend) GeneratePublicLink(path string) (string, time.Duration, error) {
	return "https://example.invalid/" + path, time.Minute, nil
}

func (b *optionalBackend) WriteFileContext(ctx context.Context, src io.Reader, path string) (int64, error) {
	b.contextSeen = ctx
	return b.WriteFile(src, path)
}

func TestGuardPreservesOptionalBackendContracts(t *testing.T) {
	vault, _ := openTestVault(t)
	backend, err := filestore.NewFileBackend(filestore.FileBackendSettings{DriverName: model.ImageDriverLocal, Directory: t.TempDir()})
	require.NoError(t, err)
	guard := newFixtureGuard(vault)
	plain, err := GuardBackend(backend, guard)
	require.NoError(t, err)
	_, supportsLinks := plain.(filestore.FileBackendWithLinkGenerator)
	require.False(t, supportsLinks)
	optional := &optionalBackend{FileBackend: backend}
	wrapped, err := GuardBackend(optional, guard)
	require.NoError(t, err)
	links, ok := wrapped.(filestore.FileBackendWithLinkGenerator)
	require.True(t, ok)
	link, duration, err := links.GeneratePublicLink("file")
	require.NoError(t, err)
	require.Equal(t, "https://example.invalid/file", link)
	require.Equal(t, time.Minute, duration)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = filestore.TryWriteFileContext(ctx, wrapped, strings.NewReader("body"), "file")
	require.NoError(t, err)
	require.Same(t, ctx, optional.contextSeen)
	require.Equal(t, backend.DriverName(), wrapped.DriverName())
	_, err = GuardBackend(nil, guard)
	require.Error(t, err)
	_, err = GuardBackend(backend, nil)
	require.Error(t, err)
}
