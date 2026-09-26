//go:build linux && amd64

package desktopfiles

import (
	"syscall"
	"unsafe"
)

const atFDCWD = ^uintptr(99)
const renameNoReplaceFlag = 1
const sysRenameAt2AMD64 = 316

func renameNoReplace(source, destination string) error {
	oldPath, err := syscall.BytePtrFromString(source)
	if err != nil {
		return err
	}
	newPath, err := syscall.BytePtrFromString(destination)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(sysRenameAt2AMD64, atFDCWD, uintptr(unsafe.Pointer(oldPath)),
		atFDCWD, uintptr(unsafe.Pointer(newPath)), renameNoReplaceFlag, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
