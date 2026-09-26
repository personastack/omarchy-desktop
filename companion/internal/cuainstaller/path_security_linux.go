//go:build linux

package cuainstaller

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func ensurePrivateDirectoryPath(path string, create bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrForeignInstall
	}
	current := string(filepath.Separator)
	components := strings.Split(strings.Trim(path, string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && create {
			err = os.Mkdir(current, 0o700)
			if errors.Is(err, os.ErrExist) {
				err = nil
			}
			if err == nil {
				info, err = os.Lstat(current)
			}
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !safeDirectoryOwner(info) {
			return ErrForeignInstall
		}
	}
	return nil
}

func safeDirectoryOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Getuid())) {
		return false
	}
	if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return false
	}
	return true
}

func currentUserOwnsDirectory(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}
