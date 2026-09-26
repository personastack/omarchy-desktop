package desktopfiles

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestListPagesAndReportsSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	entries, next, err := List(root, 0, 2)
	if err != nil || len(entries) != 2 || next == nil || *next != 2 {
		t.Fatalf("first page: %#v %v %v", entries, next, err)
	}
	entries, next, err = List(root, *next, 2)
	if err != nil || len(entries) != 2 || next != nil {
		t.Fatalf("second page: %#v %v %v", entries, next, err)
	}
	link, err := Metadata(filepath.Join(root, "link"))
	if err != nil {
		t.Fatal(err)
	}
	target, _ := filepath.EvalSymlinks(filepath.Join(root, "a.txt"))
	if link.Kind != "symlink" || link.SymlinkTarget != target {
		t.Fatalf("symlink metadata: %#v", link)
	}
}

func TestMetadataReportsDanglingSymlinkTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink("missing/deep-target", link); err != nil {
		t.Fatal(err)
	}
	entry, err := Metadata(link)
	want := resolveExistingPrefix(filepath.Join(root, "missing", "deep-target"))
	if err != nil || entry.Kind != "symlink" || entry.SymlinkTarget != want {
		t.Fatalf("dangling symlink metadata = %#v, %v; want target %q", entry, err, want)
	}
}

func TestOpenReadLinesAndDetectChanges(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lines.txt")
	content := []byte("one\nλambda\nlast")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	files := New()
	opened, err := files.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if opened.ContentText != string(content) || opened.Encoding != "utf-8" || opened.Size != int64(len(content)) || opened.NextLine != 4 {
		t.Fatalf("open result: %#v", opened)
	}
	line, err := files.ReadLines(opened.Handle, 2, 1, 5)
	if err != nil || line.ContentText != "λamb" || !line.Truncated || line.NextLine != 2 {
		t.Fatalf("line page: %#v %v", line, err)
	}
	if err = os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := files.Read(opened.Handle, 0, MaxReadBytes)
	if err != nil || !read.ChangedSinceOpen || read.ContentText != "changed" {
		t.Fatalf("changed read: %#v %v", read, err)
	}
	if err = files.Close(opened.Handle); err != nil {
		t.Fatal(err)
	}
	if _, err = files.Read(opened.Handle, 0, 1); !errors.Is(err, ErrMissingHandle) {
		t.Fatalf("read after close error = %v", err)
	}
}

func TestReadPreservesBinaryAndUTF8ByteRanges(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	binaryPath := filepath.Join(root, "raw.bin")
	if err := os.WriteFile(binaryPath, []byte{0, 0xff, 'x'}, 0o600); err != nil {
		t.Fatal(err)
	}
	files := New()
	opened, err := files.Open(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Encoding != "base64" || opened.ContentText != "" {
		t.Fatalf("binary was decoded as text: %#v", opened.Read)
	}
	decoded, err := base64.StdEncoding.DecodeString(opened.ContentBase64)
	if err != nil || string(decoded) != string([]byte{0, 0xff, 'x'}) {
		t.Fatalf("binary content = %v, %v", decoded, err)
	}
	_ = files.Close(opened.Handle)

	textPath := filepath.Join(root, "utf8.txt")
	if err = os.WriteFile(textPath, []byte("aλz"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err = files.Open(textPath)
	if err != nil {
		t.Fatal(err)
	}
	page, err := files.Read(opened.Handle, 0, 2)
	if err != nil || page.ContentText != "a" || page.NextOffset != 1 {
		t.Fatalf("aligned page: %#v %v", page, err)
	}
}

func TestWritePatchMoveAndRemove(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	created := filepath.Join(root, "created.txt")
	if _, err := Write(created, []byte("first"), WriteCreate, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(created, []byte("duplicate"), WriteCreate, nil); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("duplicate create = %v", err)
	}
	if _, err := Write(created, []byte("!"), WriteAppend, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Patch(created, "first", "updated"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(created); string(got) != "updated!" {
		t.Fatalf("patched content = %q", got)
	}
	if _, err := Patch(created, "updated", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := Patch(created, "absent", "y"); !errors.Is(err, ErrPatchMismatch) {
		t.Fatalf("missing patch match = %v", err)
	}
	destination := filepath.Join(root, "moved.txt")
	if err := Move(created, destination); err != nil {
		t.Fatal(err)
	}
	if err := Move(destination, destination); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("existing destination = %v", err)
	}
	if err := Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := Remove("/"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("root removal = %v", err)
	}
}

func TestReplacePreservesModeAndRefusesSymlinkTargets(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(target, []byte("new"), WriteReplace, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("replace mode = %v, %v", info, err)
	}
	link := filepath.Join(root, "link.txt")
	if err = os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err = Write(link, []byte("!"), WriteAppend, nil); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("append through symlink = %v", err)
	}
	if _, err = Patch(link, "new", "bad"); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("patch through symlink = %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "new" {
		t.Fatalf("symlink target changed: %q, %v", got, err)
	}
}

func TestReplaceRejectsConcurrentInPlaceChange(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "changed.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("newer"), 0o600); err != nil {
		t.Fatal(err)
	}
	changedTime := original.ModTime().Add(time.Second)
	if err = os.Chtimes(path, changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	if err = replaceFile(path, []byte("stale"), original.Mode().Perm(), original); !errors.Is(err, ErrPatchMismatch) {
		t.Fatalf("stale replace error = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "newer" {
		t.Fatalf("concurrent contents = %q, %v", got, err)
	}
}

func TestPatchRejectsOversizedResultBeforeReplacement(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Patch(path, "x", strings.Repeat("y", MaxPatchSize+1)); !errors.Is(err, ErrContentTooLarge) {
		t.Fatalf("oversized patch error = %v", err)
	}
}

func TestFailedSearchDoesNotRetainContinuationState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "unreadable.txt")
	if err := os.WriteFile(path, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o600)
	files := New()
	for i := 0; i < maxSearches+1; i++ {
		if _, err := files.Search(root, "", "", "secret", 1, ""); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("failing search %d = %v", i, err)
		}
	}
}

func TestSearchContinuationCanOnlyAdvanceOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"alpha.txt", "another.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("hit"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files := New()
	page, err := files.Search(root, "a", "", "", 1, "")
	if err != nil || page.Continuation == "" {
		t.Fatalf("initial search = %#v, %v", page, err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, callErr := files.Search(root, "a", "", "", 1, page.Continuation)
			errs <- callErr
		}()
	}
	close(start)
	workers.Wait()
	close(errs)
	succeeded, expired := 0, 0
	for callErr := range errs {
		if callErr == nil {
			succeeded++
		}
		if errors.Is(callErr, ErrSearchContinuationExpired) {
			expired++
		}
	}
	if succeeded != 1 || expired != 1 {
		t.Fatalf("continuation outcomes: success=%d expired=%d", succeeded, expired)
	}
}

func TestOpenFileCapacityAndCloseAll(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	files := New()
	ids := make([]string, 0, MaxOpenFiles)
	for i := 0; i < MaxOpenFiles+1; i++ {
		path := filepath.Join(root, fmt.Sprintf("%02d.txt", i))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		opened, err := files.Open(path)
		if i == MaxOpenFiles {
			if !errors.Is(err, ErrTooManyOpenFiles) {
				t.Fatalf("open over capacity = %v", err)
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, opened.Handle)
	}
	files.CloseAll()
	for _, id := range ids {
		if _, err := files.Read(id, 0, 1); !errors.Is(err, ErrMissingHandle) {
			t.Fatalf("read after CloseAll = %v", err)
		}
	}
}

func TestSearchPaginatesAndReportsIncompleteContent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := filepath.Join(root, "a.txt")
	if err := os.WriteFile(first, []byte("hit on line one\nother\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(root, "b.txt")
	content := append([]byte("hit\n"), bytes.Repeat([]byte("x"), MaxPatchSize)...)
	if err := os.WriteFile(large, content, 0o600); err != nil {
		t.Fatal(err)
	}
	files := New()
	page, err := files.Search(root, "", "", "hit", 1, "")
	resolvedFirst, _ := filepath.EvalSymlinks(first)
	if err != nil || len(page.Matches) != 1 || page.Matches[0].Entry.Path != resolvedFirst || page.Continuation == "" {
		t.Fatalf("first search page: %#v, %v", page, err)
	}
	continuation := page.Continuation
	page, err = files.Search(root, "", "", "hit", 1, continuation)
	resolvedLarge, _ := filepath.EvalSymlinks(large)
	if err != nil || len(page.Matches) != 1 || page.Matches[0].Entry.Path != resolvedLarge || !page.Matches[0].ContentScanTruncated || len(page.IncompleteContentPaths) != 1 {
		t.Fatalf("second search page: %#v, %v", page, err)
	}
	if _, err = files.Search(root, "different", "", "hit", 1, continuation); !errors.Is(err, ErrSearchContinuationExpired) {
		t.Fatalf("search continuation with changed query = %v", err)
	}
}

func TestOpenRejectsSpecialFilesAndUnsafePaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New().Open(fifo); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("FIFO open error = %v", err)
	}
	for _, path := range []string{"", "relative", "/bad\x00path", "/" + strings.Repeat("a", MaxPathBytes)} {
		if _, err := Metadata(path); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Metadata(%q) error = %v", path, err)
		}
	}
}
