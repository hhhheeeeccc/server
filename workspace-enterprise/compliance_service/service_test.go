package compliance_service

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"io"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestWriteComplianceArchive(t *testing.T) {
	var output bytes.Buffer
	createAt := int64(1000)
	post := &model.CompliancePost{
		TeamName:       "team",
		ChannelName:    "channel",
		UserUsername:   "user",
		PostId:         "post-id",
		PostCreateAt:   createAt,
		PostUpdateAt:   createAt,
		PostMessage:    "message",
		PostFileIds:    "[]",
		UserEmail:      "",
		PostDeleteAt:   0,
		PostOriginalId: "",
	}
	calls := 0
	count, err := writeComplianceArchive(&output, "report.csv", 1, func(cursor model.ComplianceExportCursor, _ int) ([]*model.CompliancePost, model.ComplianceExportCursor, error) {
		calls++
		if calls == 1 {
			cursor.ChannelsQueryCompleted = true
			return []*model.CompliancePost{post}, cursor, nil
		}
		cursor.DirectMessagesQueryCompleted = true
		return nil, cursor, nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, count)

	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	require.NoError(t, err)
	require.Len(t, reader.File, 1)
	require.Equal(t, "report.csv", reader.File[0].Name)

	file, err := reader.File[0].Open()
	require.NoError(t, err)
	defer file.Close()
	records, err := csv.NewReader(file).ReadAll()
	require.NoError(t, err)
	require.Equal(t, model.CompliancePostHeader(), records[0])
	require.Equal(t, post.Row(), records[1])
}

func TestWriteComplianceArchiveRejectsCursorWithoutProgress(t *testing.T) {
	_, err := writeComplianceArchive(io.Discard, "report.csv", 10, func(cursor model.ComplianceExportCursor, _ int) ([]*model.CompliancePost, model.ComplianceExportCursor, error) {
		return nil, cursor, nil
	})
	require.EqualError(t, err, "compliance export cursor made no progress")
}

func TestWriteComplianceArchiveRejectsPostsWithoutCursorProgress(t *testing.T) {
	calls := 0
	count, err := writeComplianceArchive(io.Discard, "report.csv", 1, func(cursor model.ComplianceExportCursor, _ int) ([]*model.CompliancePost, model.ComplianceExportCursor, error) {
		calls++
		return []*model.CompliancePost{{PostId: "same-post"}}, cursor, nil
	})
	require.EqualError(t, err, "compliance export cursor made no progress")
	require.Zero(t, count)
	require.Equal(t, 1, calls)
}
