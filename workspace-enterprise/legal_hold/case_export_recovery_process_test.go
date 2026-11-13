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

// Dispatch only an explicitly selected helper process, before TestMain. No
// extra empty/skipped test or change to prior TestMain behavior is needed.
func init() {
	if phase := os.Getenv("WORKSPACE_LEGAL_EXPORT_CHILD_PHASE"); phase != "" {
		if err := runExportRecoveryChild(phase); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func runExportRecoveryChild(phase string) error {
	ctx := context.Background()
	db, err := sql.Open("postgres", os.Getenv("WORKSPACE_LEGAL_EXPORT_CHILD_DSN"))
	if err != nil {
		return err
	}
	defer db.Close()
	fence, err := InstallFileFence(ctx, db)
	if err != nil {
		return err
	}
	vault, err := OpenLocalVault(os.Getenv("WORKSPACE_LEGAL_EXPORT_CHILD_ARCHIVE"))
	if err != nil {
		return err
	}
	defer vault.Close()
	source, err := filestore.NewFileBackend(filestore.FileBackendSettings{DriverName: model.ImageDriverLocal, Directory: os.Getenv("WORKSPACE_LEGAL_EXPORT_CHILD_LIVE")})
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
	root, err := os.OpenRoot(os.Getenv("WORKSPACE_LEGAL_EXPORT_CHILD_DIRECTORY"))
	if err != nil {
		return err
	}
	defer root.Close()
	e := &CaseExporter{manager: &CaseManager{journal: journal}, root: root, limit: 16 << 20}
	lease, err := (&LocalVault{root: root}).mutationLease(ctx)
	if err != nil {
		return err
	}
	defer lease.Close()
	if err = e.bindStore(ctx); err != nil {
		return err
	}
	result := CaseExportResult{ID: os.Getenv("WORKSPACE_LEGAL_EXPORT_CHILD_ID")}
	var actor string
	if err = db.QueryRow(`SELECT case_id,actor_id FROM workspace_legal_hold.case_exports WHERE id=$1`, result.ID).Scan(&result.CaseID, &actor); err != nil {
		return err
	}
	attempt := model.NewId()
	if err = e.audit(ctx, `INSERT INTO workspace_legal_hold.case_export_attempts(id,export_id,actor_id) VALUES($1,$2,$3)`, attempt, result.ID, actor); err != nil {
		return err
	}
	work := "staging/" + result.ID
	if err = root.Mkdir(work, 0o700); err != nil {
		return err
	}
	budget := &exportBudget{ctx, e.limit}
	manifest, err := e.snapshotMode(ctx, work, result, actor, budget, true)
	if err != nil {
		return err
	}
	if phase != "snapshot" {
		result.BlobRef, result.ManifestSHA256, err = e.build(ctx, work, manifest, budget)
		if err != nil {
			return err
		}
		if err = e.publish(ctx, work, result.ID); err != nil {
			return err
		}
		if phase == "complete" {
			if err = e.recordOutcome(ctx, result, attempt, true); err != nil {
				return err
			}
		}
	}
	fmt.Println("export-" + phase + "-committed")
	_, err = io.Copy(io.Discard, os.Stdin)
	return err
}

func TestCaseExportRecoveryAfterActualProcessDeathAtDurabilityBoundaries(t *testing.T) {
	for _, phase := range []string{"snapshot", "published", "complete", "corrupt-published"} {
		t.Run(phase, func(t *testing.T) {
			th, c, m, rctx, archive := caseFixture(t)
			data := bytes.Repeat([]byte("survives real exporter death\x00"), 20000)
			_, info := uploadFixtureFile(t, th, data)
			status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
			exporter, directory := exportFixture(t, m, 16<<20)
			request, err := exporter.Request(rctx, status.ID, model.NewId())
			require.NoError(t, err)
			executable, err := os.Executable()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable)
			command.Env = append(os.Environ(),
				"WORKSPACE_LEGAL_EXPORT_CHILD_PHASE="+phase,
				"WORKSPACE_LEGAL_EXPORT_CHILD_DSN="+*th.App.Config().SqlSettings.DataSource,
				"WORKSPACE_LEGAL_EXPORT_CHILD_ARCHIVE="+archive,
				"WORKSPACE_LEGAL_EXPORT_CHILD_LIVE="+*th.App.Config().FileSettings.Directory,
				"WORKSPACE_LEGAL_EXPORT_CHILD_DIRECTORY="+directory,
				"WORKSPACE_LEGAL_EXPORT_CHILD_ID="+request.ID)
			stdin, err := command.StdinPipe()
			require.NoError(t, err)
			defer stdin.Close()
			stdout, err := command.StdoutPipe()
			require.NoError(t, err)
			stderr, err := os.Create(filepath.Join(t.TempDir(), "exporter-stderr.log"))
			require.NoError(t, err)
			defer stderr.Close()
			command.Stderr = stderr
			require.NoError(t, command.Start())
			defer func() { _ = command.Process.Kill() }()
			scanner := bufio.NewScanner(stdout)
			if !scanner.Scan() {
				_ = command.Wait()
				output, _ := os.ReadFile(stderr.Name())
				t.Fatalf("exporter child did not commit %s: %s", phase, output)
			}
			require.Equal(t, "export-"+phase+"-committed", scanner.Text())
			var snapshots int
			require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_export_snapshots WHERE export_id=$1`, request.ID).Scan(&snapshots))
			require.Equal(t, 1, snapshots)
			// The active writer still owns the shared export inode.
			waiting, stop := context.WithTimeout(ctx, 120*time.Millisecond)
			_, err = exporter.Resume(rctx.WithContext(waiting), request.ID)
			stop()
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NoError(t, command.Process.Kill())
			require.Error(t, command.Wait(), "the original exporter process must actually die")
			output, err := os.ReadFile(stderr.Name())
			require.NoError(t, err)
			require.NotContains(t, string(output), "WARNING: DATA RACE")
			post, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "after exporter process death"})
			require.NoError(t, err)
			require.NoError(t, th.Server.FileBackend().RemoveFile(info.Path))
			successor, err := NewCaseExporter(m, directory, 16<<20)
			require.NoError(t, err)
			defer successor.Close()
			var before os.FileInfo
			if phase != "snapshot" {
				before, err = os.Stat(filepath.Join(directory, "packages", request.ID+".zip"))
				require.NoError(t, err)
			}
			if phase == "corrupt-published" {
				name := filepath.Join(directory, "packages", request.ID+".zip")
				require.NoError(t, os.Chmod(name, 0o600))
				file, err := os.OpenFile(name, os.O_WRONLY, 0)
				require.NoError(t, err)
				_, err = file.WriteAt([]byte("X"), 0)
				require.NoError(t, err)
				require.NoError(t, file.Close())
				_, err = successor.Resume(rctx, request.ID)
				require.ErrorIs(t, err, ErrIntegrity)
				after, err := os.Stat(name)
				require.NoError(t, err)
				require.True(t, os.SameFile(before, after))
				var complete int
				require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_export_attempt_outcomes WHERE export_id=$1 AND state='complete'`, request.ID).Scan(&complete))
				require.Zero(t, complete)
				t.Logf("export recovery phase=%s killed_pid=%d recovery_pid=%d; corruption rejected without replacing published inode", phase, command.Process.Pid, os.Getpid())
				return
			}
			result, err := successor.Resume(rctx, request.ID)
			require.NoError(t, err)
			require.Equal(t, request.ID, result.ID)
			file, _, err := successor.Open(rctx, result.ID)
			require.NoError(t, err)
			entries := readExportEntries(t, file)
			require.NoError(t, file.Close())
			require.NotContains(t, string(entries["versions.jsonl"]), post.Id)
			found := false
			for name, content := range entries {
				if strings.HasPrefix(name, "objects/sha256/") && bytes.Equal(content, data) {
					found = true
				}
			}
			require.True(t, found, "snapshot attachment bytes must survive live removal and process death")
			if before != nil {
				after, err := os.Stat(filepath.Join(directory, "packages", request.ID+".zip"))
				require.NoError(t, err)
				require.True(t, os.SameFile(before, after), "recovery must verify the published inode rather than replace it")
			}
			var attempts int
			require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_export_attempts WHERE export_id=$1`, request.ID).Scan(&attempts))
			if phase == "complete" {
				require.Equal(t, 1, attempts)
			} else {
				require.Equal(t, 2, attempts)
			}
			staging, err := os.ReadDir(filepath.Join(directory, "staging"))
			require.NoError(t, err)
			require.Empty(t, staging, "successful recovery must clean the dead process staging directory")
			t.Logf("export recovery phase=%s killed_pid=%d recovery_pid=%d; same request and cutoff retained, published inode never replaced", phase, command.Process.Pid, os.Getpid())
		})
	}
}
