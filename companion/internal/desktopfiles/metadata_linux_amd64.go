//go:build linux && amd64

package desktopfiles

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const maxCopiedXattrBytes = 1024 * 1024

const (
	fsIOCGetFlags     = 0x80086601
	fsIOCSetFlags     = 0x40086602
	fsSyncFlag        = 0x00000008
	fsImmutableFlag   = 0x00000010
	fsAppendFlag      = 0x00000020
	fsNoDumpFlag      = 0x00000040
	fsNoAtimeFlag     = 0x00000080
	fsExtentFlag      = 0x00080000
	fsFlagsToPreserve = fsSyncFlag | fsNoDumpFlag | fsNoAtimeFlag
)

func preservePlatformMetadata(sourceFD, destinationFD int, original os.FileInfo, mode os.FileMode) error {
	stat, ok := original.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("source file ownership is unavailable")
	}
	if err := syscall.Fchown(destinationFD, int(stat.Uid), int(stat.Gid)); err != nil {
		return err
	}
	if err := copyExtendedAttributes(sourceFD, destinationFD); err != nil {
		return err
	}
	if err := syscall.Fchmod(destinationFD, unixFileMode(mode)); err != nil {
		return err
	}
	if err := preserveFileTimes(destinationFD, stat); err != nil {
		return err
	}
	return preserveInodeFlags(sourceFD, destinationFD)
}

func platformMetadataUnchanged(before, after os.FileInfo) bool {
	beforeStat, beforeOK := before.Sys().(*syscall.Stat_t)
	afterStat, afterOK := after.Sys().(*syscall.Stat_t)
	return beforeOK && afterOK && before.Mode() == after.Mode() && beforeStat.Uid == afterStat.Uid && beforeStat.Gid == afterStat.Gid && beforeStat.Ctim == afterStat.Ctim
}

func preserveFileTimes(destinationFD int, stat *syscall.Stat_t) error {
	times := [2]syscall.Timespec{stat.Atim, stat.Mtim}
	emptyPath, err := syscall.BytePtrFromString("")
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_UTIMENSAT, uintptr(destinationFD), uintptr(unsafe.Pointer(emptyPath)), uintptr(unsafe.Pointer(&times[0])), 0x1000, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func preserveInodeFlags(sourceFD, destinationFD int) error {
	var sourceFlags int64
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(sourceFD), fsIOCGetFlags, uintptr(unsafe.Pointer(&sourceFlags)))
	if errno == syscall.ENOTTY || errno == syscall.EOPNOTSUPP || errno == syscall.ENOSYS {
		return nil
	}
	if errno != 0 {
		return errno
	}
	unsupported := sourceFlags &^ (fsFlagsToPreserve | fsExtentFlag)
	if unsupported != 0 || sourceFlags&(fsImmutableFlag|fsAppendFlag) != 0 {
		return ErrMetadataUnsupported
	}
	flags := sourceFlags & fsFlagsToPreserve
	if flags == 0 {
		return nil
	}
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(destinationFD), fsIOCSetFlags, uintptr(unsafe.Pointer(&flags)))
	if errno != 0 {
		return errno
	}
	return nil
}

func copyExtendedAttributes(sourceFD, destinationFD int) error {
	size, err := listFileXattrs(sourceFD, nil)
	if err != nil {
		if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
			return nil
		}
		return err
	}
	if size > maxCopiedXattrBytes {
		return ErrMetadataTooLarge
	}
	if size == 0 {
		return nil
	}
	names := make([]byte, size)
	readSize, err := listFileXattrs(sourceFD, names)
	if err != nil {
		return err
	}
	if readSize > len(names) {
		return ErrMetadataTooLarge
	}
	used := 0
	for _, name := range strings.Split(string(names[:readSize]), "\x00") {
		if name == "" {
			continue
		}
		namePointer, err := syscall.BytePtrFromString(name)
		if err != nil {
			return err
		}
		valueSize, err := getFileXattr(sourceFD, namePointer, nil)
		if err != nil {
			return err
		}
		if valueSize > maxCopiedXattrBytes-used {
			return ErrMetadataTooLarge
		}
		value := make([]byte, valueSize)
		actualSize, err := getFileXattr(sourceFD, namePointer, value)
		if err != nil {
			return err
		}
		if actualSize > len(value) {
			return ErrMetadataTooLarge
		}
		value = value[:actualSize]
		if err = setFileXattr(destinationFD, namePointer, value); err != nil {
			return err
		}
		used += len(value)
	}
	return nil
}

func listFileXattrs(fd int, value []byte) (int, error) {
	var pointer uintptr
	if len(value) > 0 {
		pointer = uintptr(unsafe.Pointer(&value[0]))
	}
	result, _, errno := syscall.Syscall(196, uintptr(fd), pointer, uintptr(len(value)))
	if errno != 0 {
		return 0, errno
	}
	return int(result), nil
}

func getFileXattr(fd int, name *byte, value []byte) (int, error) {
	var pointer uintptr
	if len(value) > 0 {
		pointer = uintptr(unsafe.Pointer(&value[0]))
	}
	result, _, errno := syscall.Syscall6(193, uintptr(fd), uintptr(unsafe.Pointer(name)), pointer, uintptr(len(value)), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(result), nil
}

func setFileXattr(fd int, name *byte, value []byte) error {
	var pointer uintptr
	if len(value) > 0 {
		pointer = uintptr(unsafe.Pointer(&value[0]))
	}
	_, _, errno := syscall.Syscall6(190, uintptr(fd), uintptr(unsafe.Pointer(name)), pointer, uintptr(len(value)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
