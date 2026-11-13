//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package legal_hold

import (
	"errors"
	"os"
)

func tryMutationFileLock(*os.File) (bool, error) {
	return false, errors.New("legal hold file coordination requires supported shared file locking")
}
