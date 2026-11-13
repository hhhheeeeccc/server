package audit_file

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadPageTraversesEntireFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	var contents strings.Builder
	for i := 0; i < 257; i++ {
		fmt.Fprintf(&contents, "{\"sequence\":%d}\n", i)
	}
	require.NoError(t, os.WriteFile(path, []byte(contents.String()), 0o600))

	var got []int
	var cursor string
	for {
		page, err := readPage(path, 37, cursor)
		require.NoError(t, err)
		for _, record := range page.Records {
			var item struct {
				Sequence int `json:"sequence"`
			}
			require.NoError(t, json.Unmarshal(record, &item))
			got = append(got, item.Sequence)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	require.Len(t, got, 257)
	for index, sequence := range got {
		require.Equal(t, 256-index, sequence)
	}
}

func TestReadPageSkipsInvalidLinesAndContinuesPastScanLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	contents := "{\"sequence\":1}\n" + strings.Repeat(strings.Repeat("x", 1<<20)+"\n", 9) + "invalid\n{\"sequence\":2}\n"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	first, err := readPage(path, 1, "")
	require.NoError(t, err)
	require.Len(t, first.Records, 1)
	require.NotEmpty(t, first.NextCursor)

	var got []int
	var cursor = first.NextCursor
	for cursor != "" {
		page, readErr := readPage(path, 1, cursor)
		require.NoError(t, readErr)
		for _, record := range page.Records {
			var item struct {
				Sequence int `json:"sequence"`
			}
			require.NoError(t, json.Unmarshal(record, &item))
			got = append(got, item.Sequence)
		}
		cursor = page.NextCursor
	}
	require.Equal(t, []int{1}, got)
}

func TestReadPageCursorSurvivesAppendAndDetectsReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	require.NoError(t, os.WriteFile(path, []byte("{\"sequence\":1}\n{\"sequence\":2}\n"), 0o600))
	page, err := readPage(path, 1, "")
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = file.WriteString("{\"sequence\":3}\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	previous, err := readPage(path, 1, page.NextCursor)
	require.NoError(t, err)
	require.JSONEq(t, "{\"sequence\":1}", string(previous.Records[0]))

	require.NoError(t, os.WriteFile(path, []byte("{\"sequence\":1}\n{\"replacement\":true}\n"), 0o600))
	_, err = readPage(path, 1, page.NextCursor)
	require.True(t, errors.Is(err, errStaleCursor))
}

func TestReadPageRejectsRotatedFileWithMatchingHeadAndTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	head := strings.Repeat("{\"head\":true}\n", 6000)
	tail := strings.Repeat("{\"tail\":true}\n", 6000)
	require.NoError(t, os.WriteFile(path, []byte(head+`{"middle":"old"}`+"\n"+tail), 0o600))
	page, err := readPage(path, 1, "")
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor)
	require.NoError(t, os.Rename(path, filepath.Join(dir, "audit.log.1")))
	require.NoError(t, os.WriteFile(path, []byte(head+`{"middle":"new"}`+"\n"+tail), 0o600))
	_, err = readPage(path, 1, page.NextCursor)
	require.ErrorIs(t, err, errStaleCursor)
}

func TestReadPageEmptyMissingAndInvalidCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	page, err := readPage(path, 100, "")
	require.NoError(t, err)
	require.Empty(t, page.Records)
	require.Empty(t, page.NextCursor)

	require.NoError(t, os.WriteFile(path, nil, 0o600))
	page, err = readPage(path, 100, "")
	require.NoError(t, err)
	require.Empty(t, page.Records)

	_, err = readPage(path, 100, "not-a-cursor")
	require.True(t, errors.Is(err, errStaleCursor))
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	_, err = readPage(path, 100, "not-a-cursor")
	require.True(t, errors.Is(err, errInvalidCursor))
}

func TestReadPageDoesNotSilentlySkipOversizedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	require.NoError(t, os.WriteFile(path, []byte("{\"value\":\""+strings.Repeat("x", maxRecordBytes)+"\"}\n"), 0o600))
	first, err := readPage(path, 100, "")
	require.NoError(t, err)
	require.Empty(t, first.Records)
	require.NotEmpty(t, first.NextCursor)
	_, err = readPage(path, 100, first.NextCursor)
	require.ErrorIs(t, err, errRecordTooLarge)
}

func TestReadFilteredPageSearchesAcrossPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	var contents strings.Builder
	for i := 0; i < 200; i++ {
		label := "ordinary"
		if i == 3 || i == 75 || i == 198 {
			label = "TARGET"
		}
		fmt.Fprintf(&contents, "{\"sequence\":%d,\"event_name\":%q}\n", i, label)
	}
	require.NoError(t, os.WriteFile(path, []byte(contents.String()), 0o600))

	var got []int
	var cursor string
	for {
		page, err := readFilteredPage(path, 2, cursor, " target ")
		require.NoError(t, err)
		for _, record := range page.Records {
			var item struct {
				Sequence int `json:"sequence"`
			}
			require.NoError(t, json.Unmarshal(record, &item))
			got = append(got, item.Sequence)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	require.Equal(t, []int{198, 75, 3}, got)

	first, err := readFilteredPage(path, 1, "", "target")
	require.NoError(t, err)
	_, err = readFilteredPage(path, 1, first.NextCursor, "ordinary")
	require.ErrorIs(t, err, errInvalidCursor)
	_, err = readFilteredPage(path, 1, "", strings.Repeat("x", 1025))
	require.ErrorIs(t, err, errInvalidCursor)
}
