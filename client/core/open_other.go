//go:build !windows

package core

import "os"

func openForRead(path string) (*os.File, error) {
	return os.Open(path)
}
