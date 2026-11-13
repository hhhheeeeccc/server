//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd && !windows

package compliance_service

import (
	"errors"
	"os"
)

func tryDailyFileLock(*os.File) (bool, error) {
	return false, errors.New("daily compliance export requires supported shared file locking")
}
