package legal_hold

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
	"github.com/stretchr/testify/require"
)

func coordinatorChildBytes() []byte { return bytes.Repeat([]byte("successor evidence\x00"), 10000) }

type childPhysicalWriter struct{ filestore.FileBackend }

func (b *childPhysicalWriter) AppendFile(reader io.Reader, path string) (int64, error) {
	written, err := b.FileBackend.AppendFile(reader, path)
	if err != nil {
		return written, err
	}
	fmt.Println("physical-generation-written")
	_, err = io.Copy(io.Discard, os.Stdin)
	return written, err
}

// Called directly by TestMain in a subprocess, not counted as an empty test.
func coordinatorChildMain() int {
	ctx := context.Background()
	db, err := sql.Open("postgres", os.Getenv("WORKSPACE_LEGAL_HOLD_CHILD_DSN"))
	if err != nil {
		return 2
	}
	defer db.Close()
	fence, err := InstallFileFence(ctx, db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "child fence setup failed")
		return 3
	}
	vault, err := OpenLocalVault(os.Getenv("WORKSPACE_LEGAL_HOLD_CHILD_ARCHIVE"))
	if err != nil {
		return 4
	}
	defer vault.Close()
	raw, err := filestore.NewFileBackend(filestore.FileBackendSettings{DriverName: model.ImageDriverLocal, Directory: os.Getenv("WORKSPACE_LEGAL_HOLD_CHILD_LIVE")})
	if err != nil {
		return 5
	}
	source := &childPhysicalWriter{FileBackend: raw}
	coordinator, err := NewFileCoordinator(ctx, fence, vault, source)
	if err != nil {
		fmt.Fprintln(os.Stderr, "child coordinator setup failed")
		return 6
	}
	wrapped, err := GuardBackend(source, coordinator)
	if err != nil {
		return 7
	}
	_, err = wrapped.AppendFile(bytes.NewReader(coordinatorChildBytes()), os.Getenv("WORKSPACE_LEGAL_HOLD_CHILD_PATH"))
	if err != nil {
		return 8
	}
	return 0
}

func TestCoordinatorRecoversPhysicalGenerationAfterActualProcessKill(t *testing.T) {
	th, c, archive := coordinatorFixture(t, nil)
	original := []byte("before process death")
	_, info := uploadFixtureFile(t, th, original)
	_, err := c.ReserveFile(context.Background(), info.Id)
	require.NoError(t, err)
	before, err := os.Stat(filepath.Join(archive, mutationLockPath))
	require.NoError(t, err)
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable)
	command.Env = append(os.Environ(),
		"WORKSPACE_LEGAL_HOLD_CHILD=1",
		"WORKSPACE_LEGAL_HOLD_CHILD_DSN="+*th.App.Config().SqlSettings.DataSource,
		"WORKSPACE_LEGAL_HOLD_CHILD_ARCHIVE="+archive,
		"WORKSPACE_LEGAL_HOLD_CHILD_LIVE="+*th.App.Config().FileSettings.Directory,
		"WORKSPACE_LEGAL_HOLD_CHILD_PATH="+info.Path)
	stdin, err := command.StdinPipe()
	require.NoError(t, err)
	defer stdin.Close()
	stdout, err := command.StdoutPipe()
	require.NoError(t, err)
	stderr, err := os.Create(filepath.Join(t.TempDir(), "writer-stderr.log"))
	require.NoError(t, err)
	defer stderr.Close()
	command.Stderr = stderr
	require.NoError(t, command.Start())
	defer func() { _ = command.Process.Kill() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		_ = command.Wait()
		diagnostic, _ := os.ReadFile(stderr.Name())
		t.Fatalf("child did not enter the physical write: %s", diagnostic)
	}
	require.Equal(t, "physical-generation-written", scanner.Text())
	_, err = c.fence.db.ExecContext(ctx, `DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.ErrorContains(t, err, "physical mutation is pending")
	waitCtx, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	defer stop()
	_, err = NewFileCoordinator(waitCtx, c.fence, c.vault, c.source)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, command.Process.Kill())
	require.Error(t, command.Wait(), "the writer must actually die before recovery")
	diagnostic, err := os.ReadFile(stderr.Name())
	require.NoError(t, err)
	require.False(t, strings.Contains(string(diagnostic), "WARNING: DATA RACE"), "child race detector output: %s", diagnostic)
	successor, err := NewFileCoordinator(ctx, c.fence, c.vault, c.source)
	require.NoError(t, err)
	requireMutationBytes(t, successor, info.Id, "before", info.Path, original)
	expected := append(append([]byte(nil), original...), coordinatorChildBytes()...)
	requireMutationBytes(t, successor, info.Id, "recovery", info.Path, expected)
	live, err := c.source.ReadFile(info.Path)
	require.NoError(t, err)
	require.Equal(t, expected, live, "recovery must not append the payload a second time")
	after, err := os.Stat(filepath.Join(archive, mutationLockPath))
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	t.Logf("legal hold physical generation after SIGKILL: writer_pid=%d recovery_pid=%d; original and appended bytes retained without replay", command.Process.Pid, os.Getpid())
}
