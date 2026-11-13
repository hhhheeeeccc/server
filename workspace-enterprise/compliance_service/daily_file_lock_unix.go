//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package compliance_service

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryDailyFileLock(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}
