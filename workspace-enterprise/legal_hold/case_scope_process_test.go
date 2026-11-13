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
	if phase := os.Getenv("WORKSPACE_LEGAL_SCOPE_CHILD_PHASE"); phase != "" {
		if err := runCaseScopeChild(phase); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

// Only this explicitly selected crash fixture invokes the private transaction
// phases. Public calls in the parent still use a real licensed administrator.
func runCaseScopeChild(phase string) error {
	if phase != "staged" && phase != "prepared" && phase != "completion-staged" && phase != "complete" {
		return fmt.Errorf("unexpected scope crash phase %q", phase)
	}
	ctx := context.Background()
	db, err := sql.Open("postgres", os.Getenv("WORKSPACE_LEGAL_SCOPE_CHILD_DSN"))
	if err != nil {
		return err
	}
	defer db.Close()
	fence, err := InstallFileFence(ctx, db)
	if err != nil {
		return err
	}
	vault, err := OpenLocalVault(os.Getenv("WORKSPACE_LEGAL_SCOPE_CHILD_ARCHIVE"))
	if err != nil {
		return err
	}
	defer vault.Close()
	source, err := filestore.NewFileBackend(filestore.FileBackendSettings{DriverName: model.ImageDriverLocal, Directory: os.Getenv("WORKSPACE_LEGAL_SCOPE_CHILD_LIVE")})
	if err != nil {
		return err
	}
	c, err := NewFileCoordinator(ctx, fence, vault, source)
	if err != nil {
		return err
	}
	j, err := InstallCaseJournal(ctx, c)
	if err != nil {
		return err
	}
	if err = InstallCaseScopeJournal(ctx, j); err != nil {
		return err
	}
	var input CaseScopeChangeRequest
	if err = json.Unmarshal([]byte(os.Getenv("WORKSPACE_LEGAL_SCOPE_CHILD_REQUEST")), &input); err != nil {
		return err
	}
	input, payload, err := normalizeScopeChange(input)
	if err != nil {
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
	m := &CaseManager{journal: j}
	actor := os.Getenv("WORKSPACE_LEGAL_SCOPE_CHILD_ACTOR")
	status, err := m.prepareScopeChangeTx(ctx, tx, actor, input, payload)
	if err != nil {
		return err
	}
	if phase != "staged" {
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	if phase == "completion-staged" || phase == "complete" {
		completion, err := fence.begin(ctx)
		if err != nil {
			return err
		}
		defer completion.Rollback()
		if err = m.completeScopeChangeTx(ctx, completion, actor, status); err != nil {
			return err
		}
		if phase == "complete" {
			if err = completion.Commit(); err != nil {
				return err
			}
		}
	}
	fmt.Println("scope-" + phase + ":" + status.ID)
	_, err = io.Copy(io.Discard, os.Stdin)
	return err
}

func TestCaseScopeChangeAfterActualProcessDeathAtBothCommits(t *testing.T) {
	for _, phase := range []string{"staged", "prepared", "completion-staged", "complete"} {
		t.Run(phase, func(t *testing.T) {
			th, c, m, rctx, archive := caseFixture(t)
			require.NoError(t, InstallCaseScopeJournal(context.Background(), m.journal))
			keep := th.CreatePublicChannel(t)
			status := startChannelCase(t, m, rctx, keep.Id)
			original := bytes.Repeat([]byte("scope evidence survives process death\x00"), 9000)
			post, info := uploadFixtureFile(t, th, original)
			input := scopeChange(status.ID, []CaseScope{{"channels", post.ChannelId}}, []CaseScope{{"channels", keep.Id}})
			payload, err := json.Marshal(input)
			require.NoError(t, err)
			executable, err := os.Executable()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable)
			command.Env = append(os.Environ(),
				"WORKSPACE_LEGAL_SCOPE_CHILD_PHASE="+phase,
				"WORKSPACE_LEGAL_SCOPE_CHILD_DSN="+*th.App.Config().SqlSettings.DataSource,
				"WORKSPACE_LEGAL_SCOPE_CHILD_ARCHIVE="+archive,
				"WORKSPACE_LEGAL_SCOPE_CHILD_LIVE="+*th.App.Config().FileSettings.Directory,
				"WORKSPACE_LEGAL_SCOPE_CHILD_REQUEST="+string(payload),
				"WORKSPACE_LEGAL_SCOPE_CHILD_ACTOR="+rctx.Session().UserId)
			stdin, err := command.StdinPipe()
			require.NoError(t, err)
			defer stdin.Close()
			stdout, err := command.StdoutPipe()
			require.NoError(t, err)
			stderr, err := os.Create(filepath.Join(t.TempDir(), "scope-child-stderr.log"))
			require.NoError(t, err)
			defer stderr.Close()
			command.Stderr = stderr
			require.NoError(t, command.Start())
			defer func() { _ = command.Process.Kill() }()
			scanner := bufio.NewScanner(stdout)
			if !scanner.Scan() {
				_ = command.Wait()
				output, _ := os.ReadFile(stderr.Name())
				t.Fatalf("scope child did not reach %s: %s", phase, output)
			}
			marker := strings.Split(scanner.Text(), ":")
			require.Len(t, marker, 2)
			require.Equal(t, "scope-"+phase, marker[0])
			require.True(t, model.IsValidId(marker[1]))
			waiting, stop := context.WithTimeout(ctx, 120*time.Millisecond)
			_, err = m.ChangeScope(rctx.WithContext(waiting), input)
			stop()
			require.ErrorIs(t, err, context.DeadlineExceeded, "the independent writer still owns its archive lease")
			require.NoError(t, command.Process.Kill())
			require.Error(t, command.Wait(), "the original process must actually die")
			output, err := os.ReadFile(stderr.Name())
			require.NoError(t, err)
			require.NotContains(t, string(output), "WARNING: DATA RACE")
			var requests, completions int
			require.NoError(t, c.fence.db.QueryRow(`SELECT (SELECT count(*) FROM workspace_legal_hold.case_scope_requests),(SELECT count(*) FROM workspace_legal_hold.case_scope_completions)`).Scan(&requests, &completions))
			if phase == "staged" {
				require.Zero(t, requests)
			} else {
				require.Equal(t, 1, requests)
			}
			if phase == "complete" {
				require.Equal(t, 1, completions)
			} else {
				require.Zero(t, completions)
			}
			_, err = c.fence.db.Exec(`UPDATE posts SET message='native edit after scope writer died' WHERE id=$1`, post.Id)
			require.NoError(t, err)
			if phase != "staged" {
				require.NoError(t, th.Server.FileBackend().RemoveFile(info.Path), "committed addition must preserve bytes even before completion")
			}
			successor, err := NewCaseManager(th.App, m.journal)
			require.NoError(t, err)
			recovered, err := successor.ChangeScope(rctx, input)
			require.NoError(t, err)
			require.Equal(t, "complete", recovered.State)
			if phase == "staged" {
				require.NotEqual(t, marker[1], recovered.ID)
			} else {
				require.Equal(t, marker[1], recovered.ID)
			}
			if phase != "staged" {
				require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id), "رسالة للحفظ")
			}
			require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id), "native edit after scope writer died")
			evidence, err := c.fence.LatestFileEvidence(ctx, info.Id)
			require.NoError(t, err)
			require.Equal(t, original, readVaultBlob(t, c.vault, evidence.Objects[0].BlobRef))
			details, err := m.Get(rctx, status.ID)
			require.NoError(t, err)
			require.Equal(t, []CaseScope{{"channels", post.ChannelId}}, details.Scopes)
			t.Logf("SIGKILL at %s: recovered the committed request and protected bytes without duplicate scope change", phase)
		})
	}
}
