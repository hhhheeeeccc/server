// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.enterprise for license information.

package csv_export

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
	"testing"

	"github.com/mattermost/enterprise/message_export/shared"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
	"github.com/stretchr/testify/require"
)

type archiveAttachmentStore struct {
	shared.MessageExportStore
	files []*model.FileInfo
	calls int
}

func (s *archiveAttachmentStore) FileInfo() shared.MEFileInfoStore { return s }

func (s *archiveAttachmentStore) GetForPost(_ string, _, _, _ bool) ([]*model.FileInfo, error) {
	s.calls++
	return s.files, nil
}

type archiveAttachmentBackend struct {
	filestore.FileBackend
	reader func() (filestore.ReadCloseSeeker, error)
	calls  int
}

func (b *archiveAttachmentBackend) Reader(string) (filestore.ReadCloseSeeker, error) {
	b.calls++
	return b.reader()
}

type archiveCompressor struct {
	io.Writer
	writeErr error
	closeErr error
}

func (w archiveCompressor) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.Writer.Write(p)
}

func (w archiveCompressor) Close() error { return w.closeErr }

func archiveWithAttachment() (shared.ExportParams, *archiveAttachmentStore, *archiveAttachmentBackend) {
	store := &archiveAttachmentStore{files: []*model.FileInfo{{Id: "file", Path: "stored/report.txt"}}}
	backend := &archiveAttachmentBackend{reader: func() (filestore.ReadCloseSeeker, error) {
		return nil, errors.New("attachment unavailable")
	}}
	return shared.ExportParams{
		Posts: []*model.MessageExport{{PostId: model.NewPointer("post"), PostFileIds: []string{"file"}}},
		Db:    store, FileAttachmentBackend: backend,
	}, store, backend
}

func readArchive(t *testing.T, buffer *bytes.Buffer) map[string][]byte {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
	require.NoError(t, err)
	files := make(map[string][]byte, len(archive.File))
	for _, entry := range archive.File {
		r, err := entry.Open()
		require.NoError(t, err)
		data, readErr := io.ReadAll(r)
		require.NoError(t, r.Close())
		require.NoError(t, readErr)
		files[entry.Name] = data
	}
	return files
}

func TestCSVArchiveReportsWarningEntryCreationFailure(t *testing.T) {
	p, _, _ := archiveWithAttachment()
	zipFile := zip.NewWriter(io.Discard)
	failure := errors.New("previous entry could not close")
	zipFile.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) {
		return archiveCompressor{Writer: w, closeErr: failure}, nil
	})
	var err error
	require.NotPanics(t, func() {
		_, err = writeCSVArchive(request.TestContext(t), p, shared.GenericExportData{}, nil, zipFile)
	})
	require.ErrorIs(t, err, failure)
	require.ErrorContains(t, err, "warning")
}

func TestCSVArchiveStopsBeforeAttachmentReadsAfterCSVFlushFailure(t *testing.T) {
	p, store, backend := archiveWithAttachment()
	zipFile := zip.NewWriter(io.Discard)
	failure := errors.New("CSV destination unavailable")
	zipFile.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) {
		return archiveCompressor{Writer: w, writeErr: failure}, nil
	})
	_, err := writeCSVArchive(request.TestContext(t), p, shared.GenericExportData{}, nil, zipFile)
	t.Logf("attachment metadata reads=%d; backend reads=%d", store.calls, backend.calls)
	require.ErrorIs(t, err, failure)
	require.ErrorContains(t, err, "flush")
	require.Zero(t, store.calls)
	require.Zero(t, backend.calls)
}

func TestCSVArchivePreservesCSVAndMetadata(t *testing.T) {
	var buffer bytes.Buffer
	metadata := shared.Metadata{MessagesCount: 2, StartTime: 10, EndTime: 20}
	data := shared.GenericExportData{Metadata: metadata, Results: shared.RunExportResults{CreatedPosts: 2}}
	rows := []Row{{CreateAt: 11, PostId: "one", PostMessage: "رسالة, \"quoted\"\nline"}, {CreateAt: 12, PostId: "two", PostMessage: "second"}}
	results, err := writeCSVArchive(request.TestContext(t), shared.ExportParams{}, data, rows, zip.NewWriter(&buffer))
	require.NoError(t, err)
	require.Equal(t, data.Results, results)
	files := readArchive(t, &buffer)
	require.Len(t, files, 2)
	require.NotContains(t, files, CSVWarningFilename)
	records, err := csv.NewReader(bytes.NewReader(files["posts.csv"])).ReadAll()
	require.NoError(t, err)
	require.Len(t, records, 3)
	require.Len(t, records[0], 20)
	require.Equal(t, "Post Id", records[0][13])
	require.Equal(t, "Previews Post Id", records[0][19])
	require.Equal(t, []string{"one", "", "", rows[0].PostMessage, "", "", ""}, records[1][13:])
	require.Equal(t, "two", records[2][13])
	var decoded shared.Metadata
	require.NoError(t, json.Unmarshal(files["metadata.json"], &decoded))
	require.Equal(t, metadata, decoded)
}

func TestCSVArchivePreservesMissingAttachmentWarning(t *testing.T) {
	p, store, backend := archiveWithAttachment()
	var buffer bytes.Buffer
	results, err := writeCSVArchive(request.TestContext(t), p, shared.GenericExportData{}, nil, zip.NewWriter(&buffer))
	require.NoError(t, err)
	require.Equal(t, 1, results.NumWarnings)
	require.Equal(t, 1, store.calls)
	require.Equal(t, 1, backend.calls)
	files := readArchive(t, &buffer)
	require.Len(t, files, 3)
	require.Equal(t, "Warning:"+shared.MissingFileMessageDuringBackendRead+" - Post: post - stored/report.txt\n", string(files[CSVWarningFilename]))
	require.Contains(t, files, "posts.csv")
	require.Contains(t, files, "metadata.json")
}

func TestCSVArchiveStreamsAttachmentAndClosesReader(t *testing.T) {
	p, _, backend := archiveWithAttachment()
	// Larger than the copy buffer, with Unicode preserved in the archive bytes.
	content := strings.Repeat("المرفق-", 400000)
	reader := &archiveReadCloser{ReadSeeker: strings.NewReader(content)}
	backend.reader = func() (filestore.ReadCloseSeeker, error) { return reader, nil }
	var buffer bytes.Buffer
	results, err := writeCSVArchive(request.TestContext(t), p, shared.GenericExportData{}, nil, zip.NewWriter(&buffer))
	require.NoError(t, err)
	require.Zero(t, results.NumWarnings)
	require.Equal(t, 1, reader.closed)
	files := readArchive(t, &buffer)
	require.Len(t, files, 3)
	require.Equal(t, content, string(files[path.Join("files", "post", "file-report.txt")]))
	require.NotContains(t, files, CSVWarningFilename)
}

type archiveReadCloser struct {
	io.ReadSeeker
	closed int
}

func (r *archiveReadCloser) Close() error {
	r.closed++
	return nil
}

func TestCSVArchiveReportsFinalCloseFailure(t *testing.T) {
	failure := errors.New("metadata close failed")
	zipFile := zip.NewWriter(io.Discard)
	entries := 0
	zipFile.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) {
		entries++
		var closeErr error
		if entries == 2 {
			closeErr = failure
		}
		return archiveCompressor{Writer: w, closeErr: closeErr}, nil
	})
	_, err := writeCSVArchive(request.TestContext(t), shared.ExportParams{}, shared.GenericExportData{}, nil, zipFile)
	require.ErrorIs(t, err, failure)
	require.ErrorContains(t, err, "close")
}
