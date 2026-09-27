package cuainstaller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

var fixtureNotices = []noticeFile{
	{name: "THIRD-PARTY-NOTICES/CUA-NODE-NOTICE.md", content: []byte("Cua Node notice")},
	{name: "THIRD-PARTY-NOTICES/MPL-2.0.txt", content: []byte("MPL license")},
}

func TestInstallerVerifiesAndReusesPinnedRuntime(t *testing.T) {
	t.Parallel()
	archive, selected := fixtureRelease(t, []archiveEntry{
		{name: "cua-driver", mode: 0o755, content: []byte("driver binary")},
		{name: "wayland-helper/", directory: true},
		{name: "wayland-helper/install.sh", mode: 0o755, content: []byte("install helper")},
	})
	client := &fakeHTTP{body: archive}
	root := filepath.Join(t.TempDir(), "cua", selected.version)
	installer, err := newInstallerForRelease(root, "linux", "amd64", client, selected, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	executable, err := installer.Install(context.Background())
	if err != nil {
		t.Fatalf("install verified runtime: %v", err)
	}
	if executable != filepath.Join(root, "cua-driver") || client.calls != 1 {
		t.Fatalf("installed executable/call count = %q/%d", executable, client.calls)
	}
	if err := installer.Verify(); err != nil {
		t.Fatalf("verify installed runtime: %v", err)
	}
	info, err := os.Stat(executable)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("executable mode = %v, %v", info, err)
	}
	if _, err := installer.Install(context.Background()); err != nil || client.calls != 1 {
		t.Fatalf("repeat install did not reuse verified runtime: %v, calls=%d", err, client.calls)
	}
}

func TestRepairReplacesDamagedFilesInOwnedTree(t *testing.T) {
	t.Parallel()
	archive, selected := fixtureRelease(t, []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("pinned driver")}})
	client := &fakeHTTP{body: archive}
	root := filepath.Join(t.TempDir(), "cua", selected.version)
	installer, err := newInstallerForRelease(root, "linux", "amd64", client, selected, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	executable, err := installer.Install(context.Background())
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if err := os.WriteFile(executable, []byte("damaged"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := installer.Repair(context.Background()); err != nil {
		t.Fatalf("Repair() error = %v", err)
	}
	if err := installer.Verify(); err != nil {
		t.Fatalf("Verify() after repair = %v", err)
	}
	content, err := os.ReadFile(executable)
	if err != nil || string(content) != "pinned driver" || client.calls != 2 {
		t.Fatalf("repaired executable=%q err=%v download calls=%d", content, err, client.calls)
	}
}

func TestRepairPublishesBeforeRemovingReadOnlyOwnedDirectories(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	archive, selected := fixtureRelease(t, []archiveEntry{
		{name: "cua-driver", mode: 0o755, content: []byte("pinned driver")},
		{name: "wayland-helper/", directory: true},
		{name: "wayland-helper/install.sh", mode: 0o755, content: []byte("install helper")},
	})
	root := filepath.Join(t.TempDir(), "cua", selected.version)
	installer, err := newInstallerForRelease(root, "linux", "amd64", &fakeHTTP{body: archive}, selected, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	if _, err := installer.Install(context.Background()); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "cua-driver"), []byte("damaged"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "cua-driver"), 0); err != nil {
		t.Fatal(err)
	}
	readOnlyDirectory := filepath.Join(root, "wayland-helper")
	if err := os.Chmod(readOnlyDirectory, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := installer.Repair(context.Background()); err != nil {
		t.Fatalf("Repair() error = %v", err)
	}
	if err := installer.Verify(); err != nil {
		t.Fatalf("Verify() after repair = %v", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "cua-driver"))
	if err != nil || string(content) != "pinned driver" {
		t.Fatalf("repaired executable = %q, %v", content, err)
	}
}

func TestRepairRestoresOwnedTreeWhenPublishingStageFails(t *testing.T) {
	t.Parallel()
	archive, selected := fixtureRelease(t, []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("pinned driver")}})
	root := filepath.Join(t.TempDir(), "cua", selected.version)
	installer, err := newInstallerForRelease(root, "linux", "amd64", &fakeHTTP{body: archive}, selected, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	executable, err := installer.Install(context.Background())
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if err := os.WriteFile(executable, []byte("damaged"), 0o700); err != nil {
		t.Fatal(err)
	}
	publishErr := errors.New("injected stage publication failure")
	originalRename := installer.rename
	failed := false
	installer.rename = func(oldPath, newPath string) error {
		if filepath.Base(oldPath) != filepath.Base(root) && newPath == root && !failed {
			failed = true
			return publishErr
		}
		return originalRename(oldPath, newPath)
	}
	if _, err := installer.Repair(context.Background()); !errors.Is(err, publishErr) {
		t.Fatalf("Repair() error = %v, want injected publication error", err)
	}
	marker := filepath.Join(root, ".personastack-cua.json")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("failed publish did not restore owned tree marker: %v", err)
	}
	installer.rename = originalRename
	if _, err := installer.Repair(context.Background()); err != nil {
		t.Fatalf("retry Repair() after publication failure = %v", err)
	}
	if err := installer.Verify(); err != nil {
		t.Fatalf("Verify() after retry = %v", err)
	}
}

func TestRepairVerifiesPublishedTreeBeforeRemovingBackup(t *testing.T) {
	t.Parallel()
	archive, selected := fixtureRelease(t, []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("pinned driver")}})
	root := filepath.Join(t.TempDir(), "cua", selected.version)
	installer, err := newInstallerForRelease(root, "linux", "amd64", &fakeHTTP{body: archive}, selected, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	executable, err := installer.Install(context.Background())
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if err := os.WriteFile(executable, []byte("damaged"), 0o700); err != nil {
		t.Fatal(err)
	}
	originalRename := installer.rename
	installer.rename = func(oldPath, newPath string) error {
		if err := originalRename(oldPath, newPath); err != nil {
			return err
		}
		if filepath.Base(oldPath) != filepath.Base(root) && newPath == root {
			return os.WriteFile(executable, []byte("changed during publication"), 0o700)
		}
		return nil
	}
	if _, err := installer.Repair(context.Background()); err == nil {
		t.Fatal("Repair() succeeded with a changed published executable")
	}
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(root), ".cua-driver-repair-*"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("preserved repair backups = %v, err = %v", backups, err)
	}
	if err := verifyTreeMode(context.Background(), backups[0], selected, fixtureNotices, false, true); err != nil {
		t.Fatalf("original owned tree was not preserved: %v", err)
	}
	oldExecutable, err := os.ReadFile(filepath.Join(backups[0], "cua-driver"))
	if err != nil || string(oldExecutable) != "damaged" {
		t.Fatalf("original damaged executable = %q, err = %v", oldExecutable, err)
	}
}

func TestRepairPreservesUnexpectedFilesAddedDuringSwap(t *testing.T) {
	t.Parallel()
	archive, selected := fixtureRelease(t, []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("pinned driver")}})
	root := filepath.Join(t.TempDir(), "cua", selected.version)
	installer, err := newInstallerForRelease(root, "linux", "amd64", &fakeHTTP{body: archive}, selected, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	executable, err := installer.Install(context.Background())
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if err := os.WriteFile(executable, []byte("damaged"), 0o700); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(root, "user-data")
	originalRename := installer.rename
	added := false
	installer.rename = func(oldPath, newPath string) error {
		if oldPath == root && newPath != root {
			if err := os.WriteFile(extra, []byte("preserve"), 0o600); err != nil {
				return err
			}
			added = true
		}
		return originalRename(oldPath, newPath)
	}
	if _, err := installer.Repair(context.Background()); !errors.Is(err, ErrForeignInstall) {
		t.Fatalf("Repair() error = %v, want changed tree rejection", err)
	}
	if !added {
		t.Fatal("repair did not exercise the post-verification tree change")
	}
	content, err := os.ReadFile(extra)
	if err != nil || string(content) != "preserve" {
		t.Fatalf("unexpected file after failed repair = %q, %v", content, err)
	}
	installer.rename = originalRename
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	if _, err := installer.Repair(context.Background()); err != nil {
		t.Fatalf("retry Repair() after removing test file = %v", err)
	}
}

func TestRepairCompletesWhenContextCancelsDuringPublication(t *testing.T) {
	t.Parallel()
	archive, selected := fixtureRelease(t, []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("pinned driver")}})
	root := filepath.Join(t.TempDir(), "cua", selected.version)
	installer, err := newInstallerForRelease(root, "linux", "amd64", &fakeHTTP{body: archive}, selected, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	executable, err := installer.Install(context.Background())
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if err := os.WriteFile(executable, []byte("damaged"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	originalRename := installer.rename
	installer.rename = func(oldPath, newPath string) error {
		if filepath.Base(oldPath) != filepath.Base(root) && newPath == root {
			cancel()
		}
		return originalRename(oldPath, newPath)
	}
	if _, err := installer.Repair(ctx); err != nil {
		t.Fatalf("Repair() after cancellation during atomic publication = %v", err)
	}
	if err := installer.Verify(); err != nil {
		t.Fatalf("Verify() after canceled publication = %v", err)
	}
}

func TestRepairRefusesUnknownFilesInManagedDirectory(t *testing.T) {
	t.Parallel()
	archive, selected := fixtureRelease(t, []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("pinned driver")}})
	client := &fakeHTTP{body: archive}
	root := filepath.Join(t.TempDir(), "cua", selected.version)
	installer, err := newInstallerForRelease(root, "linux", "amd64", client, selected, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	if _, err := installer.Install(context.Background()); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	extra := filepath.Join(root, "user-data")
	if err := os.WriteFile(extra, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installer.Repair(context.Background()); !errors.Is(err, ErrForeignInstall) {
		t.Fatalf("Repair() error = %v, want foreign install", err)
	}
	content, err := os.ReadFile(extra)
	if err != nil || string(content) != "preserve" || client.calls != 1 {
		t.Fatalf("unknown file=%q err=%v download calls=%d", content, err, client.calls)
	}
}

func TestInstallerAcceptsPinnedArchiveRootDirectory(t *testing.T) {
	t.Parallel()
	archive, selected := fixtureReleaseWithPrefix(t, []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("driver")}}, "fixture-root/")
	installer, err := newInstallerForRelease(filepath.Join(t.TempDir(), "cua", selected.version), "linux", "amd64", &fakeHTTP{body: archive}, selected, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	if _, err := installer.Install(context.Background()); err != nil {
		t.Fatalf("install archive with root directory: %v", err)
	}
}

func TestInstallerRejectsBadArchiveChecksumWithoutWriting(t *testing.T) {
	t.Parallel()
	archive, selected := fixtureRelease(t, []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("driver")}})
	selected.archiveSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	root := filepath.Join(t.TempDir(), "cua", selected.version)
	installer, err := newInstallerForRelease(root, "linux", "amd64", &fakeHTTP{body: archive}, selected, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	if _, err := installer.Install(context.Background()); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("checksum mismatch error = %v", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("installer wrote the rejected archive: %v", err)
	}
}

func TestInstallerHonorsCancellationAfterDownloadBeforeStaging(t *testing.T) {
	t.Parallel()
	archive, selected := fixtureRelease(t, []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("driver binary")}})
	root := filepath.Join(t.TempDir(), "cua", selected.version)
	ctx, cancel := context.WithCancel(context.Background())
	installer, err := newInstallerForRelease(root, "linux", "amd64", &cancelAfterReadHTTP{body: archive, cancel: cancel}, selected, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	if _, err := installer.Install(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Install() cancellation error = %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled install created its staging parent: %v", err)
	}
}

func TestExtractVerifiedRejectsCanceledContextBeforeWriting(t *testing.T) {
	t.Parallel()
	archive, selected := fixtureRelease(t, []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("driver binary")}})
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := extractVerified(ctx, archive, root, selected); !errors.Is(err, context.Canceled) {
		t.Fatalf("extractVerified() error = %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("canceled extraction wrote entries=%v error=%v", entries, err)
	}
}

func TestWritePrivateFileReturnsCancellationDuringChunkedWrite(t *testing.T) {
	t.Parallel()
	ctx := &cancelOnSecondCheck{done: make(chan struct{})}
	path := filepath.Join(t.TempDir(), "runtime-file")
	content := bytes.Repeat([]byte("x"), 128<<10)
	if err := writePrivateFile(ctx, path, content, 0o600); !errors.Is(err, context.Canceled) {
		t.Fatalf("writePrivateFile() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 64<<10 {
		t.Fatalf("partial file size = %v, %v", info, err)
	}
}

type cancelOnSecondCheck struct {
	checks int
	done   chan struct{}
}

func (c *cancelOnSecondCheck) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelOnSecondCheck) Done() <-chan struct{}       { return c.done }
func (c *cancelOnSecondCheck) Value(any) any               { return nil }
func (c *cancelOnSecondCheck) Err() error {
	c.checks++
	if c.checks == 2 {
		close(c.done)
	}
	if c.checks >= 2 {
		return context.Canceled
	}
	return nil
}

func TestInstallerRejectsUnsafeTarEntries(t *testing.T) {
	t.Parallel()
	archive, selected := fixtureRelease(t, []archiveEntry{{name: "../cua-driver", mode: 0o755, content: []byte("driver")}})
	root := filepath.Join(t.TempDir(), "cua", selected.version)
	installer, err := newInstallerForRelease(root, "linux", "amd64", &fakeHTTP{body: archive}, selected, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	if _, err := installer.Install(context.Background()); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("unsafe archive error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(root), "cua-driver")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe entry escaped staging directory: %v", err)
	}
}

func TestInstallerDetectsModifiedRuntimeFiles(t *testing.T) {
	t.Parallel()
	archive, selected := fixtureRelease(t, []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("driver binary")}})
	root := filepath.Join(t.TempDir(), "cua", selected.version)
	installer, err := newInstallerForRelease(root, "linux", "amd64", &fakeHTTP{body: archive}, selected, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	executable, err := installer.Install(context.Background())
	if err != nil {
		t.Fatalf("install verified runtime: %v", err)
	}
	if err := os.Chmod(executable, 0o600); err != nil {
		t.Fatal("change executable mode")
	}
	if err := installer.Verify(); !errors.Is(err, ErrForeignInstall) {
		t.Fatalf("changed executable mode verification = %v", err)
	}
	if err := os.Chmod(executable, 0o700); err != nil {
		t.Fatal("restore executable mode")
	}
	if err := os.WriteFile(executable, []byte("replacement"), 0o700); err != nil {
		t.Fatal("change executable contents")
	}
	if err := installer.Verify(); !errors.Is(err, ErrForeignInstall) {
		t.Fatalf("changed executable verification = %v", err)
	}
}

func TestInstallerRefusesForeignVersionDirectory(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "cua", "0.29.1")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal("create foreign runtime directory")
	}
	if err := os.WriteFile(filepath.Join(root, "user-data"), []byte("keep"), 0o600); err != nil {
		t.Fatal("write foreign runtime data")
	}
	installer, err := newInstallerForRelease(root, "linux", "amd64", &fakeHTTP{}, pinnedRelease, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	if _, err := installer.Install(context.Background()); !errors.Is(err, ErrForeignInstall) {
		t.Fatalf("foreign directory error = %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(root, "user-data")); err != nil || string(contents) != "keep" {
		t.Fatalf("foreign directory was changed: %q, %v", contents, err)
	}
}

func TestInstallerDoesNotReplaceConcurrentDestination(t *testing.T) {
	t.Parallel()
	for _, destination := range []string{"directory", "symlink"} {
		t.Run(destination, func(t *testing.T) {
			t.Parallel()
			archive, selected := fixtureRelease(t, []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("driver")}})
			root := filepath.Join(t.TempDir(), "cua", selected.version)
			installer, err := newInstallerForRelease(root, "linux", "amd64", &fakeHTTP{body: archive}, selected, fixtureNotices)
			if err != nil {
				t.Fatal("create fixture installer")
			}
			installer.rename = func(oldPath, newPath string) error {
				if destination == "directory" {
					if err := os.Mkdir(newPath, 0o700); err != nil {
						return err
					}
					if err := os.WriteFile(filepath.Join(newPath, "owner-data"), []byte("keep"), 0o600); err != nil {
						return err
					}
				} else if err := os.Symlink("/dev/null", newPath); err != nil {
					return err
				}
				return renameNoReplace(oldPath, newPath)
			}
			if _, err := installer.Install(context.Background()); err == nil {
				t.Fatal("install replaced concurrent destination")
			}
			if destination == "directory" {
				if contents, err := os.ReadFile(filepath.Join(root, "owner-data")); err != nil || string(contents) != "keep" {
					t.Fatalf("foreign destination changed: %q, %v", contents, err)
				}
			} else {
				if target, err := os.Readlink(root); err != nil || target != "/dev/null" {
					t.Fatalf("foreign symlink changed: %q, %v", target, err)
				}
			}
		})
	}
}

func TestInstallerRequiresLinuxAMD64AndAbsoluteRoot(t *testing.T) {
	t.Parallel()
	if _, err := newInstallerForRelease("/tmp/cua", "darwin", "amd64", &fakeHTTP{}, pinnedRelease, fixtureNotices); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("non-Linux platform error = %v", err)
	}
	if _, err := newInstallerForRelease("relative/path", "linux", "amd64", &fakeHTTP{}, pinnedRelease, fixtureNotices); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("relative root error = %v", err)
	}
}

func TestInstallerRejectsUnsafeOwnershipMarkers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(string) error
	}{
		{name: "symlink", mutate: func(path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Symlink("/dev/null", path)
		}},
		{name: "fifo", mutate: func(path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return syscall.Mkfifo(path, 0o600)
		}},
		{name: "oversized", mutate: func(path string) error { return os.WriteFile(path, bytes.Repeat([]byte("x"), maximumMarker+1), 0o600) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			installer, root := installedFixture(t)
			if err := test.mutate(filepath.Join(root, ".personastack-cua.json")); err != nil {
				t.Fatal("mutate ownership marker")
			}
			if err := installer.Verify(); !errors.Is(err, ErrForeignInstall) {
				t.Fatalf("unsafe marker verification = %v", err)
			}
		})
	}
}

func TestInstallerVerifiesThirdPartyNotices(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"LICENSE", "THIRD-PARTY-NOTICES/CUA-NODE-NOTICE.md", "THIRD-PARTY-NOTICES/MPL-2.0.txt"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			installer, root := installedFixture(t)
			filePath := filepath.Join(root, filepath.FromSlash(path))
			if err := os.WriteFile(filePath, []byte("altered notice"), 0o600); err != nil {
				t.Fatal("alter installed notice")
			}
			if err := installer.Verify(); !errors.Is(err, ErrForeignInstall) {
				t.Fatalf("altered notice verification = %v", err)
			}
		})
	}
}

func TestInstallerRejectsUnsafeAncestors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		make func(string) error
	}{
		{name: "symlink", make: func(path string) error {
			target := path + "-target"
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			return os.Symlink(target, path)
		}},
		{name: "group and world writable", make: func(path string) error {
			if err := os.Mkdir(path, 0o700); err != nil {
				return err
			}
			return os.Chmod(path, 0o777)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			parent := filepath.Join(t.TempDir(), "unsafe")
			if err := test.make(parent); err != nil {
				t.Fatal("create unsafe ancestor")
			}
			root := filepath.Join(parent, "cua", "0.30.1")
			installer, err := newInstallerForRelease(root, "linux", "amd64", &fakeHTTP{}, pinnedRelease, fixtureNotices)
			if err != nil {
				t.Fatal("create fixture installer")
			}
			if _, err := installer.Install(context.Background()); !errors.Is(err, ErrForeignInstall) {
				t.Fatalf("unsafe ancestor install = %v", err)
			}
		})
	}
}

func TestInstallerBoundsUnexpectedDirectoryEntries(t *testing.T) {
	t.Parallel()
	installer, root := installedFixture(t)
	for index := range 1024 {
		path := filepath.Join(root, "unexpected-"+strconv.Itoa(index))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal("create unexpected entry")
		}
	}
	if err := installer.Verify(); !errors.Is(err, ErrForeignInstall) {
		t.Fatalf("unexpected tree verification = %v", err)
	}
}

func TestInstallerRejectsNonregularInstalledFiles(t *testing.T) {
	t.Parallel()
	installer, root := installedFixture(t)
	runtimePath := filepath.Join(root, "cua-driver")
	if err := os.Remove(runtimePath); err != nil {
		t.Fatal("remove runtime file")
	}
	if err := syscall.Mkfifo(runtimePath, 0o600); err != nil {
		t.Fatal("replace runtime with fifo")
	}
	if err := installer.Verify(); !errors.Is(err, ErrForeignInstall) {
		t.Fatalf("nonregular runtime verification = %v", err)
	}
}

func TestInstallerRejectsMalformedArchives(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries []archiveEntry
		edit    func(*release)
	}{
		{name: "symlink", entries: []archiveEntry{{name: "cua-driver", mode: 0o755, typeflag: tar.TypeSymlink, linkname: "target"}}},
		{name: "hardlink", entries: []archiveEntry{{name: "cua-driver", mode: 0o755, typeflag: tar.TypeLink, linkname: "target"}}},
		{name: "duplicate", entries: []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("duplicate")}, {name: "cua-driver", mode: 0o755, content: []byte("duplicate")}}},
		{name: "unexpected", entries: []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("driver")}, {name: "other", mode: 0o600, content: []byte("unexpected")}}, edit: func(selected *release) { delete(selected.assetHashes, "other") }},
		{name: "missing", entries: []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("driver")}}, edit: func(selected *release) {
			selected.assetHashes["absent"] = "0000000000000000000000000000000000000000000000000000000000000000"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			archive, selected := fixtureRelease(t, test.entries)
			if test.edit != nil {
				test.edit(&selected)
			}
			installer, err := newInstallerForRelease(filepath.Join(t.TempDir(), "cua", selected.version), "linux", "amd64", &fakeHTTP{body: archive}, selected, fixtureNotices)
			if err != nil {
				t.Fatal("create fixture installer")
			}
			if _, err := installer.Install(context.Background()); !errors.Is(err, ErrInvalidArchive) {
				t.Fatalf("malformed archive error = %v", err)
			}
		})
	}
}

func TestInstallerRejectsUnsuccessfulAndTruncatedDownloads(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		status int
		body   []byte
	}{
		{name: "http status", status: http.StatusNotFound},
		{name: "truncated gzip", status: http.StatusOK, body: []byte("not a gzip archive")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			archive, selected := fixtureRelease(t, []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("driver")}})
			if test.body != nil {
				archive = test.body
				selected.archiveSHA256 = digest(archive)
			}
			installer, err := newInstallerForRelease(filepath.Join(t.TempDir(), "cua", selected.version), "linux", "amd64", &fakeHTTP{body: archive, status: test.status}, selected, fixtureNotices)
			if err != nil {
				t.Fatal("create fixture installer")
			}
			if _, err := installer.Install(context.Background()); err == nil {
				t.Fatal("installer accepted unsuccessful or malformed response")
			}
		})
	}
}

type archiveEntry struct {
	name      string
	mode      int64
	content   []byte
	directory bool
	typeflag  byte
	linkname  string
}

func fixtureRelease(t *testing.T, entries []archiveEntry) ([]byte, release) {
	return fixtureReleaseWithPrefix(t, entries, "")
}

func fixtureReleaseWithPrefix(t *testing.T, entries []archiveEntry, prefix string) ([]byte, release) {
	t.Helper()
	entries = append(entries, archiveEntry{name: "LICENSE", mode: 0o600, content: []byte("Cua license")})
	var archiveBuffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&archiveBuffer)
	tarWriter := tar.NewWriter(gzipWriter)
	assets := make(map[string]string, len(entries))
	directories := make(map[string]struct{})
	if prefix != "" {
		header := &tar.Header{Name: prefix, Mode: 0o755, Typeflag: tar.TypeDir}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal("write fixture archive root")
		}
	}
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		archiveName := prefix + entry.name
		header := &tar.Header{Name: archiveName, Mode: entry.mode, Typeflag: typeflag, Linkname: entry.linkname, Size: int64(len(entry.content))}
		if entry.directory {
			header.Typeflag = tar.TypeDir
			header.Mode = 0o755
			header.Size = 0
			directories[filepath.ToSlash(filepath.Clean(entry.name))] = struct{}{}
		} else if typeflag == tar.TypeReg || typeflag == tar.TypeRegA {
			digest := sha256.Sum256(entry.content)
			assets[entry.name] = hex.EncodeToString(digest[:])
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal("write fixture tar header")
		}
		if !entry.directory && (typeflag == tar.TypeReg || typeflag == tar.TypeRegA) {
			if _, err := tarWriter.Write(entry.content); err != nil {
				t.Fatal("write fixture tar data")
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal("close fixture tar")
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal("close fixture gzip")
	}
	archive := append([]byte(nil), archiveBuffer.Bytes()...)
	digest := sha256.Sum256(archive)
	return archive, release{version: "test", archivePrefix: prefix, archiveURL: "https://example.invalid/cua.tar.gz", archiveSHA256: hex.EncodeToString(digest[:]), assetHashes: assets, directories: directories}
}

func installedFixture(t *testing.T) (*Installer, string) {
	t.Helper()
	archive, selected := fixtureRelease(t, []archiveEntry{{name: "cua-driver", mode: 0o755, content: []byte("driver binary")}})
	root := filepath.Join(t.TempDir(), "cua", selected.version)
	installer, err := newInstallerForRelease(root, "linux", "amd64", &fakeHTTP{body: archive}, selected, fixtureNotices)
	if err != nil {
		t.Fatal("create fixture installer")
	}
	if _, err := installer.Install(context.Background()); err != nil {
		t.Fatalf("install fixture: %v", err)
	}
	return installer, root
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type fakeHTTP struct {
	body   []byte
	err    error
	calls  int
	status int
}

type cancelAfterReadHTTP struct {
	body   []byte
	cancel context.CancelFunc
}

func (c *cancelAfterReadHTTP) Do(request *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(c.body)), Body: cancelAfterEOF{reader: bytes.NewReader(c.body), cancel: c.cancel}, Request: request}, nil
}

type cancelAfterEOF struct {
	reader *bytes.Reader
	cancel context.CancelFunc
}

func (r cancelAfterEOF) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	if errors.Is(err, io.EOF) {
		r.cancel()
	}
	return count, err
}

func (cancelAfterEOF) Close() error {
	return nil
}

func (c *fakeHTTP) Do(request *http.Request) (*http.Response, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, ContentLength: int64(len(c.body)), Body: io.NopCloser(bytes.NewReader(c.body)), Request: request}, nil
}
