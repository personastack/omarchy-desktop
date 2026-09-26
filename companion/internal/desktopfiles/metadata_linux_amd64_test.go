//go:build linux && amd64

package desktopfiles

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestReplacePreservesTimesAndPortableInodeFlags(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "metadata.txt")
	if err := os.WriteFile(path, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	wantAccess := time.Date(2020, 2, 3, 4, 5, 6, 123456789, time.UTC)
	wantModify := wantAccess.Add(7 * time.Hour)
	if err := os.Chtimes(path, wantAccess, wantModify); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	var beforeFlags int64
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), fsIOCGetFlags, uintptr(unsafe.Pointer(&beforeFlags)))
	_ = syscall.Close(fd)
	if errno == syscall.ENOTTY || errno == syscall.EOPNOTSUPP || errno == syscall.ENOSYS {
		t.Skip("test filesystem does not support inode flags")
	}
	if errno != 0 {
		t.Fatal(errno)
	}
	flags := beforeFlags | fsNoDumpFlag
	fd, err = syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), fsIOCSetFlags, uintptr(unsafe.Pointer(&flags)))
	_ = syscall.Close(fd)
	if errno == syscall.EPERM || errno == syscall.EACCES || errno == syscall.EOPNOTSUPP {
		t.Skip("test filesystem does not permit changing inode flags")
	}
	if errno != 0 {
		t.Fatal(errno)
	}
	defer func() {
		clearInodeFlag(t, path, fsNoDumpFlag)
	}()
	if _, err = Write(path, []byte("after"), WriteReplace, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Atim.Sec != wantAccess.Unix() || stat.Atim.Nsec != int64(wantAccess.Nanosecond()) || stat.Mtim.Sec != wantModify.Unix() || stat.Mtim.Nsec != int64(wantModify.Nanosecond()) {
		t.Fatalf("times changed: atime=%v mtime=%v", info.ModTime(), time.Unix(stat.Atim.Sec, stat.Atim.Nsec))
	}
	fd, err = syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	var afterFlags int64
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), fsIOCGetFlags, uintptr(unsafe.Pointer(&afterFlags)))
	_ = syscall.Close(fd)
	if errno != 0 || afterFlags&fsNoDumpFlag == 0 {
		t.Fatalf("inode flags = %#x, ioctl error = %v", afterFlags, errno)
	}
}

func TestReplaceRejectsMetadataOnlyConcurrentChanges(t *testing.T) {
	t.Parallel()
	t.Run("mode", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "mode.txt")
		if err := os.WriteFile(path, []byte("same"), 0o640); err != nil {
			t.Fatal(err)
		}
		original, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if err = os.Chtimes(path, original.ModTime(), original.ModTime()); err != nil {
			t.Fatal(err)
		}
		if err = replaceFile(path, []byte("stale"), original.Mode(), original); !errors.Is(err, ErrPatchMismatch) {
			t.Fatalf("replace after mode-only change = %v", err)
		}
	})
	t.Run("xattr", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "xattr.txt")
		if err := os.WriteFile(path, []byte("same"), 0o640); err != nil {
			t.Fatal(err)
		}
		original, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = syscall.Setxattr(path, "user.concurrent-test", []byte("changed"), 0); err != nil {
			if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
				t.Skip("test filesystem does not support extended attributes")
			}
			t.Fatal(err)
		}
		if err = replaceFile(path, []byte("stale"), original.Mode(), original); !errors.Is(err, ErrPatchMismatch) {
			t.Fatalf("replace after xattr-only change = %v", err)
		}
		value := make([]byte, 16)
		n, err := syscall.Getxattr(path, "user.concurrent-test", value)
		if err != nil || string(value[:n]) != "changed" {
			t.Fatalf("concurrent xattr = %q, %v", value[:n], err)
		}
	})
}

func clearInodeFlag(t *testing.T, path string, flag int64) {
	t.Helper()
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		t.Errorf("open to clear inode flag: %v", err)
		return
	}
	defer syscall.Close(fd)
	var flags int64
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), fsIOCGetFlags, uintptr(unsafe.Pointer(&flags)))
	if errno != 0 {
		t.Errorf("read inode flags for cleanup: %v", errno)
		return
	}
	flags &^= flag
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), fsIOCSetFlags, uintptr(unsafe.Pointer(&flags)))
	if errno != 0 {
		t.Errorf("clear inode flag: %v", errno)
	}
}
