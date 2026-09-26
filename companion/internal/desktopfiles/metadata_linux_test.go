//go:build linux

package desktopfiles

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReplacePreservesLinuxXattrsAndOwnership(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "with-xattr.txt")
	if err := os.WriteFile(path, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	name := "user.personastack-test"
	if err := syscall.Setxattr(path, name, []byte("retained"), 0); err != nil {
		if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
			t.Skip("test filesystem does not support extended attributes")
		}
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Write(path, []byte("after"), WriteReplace, nil); err != nil {
		t.Fatal(err)
	}
	value := make([]byte, 32)
	n, err := syscall.Getxattr(path, name, value)
	if err != nil || string(value[:n]) != "retained" {
		t.Fatalf("extended attribute = %q, %v", value[:n], err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Sys().(*syscall.Stat_t).Uid != after.Sys().(*syscall.Stat_t).Uid || before.Sys().(*syscall.Stat_t).Gid != after.Sys().(*syscall.Stat_t).Gid {
		t.Fatalf("owner changed: before=%#v after=%#v", before.Sys(), after.Sys())
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("mode changed: before=%#o after=%#o", before.Mode().Perm(), after.Mode().Perm())
	}
}

func TestReplacePreservesSpecialModeBits(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "setuid.txt")
	if err := os.WriteFile(path, []byte("before"), 0o4751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o4751); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode()&os.ModeSetuid == 0 {
		t.Skip("test filesystem does not expose setuid mode bits")
	}
	if _, err := Write(path, []byte("after"), WriteReplace, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o751 || info.Mode()&os.ModeSetuid == 0 {
		t.Fatalf("replacement mode = %#o, before=%#o", info.Mode(), before.Mode())
	}
}

func TestUnixFileModeMapsSpecialBits(t *testing.T) {
	t.Parallel()
	mode := os.FileMode(0o751) | os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	if got := unixFileMode(mode); got != 0o7751 {
		t.Fatalf("Unix mode = %#o", got)
	}
}
