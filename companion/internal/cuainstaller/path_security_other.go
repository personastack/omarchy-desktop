//go:build !linux

package cuainstaller

import (
	"errors"
	"os"
	"path/filepath"
)

func ensurePrivateDirectoryPath(path string, create bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrForeignInstall
	}
	if create {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrForeignInstall
	}
	return nil
}

func safeDirectoryOwner(os.FileInfo) bool { return true }

func currentUserOwnsDirectory(os.FileInfo) bool { return true }
