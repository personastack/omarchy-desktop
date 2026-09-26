//go:build linux && !amd64

package desktopfiles

import (
	"errors"
)

func renameNoReplace(source, destination string) error {
	return errors.New("no-replace rename is supported only on Linux x86_64")
}
