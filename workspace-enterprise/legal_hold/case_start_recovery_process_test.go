package legal_hold

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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

func init() {
	if phase := os.Getenv("WORKSPACE_LEGAL_START_CHILD_PHASE"); phase != "" {
		if err := runCaseStartRecoveryChild(phase); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

// This explicit crash fixture exercises the private SQL phase. The parent
// authenticates all public calls; no production entry point skips permission
// checks. A separate process owns both the archive inode and transaction.
func runCaseStartRecoveryChild(phase string) error {
	if phase != "staged" && phase != "committed" {
		return fmt.Errorf("unexpected case start crash phase %q", phase)
	}
	ctx := context.Background()
	db, err := sql.Open("postgres", os.Getenv("WORKSPACE_LEGAL_START_CHILD_DSN"))
	if err != nil {
		return err
	}
	defer db.Close()
	fence, err := InstallFileFence(ctx, db)
	if err != nil {
		return err
	}
	vault, err := OpenLocalVault(os.Getenv("WORKSPACE_LEGAL_START_CHILD_ARCHIVE"))
	if err != nil {
		return err
	}
	defer vault.Close()
	source, err := filestore.NewFileBackend(filestore.FileBackendSettings{DriverName: model.ImageDriverLocal, Directory: os.Getenv("WORKSPACE_LEGAL_START_CHILD_LIVE")})
	if err != nil {
		return err
	}
	c, err := NewFileCoordinator(ctx, fence, vault, source)
	if err != nil {
		return err
	}
	journal, err := InstallCaseJournal(ctx, c)
	if err != nil {
		return err
	}
	var input CaseRequest
	if err = json.Unmarshal([]byte(os.Getenv("WORKSPACE_LEGAL_START_CHILD_REQUEST")), &input); err != nil {
		return err
	}
	lease, err := vault.mutationLease(ctx)
	if err != nil {
		return err
	}
	defer lease.Close()
	tx, err := fence.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	m := &CaseManager{journal: journal}
	status, err := m.prepareCaptureTx(ctx, tx, input, os.Getenv("WORKSPACE_LEGAL_START_CHILD_ACTOR"), os.Getenv("WORKSPACE_LEGAL_START_CHILD_KEY"))
	if err != nil {
		return err
	}
	if phase == "committed" {
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	fmt.Println("start-" + phase + ":" + status.ID)
	_, err = io.Copy(io.Discard, os.Stdin)
	return err
}

func TestCaseStartRecoveryAfterActualProcessDeathAroundCommit(t *testing.T) {
	for _, phase := range []string{"staged", "committed"} {
		t.Run(phase, func(t *testing.T) {
			th, c, m, rctx, archive := caseFixture(t)
			require.NoError(t, InstallCaseStartJournal(context.Background(), m.journal))
			data := bytes.Repeat([]byte("case start survives process death\x00"), 15000)
			post, info := uploadFixtureFile(t, th, data)
			input := CaseRequest{Title: "طلب بدء بعد الانقطاع", Scopes: []CaseScope{{Kind: "channels", TargetID: th.BasicChannel.Id}}}
			payload, err := json.Marshal(input)
			require.NoError(t, err)
			key := model.NewId()
			executable, err := os.Executable()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable)
			command.Env = append(os.Environ(),
				"WORKSPACE_LEGAL_START_CHILD_PHASE="+phase,
				"WORKSPACE_LEGAL_START_CHILD_DSN="+*th.App.Config().SqlSettings.DataSource,
				"WORKSPACE_LEGAL_START_CHILD_ARCHIVE="+archive,
				"WORKSPACE_LEGAL_START_CHILD_LIVE="+*th.App.Config().FileSettings.Directory,
				"WORKSPACE_LEGAL_START_CHILD_REQUEST="+string(payload),
				"WORKSPACE_LEGAL_START_CHILD_ACTOR="+rctx.Session().UserId,
				"WORKSPACE_LEGAL_START_CHILD_KEY="+key)
			stdin, err := command.StdinPipe()
			require.NoError(t, err)
			defer stdin.Close()
			stdout, err := command.StdoutPipe()
			require.NoError(t, err)
			stderr, err := os.Create(filepath.Join(t.TempDir(), "case-start-stderr.log"))
			require.NoError(t, err)
			defer stderr.Close()
			command.Stderr = stderr
			require.NoError(t, command.Start())
			defer func() { _ = command.Process.Kill() }()
			scanner := bufio.NewScanner(stdout)
			if !scanner.Scan() {
				_ = command.Wait()
				output, _ := os.ReadFile(stderr.Name())
				t.Fatalf("case start child did not reach %s: %s", phase, output)
			}
			marker := strings.Split(scanner.Text(), ":")
			require.Len(t, marker, 2)
			require.Equal(t, "start-"+phase, marker[0])
			require.True(t, model.IsValidId(marker[1]))
			stagedID := marker[1]
			cases, keys := caseStartCounts(t, c.fence.db)
			expected := 0
			if phase == "committed" {
				expected = 1
			}
			require.Equal(t, expected, cases)
			require.Equal(t, expected, keys, "case and request identity must become visible together")
			waiting, stop := context.WithTimeout(ctx, 120*time.Millisecond)
			_, err = m.StartCaptureWithKey(rctx.WithContext(waiting), input, key)
			stop()
			require.ErrorIs(t, err, context.DeadlineExceeded, "a live independent writer owns the archive lease")
			require.NoError(t, command.Process.Kill())
			require.Error(t, command.Wait(), "the original case start process must actually die")
			output, err := os.ReadFile(stderr.Name())
			require.NoError(t, err)
			require.NotContains(t, string(output), "WARNING: DATA RACE")
			post.Message = "native change after the case start process died"
			_, _, err = th.Client.UpdatePost(context.Background(), post.Id, post)
			require.NoError(t, err)
			if phase == "committed" {
				// Native storage removal must seal pending bytes even though
				// the process died before activation and acknowledgement.
				require.NoError(t, th.Server.FileBackend().RemoveFile(info.Path))
			}
			successor, err := NewCaseManager(th.App, m.journal)
			require.NoError(t, err)
			recovered, err := successor.StartCaptureWithKey(rctx, input, key)
			require.NoError(t, err)
			require.Equal(t, "active", recovered.State)
			if phase == "committed" {
				require.Equal(t, stagedID, recovered.ID)
				require.Contains(t, caseMessageVersions(t, c.fence.db, recovered.ID, "posts", post.Id), "رسالة للحفظ")
			} else {
				require.NotEqual(t, stagedID, recovered.ID, "the uncommitted identity must roll back")
			}
			require.Contains(t, caseMessageVersions(t, c.fence.db, recovered.ID, "posts", post.Id), post.Message)
			cases, keys = caseStartCounts(t, c.fence.db)
			require.Equal(t, 1, cases)
			require.Equal(t, 1, keys)
			replayed, err := successor.StartCaptureWithKey(rctx, input, key)
			require.NoError(t, err)
			require.Equal(t, recovered, replayed)
			exporter, _ := exportFixture(t, successor, 16<<20)
			result, err := exporter.Export(rctx, recovered.ID)
			require.NoError(t, err)
			file, _, err := exporter.Open(rctx, result.ID)
			require.NoError(t, err)
			entries := readExportEntries(t, file)
			require.NoError(t, file.Close())
			found := false
			for name, content := range entries {
				if strings.HasPrefix(name, "objects/sha256/") && bytes.Equal(content, data) {
					found = true
				}
			}
			require.True(t, found, "real attachment bytes must survive start recovery")
			t.Logf("start recovery phase=%s killed_pid=%d recovery_pid=%d; one case and request key retained, native changes and attachment bytes verified", phase, command.Process.Pid, os.Getpid())
		})
	}
}
