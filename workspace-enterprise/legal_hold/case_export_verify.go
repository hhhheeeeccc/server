package legal_hold

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
)

// VerifyCaseExport checks the entire package, all JSONL datasets, declared
// counts/hashes, and every byte object referenced by receipts/observations.
// This is integrity verification, not an origin signature. For provenance use
// the package hash anchored in the trusted SQL outcome (as Open does).
func VerifyCaseExport(ctx context.Context, src io.ReaderAt, size int64) (manifest CaseExportManifest, err error) {
	defer func() {
		// ZIP may report corruption while opening an individual entry, not
		// only at archive creation or at our own hash/count checks. Keep a
		// consistent integrity classification through every decoder path.
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			err = errors.Join(ErrIntegrity, err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return manifest, err
	}
	zr, err := zip.NewReader(src, size)
	if err != nil {
		return manifest, errors.Join(ErrIntegrity, err)
	}
	entries := map[string]*zip.File{}
	for _, file := range zr.File {
		if entries[file.Name] != nil || !file.Mode().IsRegular() || file.Method != zip.Store {
			return manifest, ErrIntegrity
		}
		entries[file.Name] = file
	}
	entry := entries["manifest.json"]
	if entry == nil || entry.UncompressedSize64 > 64*1024 {
		return manifest, ErrIntegrity
	}
	reader, err := entry.Open()
	if err != nil {
		return manifest, err
	}
	decoder := json.NewDecoder(vaultContextReader{ctx, reader})
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&manifest)
	var extra any
	if err == nil && !errors.Is(decoder.Decode(&extra), io.EOF) {
		err = ErrIntegrity
	}
	if err = errors.Join(err, reader.Close()); err != nil {
		return manifest, errors.Join(ErrIntegrity, err)
	}
	if manifest.Version != 1 || !validExportIDs(manifest) || manifest.CapturedAt.IsZero() || len(manifest.Datasets) != len(exportDatasets) {
		return manifest, ErrIntegrity
	}
	delete(entries, "manifest.json")
	required := map[string]BlobRef{}
	listed := map[string]BlobRef{}
	addRef := func(ref BlobRef, dest map[string]BlobRef) error {
		path, err := blobPath(ref)
		if err != nil {
			return err
		}
		if previous, ok := dest[path]; ok && previous != ref {
			return ErrIntegrity
		}
		dest[path] = ref
		return nil
	}
	for index, dataset := range manifest.Datasets {
		if dataset.Path != exportDatasets[index].name || dataset.Rows < 0 || dataset.Size < 0 || len(dataset.SHA256) != 64 {
			return manifest, ErrIntegrity
		}
		file := entries[dataset.Path]
		if file == nil || file.UncompressedSize64 != uint64(dataset.Size) {
			return manifest, ErrIntegrity
		}
		reader, err := file.Open()
		if err != nil {
			return manifest, err
		}
		hash := sha256.New()
		decoder := json.NewDecoder(io.TeeReader(vaultContextReader{ctx, reader}, hash))
		var count int64
		for {
			var row json.RawMessage
			if err = decoder.Decode(&row); errors.Is(err, io.EOF) {
				err = nil
				break
			} else if err != nil {
				break
			}
			count++
			switch dataset.Path {
			case "file_receipts.jsonl":
				var receipt struct {
					Manifest []PreservedFileObject `json:"manifest"`
				}
				if err = json.Unmarshal(row, &receipt); err == nil {
					for _, object := range receipt.Manifest {
						if err = addRef(object.BlobRef, required); err != nil {
							break
						}
					}
				}
			case "mutation_observations.jsonl":
				var observation struct {
					Present bool `json:"present"`
					BlobRef
				}
				if err = json.Unmarshal(row, &observation); err == nil && observation.Present {
					err = addRef(observation.BlobRef, required)
				}
			case "objects.jsonl":
				var ref BlobRef
				if err = json.Unmarshal(row, &ref); err == nil {
					path, pathErr := blobPath(ref)
					if pathErr != nil || listed[path].SHA256 != "" {
						err = ErrIntegrity
					} else {
						err = addRef(ref, listed)
					}
				}
			case "case.jsonl":
				var item struct {
					ID string `json:"id"`
				}
				if err = json.Unmarshal(row, &item); err == nil && item.ID != manifest.CaseID {
					err = ErrIntegrity
				}
			}
			if err != nil {
				break
			}
		}
		err = errors.Join(err, reader.Close())
		if err == nil && (count != dataset.Rows || hex.EncodeToString(hash.Sum(nil)) != dataset.SHA256 || dataset.Path == "case.jsonl" && count != 1) {
			err = ErrIntegrity
		}
		if err != nil {
			return manifest, fmt.Errorf("legal hold dataset %s: %w", dataset.Path, errors.Join(ErrIntegrity, err))
		}
		delete(entries, dataset.Path)
	}
	if len(required) != len(listed) {
		return manifest, ErrIntegrity
	}
	for name, ref := range required {
		if listed[name] != ref {
			return manifest, ErrIntegrity
		}
		entry := entries[name]
		if entry == nil || entry.UncompressedSize64 != uint64(ref.Size) {
			return manifest, ErrIntegrity
		}
		reader, err := entry.Open()
		if err != nil {
			return manifest, err
		}
		hash := sha256.New()
		n, err := io.CopyBuffer(hash, vaultContextReader{ctx, reader}, make([]byte, 64*1024))
		err = errors.Join(err, reader.Close())
		if err == nil && (n != ref.Size || hex.EncodeToString(hash.Sum(nil)) != ref.SHA256) {
			err = ErrIntegrity
		}
		if err != nil {
			return manifest, errors.Join(ErrIntegrity, err)
		}
		delete(entries, name)
	}
	if len(entries) != 0 {
		return manifest, ErrIntegrity
	}
	return manifest, ctx.Err()
}

func validExportIDs(manifest CaseExportManifest) bool {
	if !model.IsValidId(manifest.ExportID) || !model.IsValidId(manifest.CaseID) || !model.IsValidId(manifest.ActorID) || len(manifest.ArchiveID) != 32 {
		return false
	}
	if release := manifest.Release; release != nil {
		if !model.IsValidId(release.ActorID) || !model.IsValidId(release.RequestKey) || !release.ReleasedAt.Equal(manifest.CapturedAt) || !utf8.ValidString(release.Reason) || utf8.RuneCountInString(release.Reason) < 1 || utf8.RuneCountInString(release.Reason) > 2048 {
			return false
		}
	}
	_, err := hex.DecodeString(manifest.ArchiveID)
	return err == nil
}
