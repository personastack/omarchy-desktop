//go:build !linux

package desktopfiles

import (
	"os"
	"syscall"
)

func preservePlatformMetadata(sourceFD, destinationFD int, original os.FileInfo, mode os.FileMode) error {
	stat, ok := original.Sys().(*syscall.Stat_t)
	if !ok {
		return ErrNotRegularFile
	}
	if err := syscall.Fchown(destinationFD, int(stat.Uid), int(stat.Gid)); err != nil {
		return err
	}
	return syscall.Fchmod(destinationFD, unixFileMode(mode))
}

func platformMetadataUnchanged(before, after os.FileInfo) bool {
	return before.Mode() == after.Mode()
}
