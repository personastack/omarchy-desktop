//go:build linux && amd64

package cuainstaller

import (
	"runtime"
	"syscall"
	"unsafe"
)

func renameNoReplace(oldPath, newPath string) error {
	oldPointer, err := syscall.BytePtrFromString(oldPath)
	if err != nil {
		return err
	}
	newPointer, err := syscall.BytePtrFromString(newPath)
	if err != nil {
		return err
	}
	atCurrentWorkingDirectory := -100
	const noReplace = 1
	_, _, errno := syscall.Syscall6(
		316, // renameat2 on Linux amd64
		uintptr(atCurrentWorkingDirectory),
		uintptr(unsafe.Pointer(oldPointer)),
		uintptr(atCurrentWorkingDirectory),
		uintptr(unsafe.Pointer(newPointer)),
		noReplace,
		0,
	)
	runtime.KeepAlive(oldPointer)
	runtime.KeepAlive(newPointer)
	if errno != 0 {
		return errno
	}
	return nil
}
