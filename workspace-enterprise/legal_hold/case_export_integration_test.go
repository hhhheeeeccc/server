package legal_hold

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func exportFixture(t *testing.T, m *CaseManager, limit int64) (*CaseExporter, string) {
	t.Helper()
	require.NoError(t, InstallCaseExportJournal(context.Background(), m.journal))
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	exporter, err := NewCaseExporter(m, directory, limit)
	require.NoError(t, err)
	t.Cleanup(func() { _ = exporter.Close() })
	return exporter, directory
}

func readExportEntries(t *testing.T, file *os.File) map[string][]byte {
	t.Helper()
	info, err := file.Stat()
	require.NoError(t, err)
	zr, err := zip.NewReader(file, info.Size())
	require.NoError(t, err)
	result := map[string][]byte{}
	for _, entry := range zr.File {
		reader, err := entry.Open()
		require.NoError(t, err)
		result[entry.Name], err = io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
	}
	return result
}

func TestCaseExportPreservesDeletedMessagesTemporaryBodiesAndIndependentAttachmentBytes(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	data := bytes.Repeat([]byte("case export evidence\x00"), 130000)
	post, info := uploadFixtureFile(t, th, data)
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	post.Message = "تحرير محفوظ قبل الحذف"
	_, _, err := th.Client.UpdatePost(context.Background(), post.Id, post)
	require.NoError(t, err)
	_, err = th.Store.TemporaryPost().Save(th.Context, &model.TemporaryPost{ID: post.Id, Type: model.PostTypeBurnOnRead, Message: "جسم مؤقت محفوظ", FileIDs: post.FileIds})
	require.NoError(t, err)
	require.Nil(t, th.App.PermanentDeletePost(th.Context, post.Id, th.BasicUser.Id))
	var gone bool
	require.NoError(t, c.fence.db.QueryRow(`SELECT NOT EXISTS(SELECT 1 FROM posts WHERE id=$1)`, post.Id).Scan(&gone))
	require.True(t, gone)
	_, err = th.Store.FileInfo().GetFromMaster(info.Id)
	require.Error(t, err)
	exporter, directory := exportFixture(t, m, 32<<20)
	result, err := exporter.Export(rctx, status.ID)
	require.NoError(t, err)
	file, reopened, err := exporter.Open(rctx, result.ID)
	require.NoError(t, err)
	require.Equal(t, result, reopened)
	entries := readExportEntries(t, file)
	require.NoError(t, file.Close())
	require.Contains(t, string(entries["versions.jsonl"]), post.Message)
	require.Contains(t, string(entries["versions.jsonl"]), "جسم مؤقت محفوظ")
	hash := sha256.Sum256(data)
	ref := BlobRef{hex.EncodeToString(hash[:]), int64(len(data))}
	name, err := blobPath(ref)
	require.NoError(t, err)
	require.Equal(t, data, entries[name])
	var state, digest, manifestSHA string
	var size int64
	require.NoError(t, c.fence.db.QueryRow(`SELECT state,sha256,size,manifest_sha256 FROM workspace_legal_hold.case_export_outcomes WHERE export_id=$1`, result.ID).Scan(&state, &digest, &size, &manifestSHA))
	require.Equal(t, "complete", state)
	require.Equal(t, result.SHA256, digest)
	require.Equal(t, result.Size, size)
	manifestDigest := sha256.Sum256(entries["manifest.json"])
	require.Equal(t, hex.EncodeToString(manifestDigest[:]), manifestSHA)
	stage, err := os.ReadDir(filepath.Join(directory, "staging"))
	require.NoError(t, err)
	require.Empty(t, stage)
	// A separate exporter instance delivers only the confirmed artifact.
	other, err := NewCaseExporter(m, directory, 32<<20)
	require.NoError(t, err)
	defer other.Close()
	file, _, err = other.Open(rctx, result.ID)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	t.Log("verified ZIP retains actual deleted post, temporary body and independent attachment bytes; durable outcome survives exporter reopen")
}

func TestCaseExportIncludesPhysicalGenerationsWithoutOtherCaseDirectoryBytes(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	first, firstInfo := uploadFixtureFile(t, th, []byte("generation one"))
	second, secondInfo := uploadFixtureFile(t, th, []byte("unrelated case secret"))
	// Explicit scope fixture: both live paths share a directory, while the
	// posts are in different channels and therefore different cases.
	_, err := c.fence.db.Exec(`UPDATE posts SET channelid=$1 WHERE id=$2;`, th.BasicChannel2.Id, second.Id)
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`UPDATE fileinfo SET channelid=$1 WHERE id=$2`, th.BasicChannel2.Id, secondInfo.Id)
	require.NoError(t, err)
	firstPath := "mixed-case/a.bin"
	secondPath := "mixed-case/b.bin"
	_, err = c.source.WriteFile(bytes.NewReader([]byte("generation one")), firstPath)
	require.NoError(t, err)
	_, err = c.source.WriteFile(bytes.NewReader([]byte("unrelated case secret")), secondPath)
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`UPDATE fileinfo SET path=$1 WHERE id=$2`, firstPath, firstInfo.Id)
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`UPDATE fileinfo SET path=$1 WHERE id=$2`, secondPath, secondInfo.Id)
	require.NoError(t, err)
	status := startChannelCase(t, m, rctx, first.ChannelId)
	startChannelCase(t, m, rctx, th.BasicChannel2.Id)
	_, err = th.Server.FileBackend().WriteFile(bytes.NewReader([]byte("generation two")), firstPath)
	require.NoError(t, err)
	require.NoError(t, th.Server.FileBackend().RemoveDirectory("mixed-case"))
	exporter, _ := exportFixture(t, m, 16<<20)
	result, err := exporter.Export(rctx, status.ID)
	require.NoError(t, err)
	file, _, err := exporter.Open(rctx, result.ID)
	require.NoError(t, err)
	defer file.Close()
	entries := readExportEntries(t, file)
	for _, text := range []string{"generation one", "generation two"} {
		hash := sha256.Sum256([]byte(text))
		name, err := blobPath(BlobRef{hex.EncodeToString(hash[:]), int64(len(text))})
		require.NoError(t, err)
		require.Equal(t, text, string(entries[name]))
	}
	for name, content := range entries {
		require.NotContains(t, string(content), "unrelated case secret", name)
		if name != "manifest.json" {
			require.NotContains(t, string(content), secondInfo.Id, name)
		}
	}
	require.NotContains(t, string(entries["mutation_observations.jsonl"]), secondPath)
}

func TestCaseExportRequiresCurrentLicenseAndAdministratorForCreationAndDelivery(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	exporter, _ := exportFixture(t, m, 16<<20)
	for _, caller := range []request.CTX{nil, th.Context, th.Context.WithSession(&model.Session{UserId: th.BasicUser.Id, Roles: "system_admin"})} {
		_, err := exporter.Export(caller, status.ID)
		require.ErrorIs(t, err, ErrCaseForbidden)
	}
	var count int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_exports`).Scan(&count))
	require.Zero(t, count)
	result, err := exporter.Export(rctx, status.ID)
	require.NoError(t, err)
	for _, licensed := range []bool{false, true} {
		license := model.NewTestLicense("compliance")
		license.Features.Compliance = model.NewPointer(false)
		if licensed {
			th.Server.SetLicense(license)
		} else {
			th.Server.SetLicense(nil)
		}
		_, err = exporter.Export(rctx, status.ID)
		require.ErrorIs(t, err, ErrCaseForbidden)
		file, _, err := exporter.Open(rctx, result.ID)
		require.ErrorIs(t, err, ErrCaseForbidden)
		require.Nil(t, file)
	}
	th.Server.SetLicense(model.NewTestLicense("compliance"))
	file, _, err := exporter.Open(th.Context.WithSession(&model.Session{UserId: th.BasicUser.Id}), result.ID)
	require.ErrorIs(t, err, ErrCaseForbidden)
	require.Nil(t, file)
	file, _, err = exporter.Open(rctx, result.ID)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}

func TestCaseExportFailureNeverPublishesCompleteOrExposesPartialArtifact(t *testing.T) {
	for _, fault := range []string{"missing", "corrupt", "limit", "cancelled", "preparing"} {
		t.Run(fault, func(t *testing.T) {
			th, c, m, rctx, _ := caseFixture(t)
			_, info := uploadFixtureFile(t, th, bytes.Repeat([]byte("evidence"), 100000))
			status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
			exporter, directory := exportFixture(t, m, 16<<20)
			evidence, err := c.fence.LatestFileEvidence(context.Background(), info.Id)
			require.NoError(t, err)
			switch fault {
			case "missing", "corrupt":
				name, err := blobPath(evidence.Objects[0].BlobRef)
				require.NoError(t, err)
				if fault == "missing" {
					require.NoError(t, c.vault.root.Remove(name))
				} else {
					require.NoError(t, c.vault.root.Chmod(name, 0o600))
					file, err := c.vault.root.OpenFile(name, os.O_WRONLY, 0)
					require.NoError(t, err)
					_, err = file.WriteAt([]byte("X"), 0)
					require.NoError(t, err)
					require.NoError(t, file.Close())
				}
			case "limit":
				exporter.limit = 1024
			case "cancelled":
				ctx, cancel := context.WithCancel(rctx.Context())
				cancel()
				rctx = rctx.WithContext(ctx)
			case "preparing":
				// Use the real manager and an absent declared derivative.
				_, err = c.fence.db.Exec(`UPDATE fileinfo SET previewpath='missing-derivative' WHERE id=$1`, info.Id)
				require.NoError(t, err)
				status, err = m.StartCapture(rctx, CaseRequest{Title: "لا تصدر قضية معلقة", Scopes: []CaseScope{{Kind: "channels", TargetID: th.BasicChannel.Id}}})
				require.Error(t, err)
				require.Equal(t, "preparing", status.State)
			}
			result, err := exporter.Export(rctx, status.ID)
			require.Error(t, err)
			if fault == "limit" {
				require.ErrorIs(t, err, ErrExportLimit)
			}
			var count int
			require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_export_outcomes WHERE state='complete'`).Scan(&count))
			require.Zero(t, count)
			if fault != "cancelled" {
				var state string
				require.NoError(t, c.fence.db.QueryRow(`SELECT state FROM workspace_legal_hold.case_export_outcomes WHERE export_id=$1`, result.ID).Scan(&state))
				require.Equal(t, "failed", state)
				file, _, err := exporter.Open(rctx, result.ID)
				require.ErrorIs(t, err, sql.ErrNoRows)
				require.Nil(t, file)
			}
			for _, name := range []string{"staging", "packages"} {
				entries, err := os.ReadDir(filepath.Join(directory, name))
				require.NoError(t, err)
				require.Empty(t, entries)
			}
		})
	}
}

func TestCaseExportSnapshotDoesNotIncludeLaterWritesOrDependOnLiveSource(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	_, info := uploadFixtureFile(t, th, []byte("snapshot bytes"))
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	exporter, _ := exportFixture(t, m, 16<<20)
	work := "staging/" + model.NewId()
	require.NoError(t, exporter.root.Mkdir(work, 0o700))
	defer exporter.root.RemoveAll(work)
	budget := &exportBudget{ctx: context.Background(), remaining: 16 << 20}
	manifest, err := exporter.snapshot(context.Background(), work, CaseExportResult{ID: model.NewId(), CaseID: status.ID}, th.SystemAdminUser.Id, budget)
	require.NoError(t, err)
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: th.BasicChannel.Id, Message: "created after export snapshot"})
	require.NoError(t, err)
	require.NoError(t, th.Server.FileBackend().RemoveFile(info.Path))
	_, _, err = exporter.build(context.Background(), work, manifest, budget)
	require.NoError(t, err)
	file, err := exporter.root.Open(work + "/package.zip")
	require.NoError(t, err)
	defer file.Close()
	entries := readExportEntries(t, file)
	require.NotContains(t, string(entries["versions.jsonl"]), post.Id)
	require.Contains(t, caseMessageVersions(t, c.fence.db, status.ID, "posts", post.Id), post.Message)
}

func TestCaseExportRejectsMutatedPackagesMissingReferencedObjectsAndUnsafeEntries(t *testing.T) {
	th, _, m, rctx, _ := caseFixture(t)
	uploadFixtureFile(t, th, []byte("package verification bytes"))
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	exporter, directory := exportFixture(t, m, 16<<20)
	result, err := exporter.Export(rctx, status.ID)
	require.NoError(t, err)
	file, _, err := exporter.Open(rctx, result.ID)
	require.NoError(t, err)
	entries := readExportEntries(t, file)
	require.NoError(t, file.Close())
	for _, fault := range []string{"body", "missing", "extra", "duplicate", "index-removal"} {
		t.Run(fault, func(t *testing.T) {
			copy := map[string][]byte{}
			for name, content := range entries {
				copy[name] = append([]byte(nil), content...)
			}
			var object string
			for name := range copy {
				if strings.HasPrefix(name, "objects/sha256/") {
					object = name
					break
				}
			}
			require.NotEmpty(t, object)
			switch fault {
			case "body":
				copy[object][0] ^= 1
			case "missing":
				delete(copy, object)
			case "extra":
				copy["../unexpected"] = []byte("extra")
			case "index-removal":
				delete(copy, object)
				copy["objects.jsonl"] = nil
				var manifest CaseExportManifest
				require.NoError(t, json.Unmarshal(copy["manifest.json"], &manifest))
				for i := range manifest.Datasets {
					if manifest.Datasets[i].Path == "objects.jsonl" {
						hash := sha256.Sum256(nil)
						manifest.Datasets[i].Rows, manifest.Datasets[i].Size = 0, 0
						manifest.Datasets[i].SHA256 = hex.EncodeToString(hash[:])
					}
				}
				copy["manifest.json"], err = json.Marshal(manifest)
				require.NoError(t, err)
			}
			var data bytes.Buffer
			zw := zip.NewWriter(&data)
			for name, content := range copy {
				entry, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
				require.NoError(t, err)
				_, err = entry.Write(content)
				require.NoError(t, err)
			}
			if fault == "duplicate" {
				_, err := zw.CreateHeader(&zip.FileHeader{Name: "manifest.json", Method: zip.Store})
				require.NoError(t, err)
			}
			require.NoError(t, zw.Close())
			_, err := VerifyCaseExport(context.Background(), bytes.NewReader(data.Bytes()), int64(data.Len()))
			require.ErrorIs(t, err, ErrIntegrity)
		})
	}
	// Recompute-valid ZIP corruption must still fail its anchored SQL hash.
	path := filepath.Join(directory, "packages", result.ID+".zip")
	require.NoError(t, os.Chmod(path, 0o600))
	mutated, err := os.OpenFile(path, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = mutated.WriteAt([]byte("X"), 0)
	require.NoError(t, err)
	require.NoError(t, mutated.Close())
	file, _, err = exporter.Open(rctx, result.ID)
	require.ErrorIs(t, err, ErrIntegrity)
	require.Nil(t, file)
}

func TestCaseExportInstallationAndArtifactStorageGuards(t *testing.T) {
	th, c, m, rctx, archive := caseFixture(t)
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	_, err := NewCaseExporter(m, directory, 1<<20)
	require.Error(t, err, "explicit installation is required")
	exporter, _ := exportFixture(t, m, 16<<20)
	require.NoError(t, InstallCaseExportJournal(context.Background(), m.journal))
	for _, path := range []string{archive, *th.App.Config().FileSettings.Directory} {
		_, err := NewCaseExporter(m, path, 1<<20)
		require.ErrorContains(t, err, "separate")
	}
	require.NoError(t, os.Chmod(directory, 0o755))
	_, err = NewCaseExporter(m, directory, 1<<20)
	require.ErrorContains(t, err, "private permissions")
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	result, err := exporter.Export(rctx, status.ID)
	require.NoError(t, err)
	for _, table := range []string{"case_export_installation", "case_exports", "case_export_outcomes"} {
		for _, operation := range []string{"DELETE FROM ", "TRUNCATE "} {
			query := operation + "workspace_legal_hold." + table
			if operation == "TRUNCATE " {
				query += " CASCADE"
			}
			_, err := c.fence.db.Exec(query)
			require.ErrorContains(t, err, "append only")
		}
	}
	_, err = c.fence.db.Exec(`UPDATE workspace_legal_hold.case_export_outcomes SET sha256=repeat('0',64) WHERE export_id=$1`, result.ID)
	require.ErrorContains(t, err, "append only")
	_, err = c.fence.db.Exec(`ALTER TABLE workspace_legal_hold.case_exports DISABLE TRIGGER immutable_rows`)
	require.NoError(t, err)
	require.ErrorContains(t, InstallCaseExportJournal(context.Background(), m.journal), "triggers are incomplete")
	_, err = NewCaseExporter(m, directory, 1<<20)
	require.ErrorContains(t, err, "triggers are incomplete")
}

func TestCaseExportCancellationAfterDurableRequestKeepsFailedAuditAndReleasesWork(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	exporter, directory := exportFixture(t, m, 16<<20)
	ctx, cancel := context.WithCancel(rctx.Context())
	defer cancel()
	lease, err := c.vault.mutationLease(context.Background())
	require.NoError(t, err)
	defer lease.Close()
	response := make(chan error, 1)
	go func() {
		_, err := exporter.Export(rctx.WithContext(ctx), status.ID)
		response <- err
	}()
	assert.Eventually(t, func() bool {
		var count int
		err := c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_exports`).Scan(&count)
		return err == nil && count == 1
	}, 2*time.Second, 10*time.Millisecond, "request must really be committed before cancellation")
	cancel()
	select {
	case err = <-response:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled export did not stop waiting for the archive lease")
	}
	var state string
	require.NoError(t, c.fence.db.QueryRow(`SELECT state FROM workspace_legal_hold.case_export_outcomes`).Scan(&state))
	require.Equal(t, "failed", state)
	for _, name := range []string{"staging", "packages"} {
		entries, err := os.ReadDir(filepath.Join(directory, name))
		require.NoError(t, err)
		require.Empty(t, entries)
	}
}

func TestCaseExportCannotDeliverPublishedPackageWithoutConfirmedSQLOutcome(t *testing.T) {
	th, c, m, rctx, _ := caseFixture(t)
	uploadFixtureFile(t, th, []byte("private completed bytes without a SQL confirmation"))
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	exporter, directory := exportFixture(t, m, 16<<20)
	_, err := c.fence.db.Exec(`CREATE FUNCTION workspace_legal_hold.reject_export_completion() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.state='complete' THEN RAISE EXCEPTION 'injected completion failure'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER fail_completion BEFORE INSERT ON workspace_legal_hold.case_export_outcomes FOR EACH ROW EXECUTE FUNCTION workspace_legal_hold.reject_export_completion()`)
	require.NoError(t, err)
	result, err := exporter.Export(rctx, status.ID)
	require.ErrorContains(t, err, "injected completion failure")
	var state string
	require.NoError(t, c.fence.db.QueryRow(`SELECT state FROM workspace_legal_hold.case_export_outcomes WHERE export_id=$1`, result.ID).Scan(&state))
	require.Equal(t, "failed", state)
	info, err := os.Stat(filepath.Join(directory, "packages", result.ID+".zip"))
	require.NoError(t, err, "private orphan is retained instead of erasing an ambiguous publication")
	require.Positive(t, info.Size())
	file, _, err := exporter.Open(rctx, result.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.Nil(t, file)
}
