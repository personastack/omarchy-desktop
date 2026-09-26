//go:build linux && !amd64

package desktopfiles

import (
	"errors"
	"os"
)

func preservePlatformMetadata(sourceFD, destinationFD int, original os.FileInfo, mode os.FileMode) error {
	return errors.New("metadata-preserving file replacement is supported only on Linux x86_64")
}

func platformMetadataUnchanged(before, after os.FileInfo) bool {
	return before.Mode() == after.Mode()
}
