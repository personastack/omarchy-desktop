//go:build !linux

package desktopfiles

import (
	"errors"
	"os"
)

func renameNoReplace(source, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return ErrDestinationExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(source, destination)
}
