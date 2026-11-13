package audit_file

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadRecordsReturnsNewestValidRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	contents := "{\"sequence\":1}\nnot-json\n{\"sequence\":2}\n{\"sequence\":3}\n"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	records, err := readRecords(path, 2)
	require.NoError(t, err)
	require.Len(t, records, 2)

	var newest map[string]int
	require.NoError(t, json.Unmarshal(records[0], &newest))
	require.Equal(t, 3, newest["sequence"])

	var previous map[string]int
	require.NoError(t, json.Unmarshal(records[1], &previous))
	require.Equal(t, 2, previous["sequence"])
}

func TestReadRecordsEmptyFileReturnsArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	records, err := readRecords(path, defaultRecords)
	require.NoError(t, err)
	require.NotNil(t, records)
	require.Empty(t, records)
}

func TestReadRecordsMissingFileReturnsArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	records, err := readRecords(path, defaultRecords)
	require.NoError(t, err)
	require.NotNil(t, records)
	require.Empty(t, records)
}

func TestAuditFileStatusDistinguishesMissingFileFromInvalidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	hasRecords, err := auditFileHasRecords(path)
	require.NoError(t, err)
	require.False(t, hasRecords)
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	hasRecords, err = auditFileHasRecords(path)
	require.NoError(t, err)
	require.False(t, hasRecords)
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	hasRecords, err = auditFileHasRecords(path)
	require.NoError(t, err)
	require.True(t, hasRecords)
	_, err = auditFileHasRecords(dir)
	require.ErrorContains(t, err, "not a regular file")
}

func TestValidatedPathRejectsOutsideRootAndSymlink(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MM_LOG_PATH", root)
	inside := filepath.Join(root, "audit.log")
	outside := filepath.Join(t.TempDir(), "secret.log")
	require.NoError(t, os.WriteFile(inside, []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(outside, []byte("{}\n"), 0o600))
	_, err := validatedPath(inside)
	require.NoError(t, err)
	_, err = validatedPath(outside)
	require.Error(t, err)
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "linked.log")))
	_, err = validatedPath(filepath.Join(root, "linked.log"))
	require.Error(t, err)
}
