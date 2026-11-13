package legal_hold

import (
	"context"
	"errors"
	"io"
	"reflect"
	"time"

	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/app/platform"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
)

// Mutation identifies every path a backend operation may destroy or replace.
// Recursive removals remain explicit: a policy must select all held paths below
// the directory inside its coordinated lease, not trust a racy directory scan.
type Mutation struct {
	Operation string
	Paths     []string
	Recursive bool
}

// MutationGuard must acquire its preservation lease, durably preserve all
// required bytes/receipts and only then invoke mutate once. Failure must return
// without invoking mutate. The lease must cover both preservation and mutation.
// Case/SQL authorization and cross-node coordination belong to that guard.
type MutationGuard interface {
	Apply(context.Context, filestore.FileBackend, Mutation, func() error) error
}

type guardedBackend struct {
	filestore.FileBackend
	guard MutationGuard
}

// Local connection tests overwrite and remove their test object. Treat this
// administrative call as a recursive mutation too; a backend implementation
// may use a path that also belongs to a reserved file.
func (b *guardedBackend) TestConnection() error {
	return b.guard.Apply(context.Background(), b.FileBackend, Mutation{Operation: "test-connection", Paths: []string{"."}, Recursive: true}, b.FileBackend.TestConnection)
}

// InstallBackendBeforeStart is called only from a server Enterprise constructor,
// before Platform.Start and before Channels captures the backend. The shared
// export alias must be replaced too; otherwise WriteExportFile can mutate the
// very same live objects without passing through the guard. A dedicated export
// backend remains independent. This helper registers no routes or feature.
func InstallBackendBeforeStart(server *app.Server, guard MutationGuard) error {
	source, exports := server.FileBackend(), server.ExportFileBackend()
	switch source.(type) {
	case *guardedBackend, guardedLinkBackend:
		return errors.New("legal hold backend guard already installed")
	}
	wrapped, err := GuardBackend(source, guard)
	if err != nil {
		return err
	}
	if !reflect.TypeOf(source).Comparable() {
		return errors.New("legal hold backend identity cannot be compared safely")
	}
	sharedExport := reflect.TypeOf(source) == reflect.TypeOf(exports) && source == exports
	if err := platform.SetFileStore(wrapped)(server.Platform()); err != nil {
		return err
	}
	if sharedExport {
		return platform.SetExportFileStore(wrapped)(server.Platform())
	}
	return nil
}

// GuardBackend is a startup building block, not a Legal Hold activation API.
// No production constructor uses it until the full case guard is implemented.
// It must be installed before Channels captures its backend, never swapped into
// a running platform. A nil guard is rejected rather than silently bypassed.
func GuardBackend(backend filestore.FileBackend, guard MutationGuard) (filestore.FileBackend, error) {
	if backend == nil || guard == nil {
		return nil, errors.New("legal hold backend and mutation guard are required")
	}
	wrapped := &guardedBackend{FileBackend: backend, guard: guard}
	if links, ok := backend.(filestore.FileBackendWithLinkGenerator); ok {
		return guardedLinkBackend{guardedBackend: wrapped, links: links}, nil
	}
	return wrapped, nil
}

func (b *guardedBackend) RemoveFile(path string) error {
	return b.guard.Apply(context.Background(), b.FileBackend, Mutation{Operation: "remove", Paths: []string{path}}, func() error { return b.FileBackend.RemoveFile(path) })
}

func (b *guardedBackend) RemoveDirectory(path string) error {
	return b.guard.Apply(context.Background(), b.FileBackend, Mutation{Operation: "remove-directory", Paths: []string{path}, Recursive: true}, func() error { return b.FileBackend.RemoveDirectory(path) })
}

func (b *guardedBackend) CopyFile(source, target string) error {
	return b.guard.Apply(context.Background(), b.FileBackend, Mutation{Operation: "copy", Paths: []string{target}}, func() error { return b.FileBackend.CopyFile(source, target) })
}

func (b *guardedBackend) MoveFile(source, target string) error {
	return b.guard.Apply(context.Background(), b.FileBackend, Mutation{Operation: "move", Paths: []string{source, target}}, func() error { return b.FileBackend.MoveFile(source, target) })
}

func (b *guardedBackend) WriteFile(src io.Reader, path string) (int64, error) {
	var written int64
	err := b.guard.Apply(context.Background(), b.FileBackend, Mutation{Operation: "write", Paths: []string{path}}, func() error {
		var err error
		written, err = b.FileBackend.WriteFile(src, path)
		return err
	})
	return written, err
}

func (b *guardedBackend) WriteFileContext(ctx context.Context, src io.Reader, path string) (int64, error) {
	var written int64
	err := b.guard.Apply(ctx, b.FileBackend, Mutation{Operation: "write", Paths: []string{path}}, func() error {
		var err error
		written, err = filestore.TryWriteFileContext(ctx, b.FileBackend, src, path)
		return err
	})
	return written, err
}

func (b *guardedBackend) AppendFile(src io.Reader, path string) (int64, error) {
	var written int64
	err := b.guard.Apply(context.Background(), b.FileBackend, Mutation{Operation: "append", Paths: []string{path}}, func() error {
		var err error
		written, err = b.FileBackend.AppendFile(src, path)
		return err
	})
	return written, err
}

type guardedLinkBackend struct {
	*guardedBackend
	links filestore.FileBackendWithLinkGenerator
}

func (b guardedLinkBackend) GeneratePublicLink(path string) (string, time.Duration, error) {
	return b.links.GeneratePublicLink(path)
}
