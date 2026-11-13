package legal_hold

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
	"github.com/stretchr/testify/require"
)

func fileFenceFixture(t *testing.T) (*api4.TestHelper, *fixturePreservationGuard, *FileFence, *model.Post, *model.FileInfo, []byte) {
	t.Helper()
	th, guard, _ := startupFixtureServer(t)
	data := bytes.Repeat([]byte("دليل مرفق\x00"), 10000)
	post, info := uploadFixtureFile(t, th, data)
	fence, err := InstallFileFence(context.Background(), th.Store.GetInternalMasterDB())
	require.NoError(t, err)
	// The API helper reuses one database and truncates all public tables
	// between tests. Remove this test-only schema after assertions; otherwise
	// its real TRUNCATE fence correctly prevents that fixture reset as well.
	t.Cleanup(func() {
		_, err := th.Store.GetInternalMasterDB().Exec(`DROP SCHEMA workspace_legal_hold CASCADE`)
		require.NoError(t, err)
	})
	return th, guard, fence, post, info, data
}

func TestFileFenceNativeDeleteRequiresCommittedArchiveReceipt(t *testing.T) {
	th, guard, fence, post, info, data := fileFenceFixture(t)
	ctx := context.Background()
	require.NoError(t, fence.ReserveFile(ctx, info.Id))
	guard.mu.Lock()
	guard.held[info.Path] = true
	require.NoError(t, guard.vault.Close())
	guard.mu.Unlock()
	// The unmodified native path logs the object error, then attempts its SQL
	// deletion. This time the durable pending reservation must reject it.
	appErr := th.App.PermanentDeleteFilesByPost(th.Context, post.Id, nil)
	require.NotNil(t, appErr)
	require.Contains(t, appErr.Error(), "preservation is pending")
	_, err := th.Store.FileInfo().GetFromMaster(info.Id)
	require.NoError(t, err, "native SQL must retain FileInfo after failed byte preservation")
	live, appErr := th.App.ReadFile(info.Path)
	require.Nil(t, appErr)
	require.Equal(t, data, live)
	_, err = fence.LatestFileEvidence(ctx, info.Id)
	require.ErrorIs(t, err, ErrPreservationPending)

	// A new service object and reopened archive recover the committed pending
	// reservation; nothing depends on the original guard's in-memory map.
	fence, err = InstallFileFence(ctx, th.Store.GetInternalMasterDB())
	require.NoError(t, err)
	// openTestVault provisions the required private permissions.
	vault, archive := openTestVault(t)
	evidence, err := fence.PreservePendingFile(ctx, info.Id, th.Server.FileBackend(), vault)
	require.NoError(t, err)
	require.Len(t, evidence.Objects, 1)
	require.Equal(t, data, readVaultBlob(t, vault, evidence.Objects[0].BlobRef))
	guard.mu.Lock()
	guard.vault = vault
	guard.mu.Unlock()
	require.Nil(t, th.App.PermanentDeleteFilesByPost(th.Context, post.Id, nil))
	_, err = th.Store.FileInfo().GetFromMaster(info.Id)
	require.Error(t, err)
	exists, appErr := th.App.FileExists(info.Path)
	require.Nil(t, appErr)
	require.False(t, exists)
	require.NoError(t, vault.Close())
	reopened, err := OpenLocalVault(archive)
	require.NoError(t, err)
	defer reopened.Close()
	recovered, err := fence.PreservePendingFile(ctx, info.Id, th.Server.FileBackend(), reopened)
	require.NoError(t, err, "committed evidence must survive deletion and archive reopening")
	require.Equal(t, evidence, recovered)
	require.Equal(t, data, readVaultBlob(t, reopened, recovered.Objects[0].BlobRef))
	t.Log("native permanent attachment deletion retained FileInfo without a receipt, then deleted both live records only after committed archive evidence")
}

func TestFileFenceCapturesMetadataVersionsAndEveryReferencedObject(t *testing.T) {
	th, guard, fence, _, info, data := fileFenceFixture(t)
	ctx := context.Background()
	db := th.Store.GetInternalMasterDB()
	objects := map[string][]byte{"original": data, "thumbnail": []byte("صورة مصغرة\x00"), "preview": []byte("معاينة\xff")}
	for role, content := range objects {
		if role != "original" {
			_, err := th.App.WriteFile(bytes.NewReader(content), info.Path+"."+role)
			require.Nil(t, err)
		}
	}
	_, err := db.Exec(`ALTER TABLE fileinfo ADD COLUMN preservation_extra jsonb;
		UPDATE fileinfo SET preservation_extra='{"unknown_future_field":"يبقى"}'::jsonb`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec(`ALTER TABLE fileinfo DROP COLUMN preservation_extra`)
		require.NoError(t, err)
	})
	_, err = db.Exec(`UPDATE fileinfo SET thumbnailpath=$2,previewpath=$3,content=$4,minipreview=$5 WHERE id=$1`,
		info.Id, info.Path+".thumbnail", info.Path+".preview", "نص الفهرسة", []byte{0, 255, 1})
	require.NoError(t, err)
	require.NoError(t, fence.ReserveFile(ctx, info.Id))
	require.NoError(t, fence.ReserveFile(ctx, info.Id), "repeat reservation must not replace its metadata")
	_, err = db.Exec(`UPDATE fileinfo SET name='نسخة غير محفوظة' WHERE id=$1`, info.Id)
	require.ErrorContains(t, err, "preservation is pending")
	first, err := fence.PreservePendingFile(ctx, info.Id, th.Server.FileBackend(), guard.vault)
	require.NoError(t, err)
	require.Len(t, first.Objects, 3)
	var exact bool
	require.NoError(t, db.QueryRow(`SELECT metadata=to_jsonb(f) FROM workspace_legal_hold.file_versions v
		JOIN fileinfo f ON f.id=v.file_id WHERE v.id=$1`, first.VersionID).Scan(&exact))
	require.True(t, exact, "all fields, including NULLs and an unknown field, must be preserved")
	for _, object := range first.Objects {
		require.Equal(t, objects[object.Role], readVaultBlob(t, guard.vault, object.BlobRef))
	}
	_, err = db.Exec(`UPDATE fileinfo SET name='النسخة الثانية',deleteat=123 WHERE id=$1`, info.Id)
	require.NoError(t, err)
	pending, err := fence.LatestFileEvidence(ctx, info.Id)
	require.ErrorIs(t, err, ErrPreservationPending)
	require.Greater(t, pending.VersionID, first.VersionID)
	require.Contains(t, string(pending.Metadata), "النسخة الثانية")
	_, err = db.Exec(`DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.ErrorContains(t, err, "preservation is pending")
	second, err := fence.PreservePendingFile(ctx, info.Id, th.Server.FileBackend(), guard.vault)
	require.NoError(t, err)
	require.Equal(t, pending.VersionID, second.VersionID)
	_, err = db.Exec(`UPDATE fileinfo SET id=$2 WHERE id=$1`, info.Id, model.NewId())
	require.ErrorContains(t, err, "ID cannot change")
	_, err = db.Exec(`DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO fileinfo(id) VALUES($1)`, info.Id)
	require.ErrorContains(t, err, "ID cannot be reused")
	_, another := uploadFixtureFile(t, th, []byte("another unheld attachment"))
	_, err = db.Exec(`UPDATE fileinfo SET id=$2 WHERE id=$1`, another.Id, info.Id)
	require.ErrorContains(t, err, "ID cannot be reused", "an unheld row cannot take over a deleted reserved ID")
	var versions, receipts int
	require.NoError(t, db.QueryRow(`SELECT count(*),(SELECT count(*) FROM workspace_legal_hold.file_receipts)
		FROM workspace_legal_hold.file_versions WHERE file_id=$1`, info.Id).Scan(&versions, &receipts))
	require.Equal(t, 2, versions)
	require.Equal(t, 2, receipts)
}

func TestFileFencePendingBatchesTruncateReplicaAndJournalProtection(t *testing.T) {
	th, guard, fence, _, info, _ := fileFenceFixture(t)
	ctx := context.Background()
	db := th.Store.GetInternalMasterDB()
	_, unheld := uploadFixtureFile(t, th, []byte("unheld file"))
	require.NoError(t, fence.ReserveFile(ctx, info.Id))
	_, err := db.Exec(`DELETE FROM fileinfo WHERE id IN ($1,$2)`, unheld.Id, info.Id)
	require.ErrorContains(t, err, "preservation is pending")
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM fileinfo WHERE id IN ($1,$2)`, unheld.Id, info.Id).Scan(&count))
	require.Equal(t, 2, count, "a failed native batch must roll back its unheld rows too")
	_, err = db.Exec(`UPDATE fileinfo SET name='unheld still works' WHERE id=$1`, unheld.Id)
	require.NoError(t, err)
	_, err = db.Exec(`TRUNCATE fileinfo`)
	require.ErrorContains(t, err, "prevents FileInfo truncate")
	tx, err := db.Begin()
	require.NoError(t, err)
	_, err = tx.Exec(`SET LOCAL session_replication_role=replica`)
	require.NoError(t, err)
	_, err = tx.Exec(`DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.ErrorContains(t, err, "preservation is pending", "ALWAYS triggers must cover replication-role DML")
	require.NoError(t, tx.Rollback())
	_, err = fence.PreservePendingFile(ctx, info.Id, th.Server.FileBackend(), guard.vault)
	require.NoError(t, err)
	for _, statement := range []string{
		`UPDATE workspace_legal_hold.file_versions SET metadata='{}'`,
		`DELETE FROM workspace_legal_hold.file_versions`,
		`TRUNCATE workspace_legal_hold.file_versions CASCADE`,
		`DELETE FROM workspace_legal_hold.file_reservations`,
		`UPDATE workspace_legal_hold.file_receipts SET manifest='[]'`,
		`DELETE FROM workspace_legal_hold.file_receipts`,
		`TRUNCATE workspace_legal_hold.file_receipts`,
		`DELETE FROM workspace_legal_hold.installation`,
		`DELETE FROM workspace_legal_hold.control`,
		`TRUNCATE workspace_legal_hold.control`,
		`UPDATE workspace_legal_hold.control SET revision=0`,
	} {
		_, err = db.Exec(statement)
		require.Error(t, err, statement)
	}
}

func TestFileFenceRejectsIncompleteManifestAndFailedCopies(t *testing.T) {
	th, guard, fence, _, info, _ := fileFenceFixture(t)
	ctx := context.Background()
	db := th.Store.GetInternalMasterDB()
	_, err := db.Exec(`UPDATE fileinfo SET previewpath=$2 WHERE id=$1`, info.Id, info.Path+".missing-preview")
	require.NoError(t, err)
	require.NoError(t, fence.ReserveFile(ctx, info.Id))
	evidence, err := fence.LatestFileEvidence(ctx, info.Id)
	require.ErrorIs(t, err, ErrPreservationPending)
	valid := []PreservedFileObject{
		{Role: "original", Path: info.Path, BlobRef: BlobRef{SHA256: strings.Repeat("a", 64), Size: info.Size}},
		{Role: "preview", Path: info.Path + ".missing-preview", BlobRef: BlobRef{SHA256: strings.Repeat("b", 64), Size: 0}},
	}
	for _, mode := range []string{"missing-original", "missing-preview", "wrong-path", "wrong-size", "wrong-digest", "duplicate-role", "extra-role"} {
		t.Run(mode, func(t *testing.T) {
			manifest := append([]PreservedFileObject(nil), valid...)
			switch mode {
			case "missing-original":
				manifest = manifest[1:]
			case "missing-preview":
				manifest = manifest[:1]
			case "wrong-path":
				manifest[0].Path = "another/file"
			case "wrong-size":
				manifest[0].Size++
			case "wrong-digest":
				manifest[0].SHA256 = "not-a-sha256"
			case "duplicate-role":
				manifest = append(manifest, manifest[0])
			case "extra-role":
				manifest = append(manifest, PreservedFileObject{Role: "alien", Path: "extra"})
			}
			body, err := json.Marshal(manifest)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO workspace_legal_hold.file_receipts(version_id,manifest) VALUES($1,$2::jsonb)`, evidence.VersionID, string(body))
			require.Error(t, err)
		})
	}
	_, err = fence.PreservePendingFile(ctx, info.Id, &fenceWrongSizeSource{FileBackend: th.Server.FileBackend()}, guard.vault)
	require.ErrorContains(t, err, "original size differs from metadata")
	_, err = fence.PreservePendingFile(ctx, info.Id, th.Server.FileBackend(), guard.vault)
	require.Error(t, err, "a referenced but missing derivative must not produce a sealed receipt")
	var receipts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.file_receipts`).Scan(&receipts))
	require.Zero(t, receipts)
	_, err = db.Exec(`DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.ErrorContains(t, err, "preservation is pending")
	_, appErr := th.App.WriteFile(strings.NewReader("preview recovered"), info.Path+".missing-preview")
	require.Nil(t, appErr)
	recovered, err := fence.PreservePendingFile(ctx, info.Id, th.Server.FileBackend(), guard.vault)
	require.NoError(t, err)
	require.Equal(t, evidence.VersionID, recovered.VersionID)
	require.Len(t, recovered.Objects, 2)
}

type fenceWrongSizeSource struct{ filestore.FileBackend }

func (b *fenceWrongSizeSource) FileSize(path string) (int64, error) {
	size, err := b.FileBackend.FileSize(path)
	return size + 1, err
}

func waitForFenceSQLLock(t *testing.T, db *sql.DB, pid int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var blocked bool
		err := db.QueryRow(`SELECT COALESCE((SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1),false)`, pid).Scan(&blocked)
		return err == nil && blocked
	}, 5*time.Second, 10*time.Millisecond, "PostgreSQL must confirm the exact waiter is blocked")
}

func TestFileFenceReservationWinsAgainstInFlightSQLSnapshots(t *testing.T) {
	for _, isolation := range []sql.IsolationLevel{sql.LevelReadCommitted, sql.LevelRepeatableRead, sql.LevelSerializable} {
		t.Run(isolation.String(), func(t *testing.T) {
			th, _, fence, _, info, _ := fileFenceFixture(t)
			db := th.Store.GetInternalMasterDB()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			conn, err := db.Conn(ctx)
			require.NoError(t, err)
			defer conn.Close()
			var pid int
			require.NoError(t, conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
			deleter, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: isolation})
			require.NoError(t, err)
			defer deleter.Rollback()
			var existing string
			require.NoError(t, deleter.QueryRowContext(ctx, `SELECT id FROM fileinfo WHERE id=$1`, info.Id).Scan(&existing))
			reserver, err := fence.begin(ctx)
			require.NoError(t, err)
			defer reserver.Rollback()
			result := make(chan error, 1)
			go func() {
				_, err := deleter.ExecContext(ctx, `DELETE FROM fileinfo WHERE id=$1`, info.Id)
				result <- err
			}()
			waitForFenceSQLLock(t, db, pid)
			require.NoError(t, fence.reserveFileTx(ctx, reserver, info.Id), "must not deadlock with the native FileInfo row lock")
			require.NoError(t, reserver.Commit())
			err = <-result
			require.Error(t, err)
			if isolation == sql.LevelReadCommitted {
				require.Contains(t, err.Error(), "preservation is pending")
			} else {
				var pgErr *pq.Error
				require.ErrorAs(t, err, &pgErr)
				require.Equal(t, pq.ErrorCode("40001"), pgErr.Code, "stale snapshots must fail instead of missing a new reservation")
			}
			require.NoError(t, deleter.Rollback())
			_, err = th.Store.FileInfo().GetFromMaster(info.Id)
			require.NoError(t, err)
		})
	}
}

func TestFileFenceDeletionWinsBeforeReservationAndLockWaitCancels(t *testing.T) {
	th, _, fence, _, info, _ := fileFenceFixture(t)
	db := th.Store.GetInternalMasterDB()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	deleter, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer deleter.Rollback()
	_, err = deleter.ExecContext(ctx, `DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.NoError(t, err)
	waitCtx, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	defer stop()
	err = fence.ReserveFile(waitCtx, info.Id)
	require.Error(t, err)
	require.ErrorIs(t, waitCtx.Err(), context.DeadlineExceeded)
	result := make(chan error, 1)
	go func() { result <- fence.ReserveFile(ctx, info.Id) }()
	require.Eventually(t, func() bool {
		var count int
		err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()
			AND wait_event_type='Lock' AND query LIKE 'UPDATE workspace_legal_hold.control SET revision=%'`).Scan(&count)
		return err == nil && count == 1
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, deleter.Commit())
	require.ErrorIs(t, <-result, sql.ErrNoRows, "a reservation cannot succeed after deletion committed first")
	_, err = fence.LatestFileEvidence(ctx, info.Id)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

type fenceBlockedSource struct {
	filestore.FileBackend
	started chan struct{}
	resume  chan struct{}
}

func (b *fenceBlockedSource) Reader(path string) (filestore.ReadCloseSeeker, error) {
	reader, err := b.FileBackend.Reader(path)
	if err != nil {
		return nil, err
	}
	return &fenceBlockedClose{ReadCloseSeeker: reader, owner: b}, nil
}

type fenceBlockedClose struct {
	filestore.ReadCloseSeeker
	owner *fenceBlockedSource
	once  sync.Once
}

func (r *fenceBlockedClose) Close() error {
	r.once.Do(func() {
		close(r.owner.started)
		<-r.owner.resume
	})
	return r.ReadCloseSeeker.Close()
}

func TestFileFenceSQLSessionLossAfterCopyLeavesPendingEvidenceRecoverable(t *testing.T) {
	th, guard, fence, _, info, data := fileFenceFixture(t)
	db := th.Store.GetInternalMasterDB()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	require.NoError(t, fence.ReserveFile(ctx, info.Id))
	pending, err := fence.LatestFileEvidence(ctx, info.Id)
	require.ErrorIs(t, err, ErrPreservationPending)
	blocked := &fenceBlockedSource{FileBackend: th.Server.FileBackend(), started: make(chan struct{}), resume: make(chan struct{})}
	defer func() {
		select {
		case <-blocked.resume:
		default:
			close(blocked.resume)
		}
	}()
	result := make(chan error, 1)
	go func() {
		_, err := fence.PreservePendingFile(ctx, info.Id, blocked, guard.vault)
		result <- err
	}()
	select {
	case <-blocked.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Source Close is reached after Put published and synced the object. Check
	// that fact before killing SQL, so the recovery test covers this real gap.
	hash := sha256.Sum256(data)
	ref := BlobRef{SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data))}
	published, err := guard.vault.OpenVerified(ref)
	require.NoError(t, err)
	before, err := published.Stat()
	require.NoError(t, err)
	require.NoError(t, published.Close())
	// This is a real PostgreSQL backend termination, not an injected return
	// value. The pending metadata committed before this session remains durable.
	var pid int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT pid FROM pg_stat_activity WHERE datname=current_database()
		AND state='idle in transaction' AND query LIKE 'SELECT v.id,v.metadata,r.manifest%'`).Scan(&pid))
	var terminated bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated))
	require.True(t, terminated)
	close(blocked.resume)
	require.Error(t, <-result)
	stillPending, err := fence.LatestFileEvidence(ctx, info.Id)
	require.ErrorIs(t, err, ErrPreservationPending)
	require.Equal(t, pending.VersionID, stillPending.VersionID)
	_, err = db.ExecContext(ctx, `DELETE FROM fileinfo WHERE id=$1`, info.Id)
	require.ErrorContains(t, err, "preservation is pending")
	recovered, err := fence.PreservePendingFile(ctx, info.Id, th.Server.FileBackend(), guard.vault)
	require.NoError(t, err)
	require.Equal(t, pending.VersionID, recovered.VersionID)
	require.Equal(t, data, readVaultBlob(t, guard.vault, recovered.Objects[0].BlobRef))
	retried, err := guard.vault.OpenVerified(ref)
	require.NoError(t, err)
	after, err := retried.Stat()
	require.NoError(t, err)
	require.NoError(t, retried.Close())
	require.True(t, os.SameFile(before, after), "retry must reuse the already published object without overwriting it")
	t.Logf("pending receipt recovered after terminating SQL backend %d; published archive bytes deduplicated on retry", pid)
}

func TestFileFenceInstallerRejectsChangedSourceAndMissingTrigger(t *testing.T) {
	th, _, fence, _, info, _ := fileFenceFixture(t)
	db := th.Store.GetInternalMasterDB()
	ctx := context.Background()
	again, err := InstallFileFence(ctx, db)
	require.NoError(t, err)
	require.Equal(t, fence.fileTable, again.fileTable)
	_, err = db.Exec(`ALTER TABLE fileinfo DISABLE TRIGGER workspace_legal_hold_file_row`)
	require.NoError(t, err)
	_, err = InstallFileFence(ctx, db)
	require.ErrorContains(t, err, "triggers are incomplete")
	_, err = db.Exec(`ALTER TABLE fileinfo ENABLE ALWAYS TRIGGER workspace_legal_hold_file_row;
		ALTER TABLE workspace_legal_hold.file_receipts DISABLE TRIGGER validate_receipt`)
	require.NoError(t, err)
	_, err = InstallFileFence(ctx, db)
	require.ErrorContains(t, err, "triggers are incomplete")
	_, err = db.Exec(`ALTER TABLE workspace_legal_hold.file_receipts ENABLE ALWAYS TRIGGER validate_receipt;
		ALTER TABLE fileinfo RENAME TO original_fileinfo;
		CREATE TABLE fileinfo(LIKE original_fileinfo INCLUDING ALL)`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec(`DROP TABLE fileinfo; ALTER TABLE original_fileinfo RENAME TO fileinfo`)
		require.NoError(t, err)
	})
	_, err = InstallFileFence(ctx, db)
	require.ErrorContains(t, err, "identity differs")
	var existing string
	require.NoError(t, db.QueryRow(`SELECT id FROM original_fileinfo WHERE id=$1`, info.Id).Scan(&existing))
	require.Equal(t, info.Id, existing)
}
