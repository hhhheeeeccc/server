package legal_hold

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
)

// FileCoordinator binds a single live backend, SQL journal and shared archive.
// Every cooperating node must use the same archive inode and this protocol.
// It remains unregistered until case and message preservation are complete.
type FileCoordinator struct {
	fence   *FileFence
	vault   *LocalVault
	source  filestore.FileBackend
	archive string
}

// NewFileCoordinator recovers unfinished physical operations before returning.
// Recovery records the current state; it never replays a write, append or move.
func NewFileCoordinator(ctx context.Context, fence *FileFence, vault *LocalVault, source filestore.FileBackend) (*FileCoordinator, error) {
	if fence == nil || vault == nil || source == nil || !reflect.TypeOf(source).Comparable() {
		return nil, errors.New("legal hold coordinator requires a fence, archive and comparable live backend")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	lease, err := vault.mutationLease(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	identity, err := vault.archiveIdentity()
	if err != nil {
		return nil, err
	}
	coordinator := &FileCoordinator{fence: fence, vault: vault, source: source, archive: identity}
	tx, err := fence.beginControl(ctx, true)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.archive_binding VALUES(true,$1) ON CONFLICT DO NOTHING`, identity); err != nil {
		return nil, err
	}
	var bound string
	if err = tx.QueryRowContext(ctx, `SELECT archive_id FROM workspace_legal_hold.archive_binding WHERE singleton`).Scan(&bound); err != nil {
		return nil, err
	}
	if bound != identity {
		return nil, errors.New("legal hold archive differs from the journal binding")
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if err = coordinator.recoverLocked(ctx); err != nil {
		return nil, err
	}
	return coordinator, nil
}

// ReserveFile coordinates reservation and initial sealing with physical writes.
// An archive failure leaves the SQL reservation pending and fails the call.
func (c *FileCoordinator) ReserveFile(ctx context.Context, fileID string) (FileEvidence, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	lease, err := c.vault.mutationLease(ctx)
	if err != nil {
		return FileEvidence{}, err
	}
	defer lease.Close()
	if err = c.recoverLocked(ctx); err != nil {
		return FileEvidence{}, err
	}
	if err = c.fence.ReserveFile(ctx, fileID); err != nil {
		return FileEvidence{}, err
	}
	return c.fence.PreservePendingFile(ctx, fileID, c.source, c.vault)
}

func canonicalMutationPath(value string, directory bool) error {
	if value == "" || path.IsAbs(value) || path.Clean(value) != value || value == ".." || strings.HasPrefix(value, "../") || strings.ContainsAny(value, "\\\x00") || value == "." && !directory {
		return errors.New("legal hold requires canonical relative storage paths")
	}
	return nil
}

func validateMutation(mutation Mutation) error {
	count := 1
	switch mutation.Operation {
	case "move":
		count = 2
	case "remove", "write", "append", "copy":
	case "remove-directory", "test-connection":
		if !mutation.Recursive {
			return errors.New("legal hold directory mutation must be recursive")
		}
	default:
		return errors.New("unsupported legal hold physical mutation")
	}
	if len(mutation.Paths) != count {
		return errors.New("invalid legal hold physical mutation paths")
	}
	for _, value := range mutation.Paths {
		if err := canonicalMutationPath(value, mutation.Recursive); err != nil {
			return err
		}
	}
	return nil
}

// Apply holds the physical lease through the actual backend call, including
// after context/SQL loss. A durable intent precedes that call. Before images
// and actual after images (also on partial failure) are archived separately.
// No completion is committed until all required after images are durable.
func (c *FileCoordinator) Apply(ctx context.Context, source filestore.FileBackend, mutation Mutation, mutate func() error) error {
	if mutate == nil || reflect.TypeOf(source) != reflect.TypeOf(c.source) || source != c.source {
		return errors.New("legal hold mutation uses a different backend")
	}
	if err := validateMutation(mutation); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	lease, err := c.vault.mutationLease(ctx)
	if err != nil {
		return err
	}
	defer lease.Close()
	if err = c.recoverLocked(ctx); err != nil {
		return err
	}
	id, observed, err := c.prepare(ctx, mutation)
	if err != nil {
		return err
	}
	// Cancellation after preparation is still a recoverable, durable intent.
	if err = ctx.Err(); err != nil {
		return err
	}
	mutationErr := mutate()
	result := "success"
	if mutationErr != nil {
		result = "failed"
	}
	return errors.Join(mutationErr, c.complete(ctx, id, observed, result))
}

func (c *FileCoordinator) prepare(ctx context.Context, mutation Mutation) (string, []string, error) {
	tx, err := c.fence.begin(ctx)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()
	// A post can reference an uploaded file before the native attachment update
	// sets FileInfo.PostId. Discover case references under the same SQL barrier
	// before selecting paths, so that interval cannot bypass physical guarding.
	var casesInstalled bool
	if err = tx.QueryRowContext(ctx, `SELECT to_regclass('workspace_legal_hold.case_installation') IS NOT NULL`).Scan(&casesInstalled); err != nil {
		return "", nil, err
	}
	creating := false
	if casesInstalled {
		// Creating an absent target cannot destroy a generation. In particular,
		// image derivatives are written after their FileInfo row is inserted;
		// reserving/sealing them before creation would deadlock that pipeline.
		creating = mutation.Operation == "write" || mutation.Operation == "append" || mutation.Operation == "copy"
		if creating {
			for _, target := range mutation.Paths {
				present, existsErr := c.source.FileExists(target)
				if existsErr != nil {
					return "", nil, existsErr
				}
				creating = creating && !present
			}
		}
		if !creating {
			paths, _ := json.Marshal(mutation.Paths)
			if _, err = tx.ExecContext(ctx, `SELECT workspace_legal_hold.discover_case_files($1::jsonb,$2)`, string(paths), mutation.Recursive); err != nil {
				return "", nil, err
			}
		}
	}
	query := `SELECT file_id,id,metadata FROM workspace_legal_hold.file_versions`
	if installed, err := lifecycleInstalled(ctx, tx); err != nil {
		return "", nil, err
	} else if installed {
		query += ` WHERE workspace_legal_hold.file_preservation_required(file_id)`
	}
	rows, err := tx.QueryContext(ctx, query+` ORDER BY file_id,id`)
	if err != nil {
		return "", nil, err
	}
	paths := map[string]map[string]bool{}
	versions := map[string]int64{}
	for rows.Next() {
		var fileID string
		var version int64
		var metadata []byte
		if err = rows.Scan(&fileID, &version, &metadata); err != nil {
			break
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(metadata, &fields); err != nil {
			break
		}
		if paths[fileID] == nil {
			paths[fileID] = map[string]bool{}
		}
		versions[fileID] = version
		for _, key := range []string{"path", "previewpath", "thumbnailpath"} {
			var value string
			if raw := fields[key]; raw != nil && string(raw) != "null" {
				if err = json.Unmarshal(raw, &value); err != nil {
					break
				}
			}
			if value != "" {
				if err = canonicalMutationPath(value, false); err != nil {
					break
				}
				paths[fileID][value] = true
			}
		}
		if err != nil {
			break
		}
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return "", nil, err
	}
	selected := map[string]bool{}
	observedSet := map[string]bool{}
	for fileID, values := range paths {
		for value := range values {
			for _, target := range mutation.Paths {
				if value == target || mutation.Recursive && (target == "." || strings.HasPrefix(value, target+"/")) {
					selected[fileID] = true
				}
			}
		}
	}
	for fileID := range selected {
		_, evidenceErr := readFileEvidence(ctx, tx, fileID)
		if !creating || !errors.Is(evidenceErr, ErrPreservationPending) {
			if _, err = preservePendingFileTx(ctx, tx, fileID, c.source, c.vault); err != nil {
				return "", nil, err
			}
		}
		for value := range paths[fileID] {
			observedSet[value] = true
		}
	}
	if len(selected) != 0 && !mutation.Recursive {
		for _, target := range mutation.Paths {
			observedSet[target] = true
		}
	}
	observed := make([]string, 0, len(observedSet))
	for value := range observedSet {
		observed = append(observed, value)
	}
	slices.Sort(observed)
	id := model.NewId()
	pathJSON, _ := json.Marshal(mutation.Paths)
	observedJSON, _ := json.Marshal(observed)
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.mutation_intents(id,archive_id,operation,paths,recursive,observed_paths)
		VALUES($1,$2,$3,$4::jsonb,$5,$6::jsonb)`, id, c.archive, mutation.Operation, string(pathJSON), mutation.Recursive, string(observedJSON)); err != nil {
		return "", nil, err
	}
	for fileID := range selected {
		if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.mutation_files VALUES($1,$2,$3)`, id, fileID, versions[fileID]); err != nil {
			return "", nil, err
		}
	}
	if err = c.observe(ctx, tx, id, "before", observed); err != nil {
		return "", nil, err
	}
	return id, observed, tx.Commit()
}

func (c *FileCoordinator) observe(ctx context.Context, tx *sql.Tx, id, phase string, paths []string) error {
	for _, value := range paths {
		if err := canonicalMutationPath(value, false); err != nil {
			return err
		}
		present, err := c.source.FileExists(value)
		if err != nil {
			return err
		}
		var digest any
		var size any
		if present {
			length, err := c.source.FileSize(value)
			if err != nil {
				return err
			}
			reader, err := c.source.Reader(value)
			if err != nil {
				return err
			}
			stop := context.AfterFunc(ctx, func() { _ = reader.Close() })
			ref, copyErr := c.vault.Put(ctx, reader, length)
			stop()
			if err = errors.Join(copyErr, reader.Close(), ctx.Err()); err != nil {
				return err
			}
			digest, size = ref.SHA256, ref.Size
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.mutation_observations VALUES($1,$2,$3,$4,$5,$6)`, id, phase, value, present, digest, size); err != nil {
			return err
		}
	}
	return nil
}

func (c *FileCoordinator) complete(ctx context.Context, id string, observed []string, result string) error {
	tx, err := c.fence.beginControl(ctx, true)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	phase := "after"
	if result == "recovered" {
		phase = "recovery"
	}
	if err = c.observe(ctx, tx, id, phase, observed); err != nil {
		return err
	}
	if err = c.sealAvailableFiles(ctx, tx, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_legal_hold.mutation_completions(intent_id,result) VALUES($1,$2)`, id, result); err != nil {
		return err
	}
	return tx.Commit()
}

// A preparing case may have captured metadata while an upload was still
// creating its declared derivatives. Creating an absent path retains before
// and after observations; seal the metadata once all declared paths exist.
// Missing objects leave it pending. Other errors keep the physical intent.
func (c *FileCoordinator) sealAvailableFiles(ctx context.Context, tx *sql.Tx, intentID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT file_id FROM workspace_legal_hold.mutation_files WHERE intent_id=$1 ORDER BY file_id`, intentID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range ids {
		evidence, err := readFileEvidence(ctx, tx, id)
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrPreservationPending) {
			return err
		}
		var metadata struct {
			Path          string `json:"path"`
			PreviewPath   string `json:"previewpath"`
			ThumbnailPath string `json:"thumbnailpath"`
		}
		if err = json.Unmarshal(evidence.Metadata, &metadata); err != nil {
			return err
		}
		ready := metadata.Path != ""
		for _, value := range []string{metadata.Path, metadata.PreviewPath, metadata.ThumbnailPath} {
			if value == "" {
				continue
			}
			present, err := c.source.FileExists(value)
			if err != nil {
				return err
			}
			ready = ready && present
		}
		if ready {
			if _, err = preservePendingFileTx(ctx, tx, id, c.source, c.vault); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *FileCoordinator) recoverLocked(ctx context.Context) error {
	var id, archive string
	var paths []byte
	err := c.fence.db.QueryRowContext(ctx, `SELECT i.id,i.archive_id,i.observed_paths FROM workspace_legal_hold.mutation_pending p
		JOIN workspace_legal_hold.mutation_intents i ON i.id=p.intent_id`).Scan(&id, &archive, &paths)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if archive != c.archive {
		return errors.New("legal hold pending intent belongs to a different archive")
	}
	// Do not clear the SQL fence merely because the current live state can be
	// archived. The durable before images must still be readable and valid.
	if err = c.verifyBefore(ctx, id); err != nil {
		return err
	}
	var observed []string
	if err = json.Unmarshal(paths, &observed); err != nil {
		return err
	}
	return c.complete(ctx, id, observed, "recovered")
}

func (c *FileCoordinator) verifyBefore(ctx context.Context, id string) error {
	rows, err := c.fence.db.QueryContext(ctx, `SELECT sha256,size FROM workspace_legal_hold.mutation_observations
		WHERE intent_id=$1 AND phase='before' AND present`, id)
	if err != nil {
		return err
	}
	var objects []BlobRef
	for rows.Next() {
		var ref BlobRef
		if err = rows.Scan(&ref.SHA256, &ref.Size); err != nil {
			break
		}
		objects = append(objects, ref)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, ref := range objects {
		if err = ctx.Err(); err != nil {
			return err
		}
		file, err := c.vault.OpenVerified(ref)
		if err != nil {
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
	}
	return nil
}

const mutationLockPath = "physical-mutations.lock"

func (v *LocalVault) mutationLease(ctx context.Context) (*os.File, error) {
	file, err := v.root.OpenFile(mutationLockPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	created := err == nil
	if errors.Is(err, os.ErrExist) {
		file, err = v.root.OpenFile(mutationLockPath, os.O_RDWR, 0)
	}
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	entry, err := v.root.Lstat(mutationLockPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !os.SameFile(info, entry) || entry.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("legal hold coordination inode is not a private regular file")
	}
	if created {
		if err = errors.Join(file.Sync(), v.syncDirectory(".")); err != nil {
			return nil, err
		}
	}
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		acquired, err := tryMutationFileLock(file)
		if err != nil {
			return nil, err
		}
		if acquired {
			ok = true
			return file, nil
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (v *LocalVault) archiveIdentity() (string, error) {
	const identityPath = "archive-id"
	file, err := v.root.Open(identityPath)
	if errors.Is(err, os.ErrNotExist) {
		var random [16]byte
		if _, err = rand.Read(random[:]); err != nil {
			return "", err
		}
		identity := hex.EncodeToString(random[:])
		staging := "staging/identity-" + identity
		out, err := v.root.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
		if err != nil {
			return "", err
		}
		defer v.root.Remove(staging)
		_, writeErr := io.WriteString(out, identity)
		err = errors.Join(writeErr, out.Sync(), out.Close())
		if err != nil {
			return "", err
		}
		if err = v.root.Link(staging, identityPath); err != nil {
			return "", err
		}
		if err = v.syncDirectory("."); err != nil {
			return "", err
		}
		file, err = v.root.Open(identityPath)
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := v.root.Lstat(identityPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("legal hold archive identity is not a private regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, 33))
	if err != nil {
		return "", err
	}
	decoded, err := hex.DecodeString(string(data))
	if err != nil || len(decoded) != 16 || strings.ToLower(string(data)) != string(data) {
		return "", fmt.Errorf("invalid legal hold archive identity")
	}
	return string(data), nil
}
