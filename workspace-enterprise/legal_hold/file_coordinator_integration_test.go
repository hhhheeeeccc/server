package legal_hold

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/enterprise/internal/objectstoretest"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
	"github.com/stretchr/testify/require"
)

func coordinatorFixture(t *testing.T, settings *model.FileSettings) (*api4.TestHelper, *FileCoordinator, string) {
	t.Helper()
	live := t.TempDir()
	vault, directory := openTestVault(t)
	var coordinator *FileCoordinator
	key := filepath.Clean(live)
	startupInitializers.Store(key, func(server *app.Server) error {
		fence, err := InstallFileFence(context.Background(), server.Store().GetInternalMasterDB())
		if err != nil {
			return err
		}
		coordinator, err = NewFileCoordinator(context.Background(), fence, vault, server.FileBackend())
		if err != nil {
			return err
		}
		return InstallBackendBeforeStart(server, coordinator)
	})
	t.Cleanup(func() { startupInitializers.Delete(key) })
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		if settings != nil {
			cfg.FileSettings = *settings
			cfg.ServiceSettings.AllowedUntrustedInternalConnections = model.NewPointer("127.0.0.1")
		}
		cfg.FileSettings.Directory = &live
		cfg.FileSettings.ExtractContent = model.NewPointer(false)
		cfg.EmailSettings.SendEmailNotifications = model.NewPointer(false)
		cfg.EmailSettings.SendPushNotifications = model.NewPointer(false)
	}).InitBasic(t)
	t.Cleanup(func() {
		_, err := th.Store.GetInternalMasterDB().Exec(`DROP SCHEMA workspace_legal_hold CASCADE`)
		require.NoError(t, err)
	})
	require.True(t, th.Server.SetLicense(model.NewTestLicense("compliance")))
	require.NotNil(t, coordinator)
	return th, coordinator, directory
}

func mutationObject(t *testing.T, c *FileCoordinator, fileID, phase, value string) (bool, BlobRef) {
	t.Helper()
	var present bool
	var digest sql.NullString
	var size sql.NullInt64
	err := c.fence.db.QueryRow(`SELECT o.present,o.sha256,o.size FROM workspace_legal_hold.mutation_observations o
		JOIN workspace_legal_hold.mutation_files f ON f.intent_id=o.intent_id
		JOIN workspace_legal_hold.mutation_intents i ON i.id=o.intent_id
		WHERE f.file_id=$1 AND o.phase=$2 AND o.path=$3 ORDER BY i.created_at DESC,i.id DESC LIMIT 1`, fileID, phase, value).Scan(&present, &digest, &size)
	require.NoError(t, err)
	return present, BlobRef{SHA256: digest.String, Size: size.Int64}
}

func requireMutationBytes(t *testing.T, c *FileCoordinator, fileID, phase, value string, expected []byte) {
	t.Helper()
	present, ref := mutationObject(t, c, fileID, phase, value)
	require.True(t, present)
	require.Equal(t, expected, readVaultBlob(t, c.vault, ref))
}

func TestCoordinatorNativeDeleteCommitsArchiveEvidenceAcrossSourceBackends(t *testing.T) {
	cases := []objectstoretest.Emulator{{Name: "local"}}
	cases = append(cases, objectstoretest.Cases()...)
	for _, storage := range cases {
		t.Run(storage.Name, func(t *testing.T) {
			var settings *model.FileSettings
			if storage.Setup != nil {
				endpoint := os.Getenv(storage.EndpointEnv)
				if endpoint == "" {
					t.Skip("requires isolated object store emulator")
				}
				configured := storage.Setup(t, endpoint)
				settings = &configured
			}
			th, c, archive := coordinatorFixture(t, settings)
			data := bytes.Repeat([]byte("دليل محفوظ\x00"), 100000)
			post, info := uploadFixtureFile(t, th, data)
			// Reserve alone intentionally leaves the file pending. The real
			// installed coordinator must seal it before native object deletion.
			require.NoError(t, c.fence.ReserveFile(context.Background(), info.Id))
			require.Nil(t, th.App.PermanentDeleteFilesByPost(th.Context, post.Id, nil))
			_, err := th.Store.FileInfo().GetFromMaster(info.Id)
			require.Error(t, err)
			exists, appErr := th.App.FileExists(info.Path)
			require.Nil(t, appErr)
			require.False(t, exists)
			requireMutationBytes(t, c, info.Id, "before", info.Path, data)
			present, _ := mutationObject(t, c, info.Id, "after", info.Path)
			require.False(t, present)
			evidence, err := c.fence.LatestFileEvidence(context.Background(), info.Id)
			require.NoError(t, err)
			require.NoError(t, c.vault.Close())
			reopened, err := OpenLocalVault(archive)
			require.NoError(t, err)
			defer reopened.Close()
			require.Equal(t, data, readVaultBlob(t, reopened, evidence.Objects[0].BlobRef))
		})
	}
}

func TestCoordinatorArchivesEveryPhysicalMutationGeneration(t *testing.T) {
	for _, operation := range []string{"remove", "write", "write-context", "append", "copy", "move", "remove-directory", "test-connection"} {
		t.Run(operation, func(t *testing.T) {
			th, c, _ := coordinatorFixture(t, nil)
			original := []byte("original held evidence\x00")
			_, info := uploadFixtureFile(t, th, original)
			if operation == "test-connection" {
				_, err := c.source.WriteFile(bytes.NewReader(original), "testfile")
				require.NoError(t, err)
				_, err = c.fence.db.Exec(`UPDATE fileinfo SET path='testfile' WHERE id=$1`, info.Id)
				require.NoError(t, err)
				info.Path = "testfile"
			}
			_, err := c.ReserveFile(context.Background(), info.Id)
			require.NoError(t, err)
			var second *model.FileInfo
			replacement := []byte("replacement bytes")
			if operation == "copy" || operation == "move" {
				_, second = uploadFixtureFile(t, th, replacement)
				if operation == "move" {
					_, err = c.ReserveFile(context.Background(), second.Id)
					require.NoError(t, err)
				}
			}
			if operation == "remove-directory" {
				_, second = uploadFixtureFile(t, th, []byte("deep evidence"))
				deep := path.Join(path.Dir(info.Path), strings.Repeat("deep/", 30), "file.bin")
				_, err = c.source.WriteFile(strings.NewReader("deep evidence"), deep)
				require.NoError(t, err)
				_, err = c.fence.db.Exec(`UPDATE fileinfo SET path=$2 WHERE id=$1`, second.Id, deep)
				require.NoError(t, err)
				second.Path = deep
				_, err = c.ReserveFile(context.Background(), second.Id)
				require.NoError(t, err)
			}
			backend := th.Server.FileBackend()
			var expected []byte
			switch operation {
			case "remove":
				err = backend.RemoveFile(info.Path)
			case "write":
				_, err = backend.WriteFile(bytes.NewReader(replacement), info.Path)
				expected = replacement
			case "write-context":
				_, err = filestore.TryWriteFileContext(context.Background(), backend, bytes.NewReader(replacement), info.Path)
				expected = replacement
			case "append":
				_, err = backend.AppendFile(strings.NewReader("tail"), info.Path)
				expected = append(append([]byte(nil), original...), []byte("tail")...)
			case "copy":
				err = backend.CopyFile(second.Path, info.Path)
				expected = replacement
			case "move":
				err = backend.MoveFile(second.Path, info.Path)
				expected = replacement
			case "remove-directory":
				err = backend.RemoveDirectory(path.Dir(info.Path))
			case "test-connection":
				err = backend.TestConnection()
			}
			require.NoError(t, err)
			requireMutationBytes(t, c, info.Id, "before", info.Path, original)
			if expected == nil {
				present, _ := mutationObject(t, c, info.Id, "after", info.Path)
				require.False(t, present)
			} else {
				requireMutationBytes(t, c, info.Id, "after", info.Path, expected)
			}
			if operation == "move" || operation == "remove-directory" {
				before := replacement
				if operation == "remove-directory" {
					before = []byte("deep evidence")
				}
				requireMutationBytes(t, c, second.Id, "before", second.Path, before)
				present, _ := mutationObject(t, c, second.Id, "after", second.Path)
				require.False(t, present)
			}
			var pending int
			require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.mutation_pending`).Scan(&pending))
			require.Zero(t, pending)
		})
	}
}

func TestCoordinatorRecoversChangedBytesAfterPostMutationArchiveFailure(t *testing.T) {
	th, c, archive := coordinatorFixture(t, nil)
	_, info := uploadFixtureFile(t, th, []byte("original"))
	evidence, err := c.ReserveFile(context.Background(), info.Id)
	require.NoError(t, err)
	replacement := []byte("replacement survived SQL failure")
	err = c.Apply(context.Background(), c.source, Mutation{Operation: "write", Paths: []string{info.Path}}, func() error {
		_, err := c.source.WriteFile(bytes.NewReader(replacement), info.Path)
		if err != nil {
			return err
		}
		return c.vault.Close()
	})
	require.Error(t, err)
	for _, statement := range []string{
		`DELETE FROM fileinfo WHERE id=$1`,
		`DELETE FROM workspace_legal_hold.mutation_pending WHERE intent_id IS NOT NULL AND $1<>''`,
		`INSERT INTO workspace_legal_hold.mutation_completions(intent_id,result) SELECT intent_id,'success' FROM workspace_legal_hold.mutation_pending WHERE $1<>''`,
	} {
		_, err = c.fence.db.Exec(statement, info.Id)
		require.Error(t, err, statement)
	}
	reopened, err := OpenLocalVault(archive)
	require.NoError(t, err)
	defer reopened.Close()
	// Simulate same-length archive corruption after the physical operation.
	// Recovery must retain its pending fence until the required before image
	// is restored from a known independent copy, not just archive the new file.
	objectPath, err := blobPath(evidence.Objects[0].BlobRef)
	require.NoError(t, err)
	objectPath = filepath.Join(archive, objectPath)
	require.NoError(t, os.Chmod(objectPath, 0o600))
	require.NoError(t, os.WriteFile(objectPath, []byte("corrupt!"), 0o600))
	_, err = NewFileCoordinator(context.Background(), c.fence, reopened, c.source)
	require.ErrorIs(t, err, ErrIntegrity, "recovery cannot complete with corrupted before evidence")
	var pending int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.mutation_pending`).Scan(&pending))
	require.Equal(t, 1, pending)
	require.NoError(t, os.WriteFile(objectPath, []byte("original"), 0o600))
	require.NoError(t, os.Chmod(objectPath, 0o400))
	successor, err := NewFileCoordinator(context.Background(), c.fence, reopened, c.source)
	require.NoError(t, err)
	requireMutationBytes(t, successor, info.Id, "before", info.Path, []byte("original"))
	requireMutationBytes(t, successor, info.Id, "recovery", info.Path, replacement)
	var result string
	require.NoError(t, c.fence.db.QueryRow(`SELECT result FROM workspace_legal_hold.mutation_completions
		WHERE intent_id IN(SELECT intent_id FROM workspace_legal_hold.mutation_files WHERE file_id=$1) ORDER BY completed_at DESC LIMIT 1`, info.Id).Scan(&result))
	require.Equal(t, "recovered", result)
	_, err = c.fence.db.Exec(`DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.NoError(t, err)
	other, _ := openTestVault(t)
	_, err = NewFileCoordinator(context.Background(), c.fence, other, c.source)
	require.ErrorContains(t, err, "archive differs")
}

func TestCoordinatorArchivesPartialBackendFailure(t *testing.T) {
	th, c, _ := coordinatorFixture(t, nil)
	_, info := uploadFixtureFile(t, th, []byte("before partial write"))
	_, err := c.ReserveFile(context.Background(), info.Id)
	require.NoError(t, err)
	failure := errors.New("backend stopped after partial write")
	err = c.Apply(context.Background(), c.source, Mutation{Operation: "write", Paths: []string{info.Path}}, func() error {
		_, err := c.source.WriteFile(strings.NewReader("partial"), info.Path)
		return errors.Join(err, failure)
	})
	require.ErrorIs(t, err, failure)
	requireMutationBytes(t, c, info.Id, "before", info.Path, []byte("before partial write"))
	requireMutationBytes(t, c, info.Id, "after", info.Path, []byte("partial"))
	var result string
	require.NoError(t, c.fence.db.QueryRow(`SELECT result FROM workspace_legal_hold.mutation_completions
		WHERE intent_id IN(SELECT intent_id FROM workspace_legal_hold.mutation_files WHERE file_id=$1)`, info.Id).Scan(&result))
	require.Equal(t, "failed", result)
}

func TestCoordinatorRetainsPhysicalLeaseAfterCancellationAndSQLLoss(t *testing.T) {
	th, c, archive := coordinatorFixture(t, nil)
	_, info := uploadFixtureFile(t, th, []byte("original"))
	_, err := c.ReserveFile(context.Background(), info.Id)
	require.NoError(t, err)
	before, err := os.Stat(filepath.Join(archive, mutationLockPath))
	require.NoError(t, err)
	entered, resume := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.Apply(ctx, c.source, Mutation{Operation: "write", Paths: []string{info.Path}}, func() error {
			_, err := c.source.WriteFile(strings.NewReader("new generation"), info.Path)
			close(entered)
			<-resume
			return err
		})
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("physical writer did not start")
	}
	cancel()
	rows, err := c.fence.db.Query(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname=current_database() AND pid<>pg_backend_pid() AND state='idle'`)
	require.NoError(t, err)
	require.NoError(t, rows.Close())
	waitCtx, stop := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer stop()
	_, err = NewFileCoordinator(waitCtx, c.fence, c.vault, c.source)
	require.ErrorIs(t, err, context.DeadlineExceeded, "SQL/caller loss must not release an active physical lease")
	_, err = c.fence.db.Exec(`DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.ErrorContains(t, err, "physical mutation is pending")
	close(resume)
	require.ErrorIs(t, <-done, context.Canceled)
	successor, err := NewFileCoordinator(context.Background(), c.fence, c.vault, c.source)
	require.NoError(t, err)
	requireMutationBytes(t, successor, info.Id, "recovery", info.Path, []byte("new generation"))
	after, err := os.Stat(filepath.Join(archive, mutationLockPath))
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
}

func TestCoordinatorRejectsUnsafePathsForeignBackendAndJournalMutation(t *testing.T) {
	th, c, _ := coordinatorFixture(t, nil)
	_, info := uploadFixtureFile(t, th, []byte("protected"))
	_, err := c.ReserveFile(context.Background(), info.Id)
	require.NoError(t, err)
	for _, value := range []string{"", "../escape", "/absolute", "a//b", "a/./b", "a\\b"} {
		_, err = th.Server.FileBackend().WriteFile(strings.NewReader("bad"), value)
		require.Error(t, err)
	}
	foreign, err := filestore.NewFileBackend(filestore.FileBackendSettings{DriverName: model.ImageDriverLocal, Directory: t.TempDir()})
	require.NoError(t, err)
	err = c.Apply(context.Background(), foreign, Mutation{Operation: "remove", Paths: []string{info.Path}}, func() error { t.Fatal("foreign backend mutation invoked"); return nil })
	require.Error(t, err)
	_, err = th.Server.FileBackend().WriteFile(strings.NewReader("next"), info.Path)
	require.NoError(t, err)
	for _, table := range []string{"archive_binding", "mutation_intents", "mutation_files", "mutation_observations", "mutation_completions"} {
		for _, statement := range []string{"DELETE FROM workspace_legal_hold." + table, "TRUNCATE workspace_legal_hold." + table + " CASCADE"} {
			_, err = c.fence.db.Exec(statement)
			require.Error(t, err, statement)
		}
	}
	_, err = c.fence.db.Exec(`UPDATE workspace_legal_hold.mutation_observations SET present=false,sha256=NULL,size=NULL`)
	require.Error(t, err)
}

//go:embed testdata/file_fence_v1.sql
var legacyFileFenceSchema string

func TestCoordinatorUpgradesRealVersionOneJournalWithoutChangingEvidence(t *testing.T) {
	th, c, _ := coordinatorFixture(t, nil)
	_, info := uploadFixtureFile(t, th, []byte("legacy evidence"))
	db := c.fence.db
	_, err := db.Exec(`DROP SCHEMA workspace_legal_hold CASCADE`)
	require.NoError(t, err)
	_, err = db.Exec(legacyFileFenceSchema)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO workspace_legal_hold.installation VALUES(1,'fileinfo'::regclass::oid);
		CREATE TRIGGER workspace_legal_hold_file_row BEFORE INSERT OR UPDATE OR DELETE ON fileinfo FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.guard_file_row();
		CREATE TRIGGER workspace_legal_hold_file_truncate BEFORE TRUNCATE ON fileinfo FOR EACH STATEMENT EXECUTE FUNCTION workspace_legal_hold.guard_file_truncate();
		ALTER TABLE fileinfo ENABLE ALWAYS TRIGGER workspace_legal_hold_file_row;
		ALTER TABLE fileinfo ENABLE ALWAYS TRIGGER workspace_legal_hold_file_truncate`)
	require.NoError(t, err)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE workspace_legal_hold.control SET revision=revision+1`)
	require.NoError(t, err)
	require.NoError(t, c.fence.reserveFileTx(ctx, tx, info.Id))
	evidence, err := preservePendingFileTx(ctx, tx, info.Id, c.source, c.vault)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	upgraded, err := InstallFileFence(ctx, db)
	require.NoError(t, err)
	after, err := upgraded.LatestFileEvidence(ctx, info.Id)
	require.NoError(t, err)
	require.Equal(t, evidence, after)
	var version int
	require.NoError(t, db.QueryRow(`SELECT max(version) FROM workspace_legal_hold.installation`).Scan(&version))
	require.Equal(t, 2, version)
	_, err = NewFileCoordinator(ctx, upgraded, c.vault, c.source)
	require.NoError(t, err)
}
