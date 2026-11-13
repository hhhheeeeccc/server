package compliance_service

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const dailyFileLockProcessEnv = "WORKSPACE_TEST_COMPLIANCE_FILE_LOCK_PROCESS"

func runDailyFileLockProcess(path string) int {
	err := func() error {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		locked, err := tryDailyFileLock(file)
		if err != nil {
			return err
		}
		if err := json.NewEncoder(os.Stdout).Encode(struct {
			PID    int
			Locked bool
		}{os.Getpid(), locked}); err != nil {
			return err
		}
		if !locked {
			return nil
		}
		var release [1]byte
		_, err = io.ReadFull(os.Stdin, release[:])
		return err
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
