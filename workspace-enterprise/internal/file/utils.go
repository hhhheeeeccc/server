// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.enterprise for license information.

package file

import (
	"os"

	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

// DeleteTemp removes a temporary file and records cleanup failures.
func DeleteTemp(logger mlog.LoggerIFace, temporaryFile *os.File) {
	if err := temporaryFile.Close(); err != nil {
		logger.Warn("Failed to close temporary file", mlog.String("filename", temporaryFile.Name()), mlog.Err(err))
	}
	if err := os.Remove(temporaryFile.Name()); err != nil {
		logger.Warn("Failed to delete temporary file", mlog.String("filename", temporaryFile.Name()), mlog.Err(err))
	}
}
