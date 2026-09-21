package blobs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

var ErrChecksumMismatch = errors.New("uploaded data does not match the declared sha256")

type Dir struct {
	root    string
	staging string
}

func Open(root string) (*Dir, error) {
	dir := &Dir{root: filepath.Join(root, "blobs"), staging: filepath.Join(root, "staging")}
	if err := os.MkdirAll(dir.root, 0o750); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(dir.staging); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir.staging, 0o750); err != nil {
		return nil, err
	}
	return dir, nil
}

func (d *Dir) Path(blobID string) string {
	return filepath.Join(d.root, blobID)
}

func (d *Dir) Store(blobID string, expectedSha256 string, source io.Reader) (int64, error) {
	staged, err := os.CreateTemp(d.staging, blobID+"-*")
	if err != nil {
		return 0, err
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath)
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(staged, hasher), source)
	if err != nil {
		staged.Close()
		return 0, err
	}
	if err := staged.Sync(); err != nil {
		staged.Close()
		return 0, err
	}
	if err := staged.Close(); err != nil {
		return 0, err
	}
	if hex.EncodeToString(hasher.Sum(nil)) != expectedSha256 {
		return 0, ErrChecksumMismatch
	}
	return size, os.Rename(stagedPath, d.Path(blobID))
}

func (d *Dir) Remove(blobID string) error {
	return os.Remove(d.Path(blobID))
}

func (d *Dir) Sweep(referenced map[string]bool) (int, error) {
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || referenced[entry.Name()] {
			continue
		}
		if err := os.Remove(d.Path(entry.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
