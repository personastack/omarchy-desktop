package desktopfiles

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/personastack/omarchy-desktop/companion/internal/testfixture"
)

func TestSharedDesktopParityFileFixtures(t *testing.T) {
	t.Parallel()
	fixture, err := testfixture.LoadDesktopParity()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()

	boundary := fixture.Files.UTF8Boundary
	boundaryPath := filepath.Join(root, "boundary.txt")
	trailing, err := hex.DecodeString(boundary.TrailingBytesHex)
	if err != nil {
		t.Fatal(err)
	}
	boundaryData := append(bytes.Repeat([]byte{'a'}, boundary.ASCIIPrefixBytes), trailing...)
	if err := os.WriteFile(boundaryPath, boundaryData, 0o600); err != nil {
		t.Fatal(err)
	}
	files := New()
	opened, err := files.Open(boundaryPath)
	if err != nil || opened.ByteLength != boundary.FirstPageBytes || opened.NextOffset != int64(boundary.NextOffset) {
		t.Fatalf("UTF-8 boundary first page = %#v, %v", opened.Read, err)
	}
	tail, err := files.Read(opened.Handle, int64(boundary.NextOffset), MaxReadBytes)
	if err != nil || tail.ContentText != boundary.TailText {
		t.Fatalf("UTF-8 boundary tail = %#v, %v", tail, err)
	}
	_ = files.Close(opened.Handle)

	binaryCase := fixture.Files.Binary
	binaryData, err := hex.DecodeString(binaryCase.BytesHex)
	if err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(root, "binary.bin")
	if err := os.WriteFile(binaryPath, binaryData, 0o600); err != nil {
		t.Fatal(err)
	}
	binaryOpened, err := files.Open(binaryPath)
	if err != nil || binaryOpened.Encoding != binaryCase.Encoding || binaryOpened.ContentBase64 != binaryCase.Base64 {
		t.Fatalf("binary fixture result = %#v, %v", binaryOpened.Read, err)
	}
	_ = files.Close(binaryOpened.Handle)

	searchCase := fixture.Files.Search
	searchRoot := filepath.Join(root, "search")
	if err := os.Mkdir(searchRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, item := range searchCase.Files {
		if err := os.WriteFile(filepath.Join(searchRoot, item.Name), []byte(item.Content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var continuation *string
	for pageIndex, expected := range searchCase.Pages {
		page, err := files.Search(searchRoot, nil, &searchCase.NameGlob, nil, searchCase.Limit, continuation)
		if err != nil {
			t.Fatalf("search page %d: %v", pageIndex, err)
		}
		got := make([]string, len(page.Matches))
		for index, match := range page.Matches {
			got[index] = match.Entry.Name
		}
		if !equalStrings(got, expected) {
			t.Fatalf("search page %d = %v, want %v", pageIndex, got, expected)
		}
		if pageIndex < len(searchCase.Pages)-1 && page.Continuation == "" {
			t.Fatalf("search page %d omitted continuation", pageIndex)
		}
		if pageIndex == len(searchCase.Pages)-1 && page.Continuation != "" {
			t.Fatalf("final search fixture page retained continuation %q", page.Continuation)
		}
		continuation = nil
		if page.Continuation != "" {
			next := page.Continuation
			continuation = &next
		}
	}
	contentFilter := searchCase.ContentContains
	contentPage, err := files.Search(searchRoot, nil, nil, &contentFilter, len(searchCase.Files), nil)
	if err != nil {
		t.Fatal(err)
	}
	contentNames := make([]string, len(contentPage.Matches))
	for index, match := range contentPage.Matches {
		contentNames[index] = match.Entry.Name
	}
	wantContentNames := []string{"a.txt", "b.txt", "c.md"}
	if !equalStrings(contentNames, wantContentNames) {
		t.Fatalf("content search fixture = %v, want %v", contentNames, wantContentNames)
	}

	changedCase := fixture.Files.Changed
	changedPath := filepath.Join(root, "changed.txt")
	if err := os.WriteFile(changedPath, []byte(changedCase.Before), 0o600); err != nil {
		t.Fatal(err)
	}
	changedOpen, err := files.Open(changedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changedPath, []byte(changedCase.After), 0o600); err != nil {
		t.Fatal(err)
	}
	changedRead, err := files.Read(changedOpen.Handle, 0, MaxReadBytes)
	if err != nil || !changedRead.ChangedSinceOpen || changedRead.ContentText != changedCase.After {
		t.Fatalf("changed-file fixture = %#v, %v", changedRead, err)
	}
	_ = files.Close(changedOpen.Handle)

	symlinkCase := fixture.Files.Symlink
	targetPath := filepath.Join(root, symlinkCase.TargetName)
	linkPath := filepath.Join(root, symlinkCase.LinkName)
	if err := os.WriteFile(targetPath, []byte(symlinkCase.TargetContents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Fatal(err)
	}
	wantTarget, err := filepath.EvalSymlinks(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	link, err := Metadata(linkPath)
	if err != nil || link.Kind != symlinkCase.Kind || link.SymlinkTarget != wantTarget {
		t.Fatalf("symlink fixture = %#v, %v; want %q", link, err, wantTarget)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

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

func TestPostOpenWriteErrorPreservesUncertainOutcomeUnlessPermissionDenied(t *testing.T) {
	t.Parallel()
	if got := operationError(syscall.ENOSPC, ErrWriteOutcomeUnknown); !errors.Is(got, ErrWriteOutcomeUnknown) {
		t.Fatalf("post-open storage error = %v, want uncertain outcome", got)
	}
	if got := operationError(syscall.EACCES, ErrWriteOutcomeUnknown); !errors.Is(got, ErrPermissionDenied) {
		t.Fatalf("post-open permission error = %v, want permission denied", got)
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
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-000 files, so this process cannot exercise permission denial")
	}
	root := t.TempDir()
	path := filepath.Join(root, "unreadable.txt")
	if err := os.WriteFile(path, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o600)
	files := New()
	for i := 0; i < maxSearches+1; i++ {
		if _, err := files.Search(root, nil, nil, stringPointer("secret"), 1, nil); !errors.Is(err, ErrPermissionDenied) {
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
	page, err := files.Search(root, stringPointer("a"), nil, nil, 1, nil)
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
			_, callErr := files.Search(root, stringPointer("a"), nil, nil, 1, &page.Continuation)
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

func TestSearchContinuationPreservesEmptyFilterPresence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"alpha.txt", "beta.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("hit"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files := New()
	emptyFilter := ""
	glob := "*.txt"
	page, err := files.Search(root, &emptyFilter, &glob, nil, 1, nil)
	if err != nil || page.Continuation == "" || len(page.Matches) != 1 || page.Matches[0].MatchedLines == nil {
		t.Fatalf("initial search = %#v, %v", page, err)
	}
	if _, err := files.Search(root, nil, &glob, nil, 1, &page.Continuation); !errors.Is(err, ErrSearchContinuationExpired) {
		t.Fatalf("continuation after dropping explicit empty filter = %v", err)
	}
}

func TestSearchEmptyNameFilterDoesNotMatchEveryEntry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "alpha.txt"), []byte("hit"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := New()
	emptyFilter := ""
	glob := "no-match*"
	page, err := files.Search(root, &emptyFilter, &glob, nil, 100, nil)
	if err != nil || len(page.Matches) != 0 {
		t.Fatalf("empty name filter should not match by itself: %#v, %v", page, err)
	}
}

func TestSearchEmptyContentFilterKeepsEmptyLinesAndScanTruncation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "large-item.txt")
	content := bytes.Repeat([]byte("x"), maxSearchFileSize+1)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	files := New()
	nameContains := "large-item"
	emptyContent := ""
	page, err := files.Search(root, &nameContains, nil, &emptyContent, 10, nil)
	if err != nil || len(page.Matches) != 1 {
		t.Fatalf("search page = %#v, %v", page, err)
	}
	match := page.Matches[0]
	if match.MatchedLines == nil || len(match.MatchedLines) != 0 || !match.ContentScanTruncated {
		t.Fatalf("empty content filter match = %#v", match)
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.IncompleteContentPaths) != 1 || page.IncompleteContentPaths[0] != resolvedPath {
		t.Fatalf("incomplete content paths = %#v", page.IncompleteContentPaths)
	}
}

func TestSearchUsesUnicodeCaseFoldingForNamesAndContents(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "straße.txt")
	if err := os.WriteFile(path, []byte("die Straße"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := New()
	nameQuery := "STRASSE"
	page, err := files.Search(root, &nameQuery, nil, nil, 10, nil)
	if err != nil || len(page.Matches) != 1 || page.Matches[0].Entry.Name != "straße.txt" {
		t.Fatalf("Unicode name search = %#v, %v", page, err)
	}
	contentQuery := "STRASSE"
	page, err = files.Search(root, nil, nil, &contentQuery, 10, nil)
	if err != nil || len(page.Matches) != 1 || len(page.Matches[0].MatchedLines) != 1 {
		t.Fatalf("Unicode content search = %#v, %v", page, err)
	}
	if containsFold("İstanbul", "i") {
		t.Fatal("dotted capital I matched plain i")
	}
	if !containsFold("straße", "STRASSE") {
		t.Fatal("sharp S did not match its case-insensitive double-s spelling")
	}
	for _, value := range []string{"a\u00adbc", "a\u200bbc"} {
		if containsFold(value, "abc") {
			t.Fatalf("default ignorable in %q was skipped across a search match", value)
		}
	}
	if containsFold("abc", "a\u00adbc") {
		t.Fatal("soft hyphen in query was ignored across a search match")
	}
	if !containsFold("a\u00adbc", "a\u00adbc") {
		t.Fatal("literal soft hyphen search did not match")
	}
	for _, test := range []struct{ value, needle string }{
		{"straße\u200b", "STRASSE\u200b"},
		{"Σ\u200b", "ς\u200b"},
		{"ﬀ\u200b", "FF\u200b"},
		{"ax\u200bstraße\u200by", "x\u200bSTRASSE\u200by"},
	} {
		if !containsFold(test.value, test.needle) {
			t.Errorf("localized search %q in %q did not match", test.needle, test.value)
		}
	}
}

func TestSearchAcceptsValidUTF8ControlsAndFoundationNewlines(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "lines.txt")
	if err := os.WriteFile(path, []byte("first\rsecond\r\nthird\u2028needle\x00\vthird2\fneedle"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := New()
	query := "needle"
	page, err := files.Search(root, nil, nil, &query, 10, nil)
	if err != nil || len(page.Matches) != 1 {
		t.Fatalf("search page = %#v, %v", page, err)
	}
	if got := page.Matches[0].MatchedLines; len(got) != 2 || got[0] != 5 || got[1] != 7 {
		t.Fatalf("matched lines = %#v, want [5 7]", got)
	}
}

func TestSearchNameGlobSupportsPOSIXNegatedCharacterClasses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		pattern    string
		name       string
		want       bool
		incomplete bool
	}{
		{"[!a]*", "banana", true, false},
		{"[!a]*", "apple", false, false},
		{"[a-\\z]", "m", true, false},
		{"\\[!a]*", "banana", false, false},
		{"[[:digit:]]", "7", true, false},
		{"[[:alpha:]]", "é", false, false},
		{"[[:digit:]]", "١", false, false},
		{"[[:space:]]", " ", false, false},
		{"[[:ascii:]]", "a", false, false},
		{"[[=a=]]", "a", true, false},
		{"[[.a.]]", "a", true, false},
		{"[![:digit:]]", "x", true, false},
		{"[![:digit:]]", "7", false, false},
		{"?", "é", false, false},
		{"??", "é", true, false},
		{"é", "é", true, false},
		{"[!a]", "é", false, false},
		{"[À-ÿ]", "é", false, false},
		{strings.Repeat("*", maxPOSIXGlobBytes+1) + "[[:digit:]]", "7", false, true},
	} {
		got, err := matchesFileGlob(test.pattern, test.name)
		if test.incomplete {
			if !errors.Is(err, ErrSearchIncomplete) {
				t.Errorf("matchesFileGlob(%q) error = %v, want %v", test.pattern, err, ErrSearchIncomplete)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("matchesFileGlob(%q, %q) = %t, want %t", test.pattern, test.name, got, test.want)
		}
	}
}

func TestSearchReportsGlobWorkLimit(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		pattern string
	}{
		{"oversized", strings.Repeat("*", maxPOSIXGlobBytes+1) + "[[:digit:]]"},
		{"state budget", strings.Repeat("*", 400) + "[[:digit:]]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			name := "7"
			if test.name == "state budget" {
				name = strings.Repeat("x", 255)
			}
			if err := os.WriteFile(filepath.Join(root, name), []byte("value"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := New().Search(root, nil, &test.pattern, nil, 10, nil)
			if !errors.Is(err, ErrSearchIncomplete) {
				t.Fatalf("search error = %v, want %v", err, ErrSearchIncomplete)
			}
		})
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
	page, err := files.Search(root, nil, nil, stringPointer("hit"), 1, nil)
	resolvedFirst, _ := filepath.EvalSymlinks(first)
	if err != nil || len(page.Matches) != 1 || page.Matches[0].Entry.Path != resolvedFirst || page.Continuation == "" {
		t.Fatalf("first search page: %#v, %v", page, err)
	}
	continuation := page.Continuation
	page, err = files.Search(root, nil, nil, stringPointer("hit"), 1, &continuation)
	resolvedLarge, _ := filepath.EvalSymlinks(large)
	if err != nil || len(page.Matches) != 1 || page.Matches[0].Entry.Path != resolvedLarge || !page.Matches[0].ContentScanTruncated || len(page.IncompleteContentPaths) != 1 {
		t.Fatalf("second search page: %#v, %v", page, err)
	}
	if _, err = files.Search(root, stringPointer("different"), nil, stringPointer("hit"), 1, &continuation); !errors.Is(err, ErrSearchContinuationExpired) {
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

func TestAppendRejectsFIFOWithoutReader(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	completed := make(chan error, 1)
	go func() {
		_, err := Write(fifo, []byte("value"), WriteAppend, nil)
		completed <- err
	}()
	select {
	case err := <-completed:
		if !errors.Is(err, ErrNotRegularFile) {
			t.Fatalf("FIFO append error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO append blocked before rejecting the special file")
	}
}

func stringPointer(value string) *string { return &value }
