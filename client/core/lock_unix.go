//go:build !windows

package core

import (
	"os"
	"path/filepath"
	"syscall"
)

func LockDir(dir string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(dir, "app.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, ErrAlreadyRunning
	}
	return func() {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}, nil
}
