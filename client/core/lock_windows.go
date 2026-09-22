//go:build windows

package core

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func LockDir(dir string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(dir, "app.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	overlapped := &windows.Overlapped{}
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if err != nil {
		file.Close()
		return nil, ErrAlreadyRunning
	}
	return func() {
		windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
		file.Close()
	}, nil
}
