package desktopfiles

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxSearches       = 32
	maxSearchEntries  = 100_000
	maxSearchFileSize = 4 * 1024 * 1024
	maxSearchLines    = 100
)

var (
	ErrSearchIncomplete          = errors.New("file search exceeded scan bounds")
	ErrSearchContinuationExpired = errors.New("file search continuation expired")
	ErrTooManySearches           = errors.New("too many active file searches")
)

type SearchMatch struct {
	Entry                Entry `json:"entry"`
	MatchedLines         []int `json:"matched_lines"`
	ContentScanTruncated bool  `json:"content_scan_truncated"`
}

type SearchPage struct {
	Matches                []SearchMatch `json:"matches"`
	Continuation           string        `json:"continuation,omitempty"`
	IncompleteContentPaths []string      `json:"incomplete_content_paths"`
	Complete               bool          `json:"complete"`
}

type searchState struct {
	mu              sync.Mutex
	root            string
	nameContains    string
	nameGlob        string
	contentContains string
	pendingDirs     []string
	currentEntries  []string
	currentIndex    int
	incompletePaths map[string]struct{}
	lastAccess      time.Time
	completed       bool
}

func (files *OpenFiles) Search(root, nameContains, nameGlob, contentContains string, limit int, continuation string) (SearchPage, error) {
	clean, err := validPath(root)
	if err != nil {
		return SearchPage{}, ErrNotDirectory
	}
	if limit < 1 || limit > MaxPageSize || nameContains == "" && nameGlob == "" && contentContains == "" {
		return SearchPage{}, ErrInvalidRange
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return SearchPage{}, operationError(err, ErrNotDirectory)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return SearchPage{}, ErrNotDirectory
	}
	root = filepath.Clean(resolved)
	now := time.Now()
	files.mu.Lock()
	for id, state := range files.searches {
		state.mu.Lock()
		expired := now.Sub(state.lastAccess) >= 120*time.Second
		state.mu.Unlock()
		if expired {
			delete(files.searches, id)
		}
	}
	var id string
	var state *searchState
	if continuation != "" {
		state = files.searches[continuation]
		if state == nil {
			files.mu.Unlock()
			return SearchPage{}, ErrSearchContinuationExpired
		}
		state.mu.Lock()
		if state.completed || state.root != root || state.nameContains != nameContains || state.nameGlob != nameGlob || state.contentContains != contentContains {
			state.mu.Unlock()
			files.mu.Unlock()
			return SearchPage{}, ErrSearchContinuationExpired
		}
		state.lastAccess = now
		delete(files.searches, continuation)
		state.mu.Unlock()
		id = continuation
	} else {
		if len(files.searches) >= maxSearches {
			files.mu.Unlock()
			return SearchPage{}, ErrTooManySearches
		}
		randomID := make([]byte, 16)
		if _, err = rand.Read(randomID); err != nil {
			files.mu.Unlock()
			return SearchPage{}, err
		}
		id = hex.EncodeToString(randomID)
		state = &searchState{root: root, nameContains: nameContains, nameGlob: nameGlob, contentContains: contentContains,
			pendingDirs: []string{root}, incompletePaths: make(map[string]struct{}), lastAccess: now}
		files.searches[id] = state
	}
	generation := files.generation
	files.mu.Unlock()

	state.mu.Lock()
	deadline := time.Now().Add(2 * time.Second)
	matches := make([]SearchMatch, 0, limit)
	for len(matches) < limit && time.Now().Before(deadline) {
		if state.currentIndex >= len(state.currentEntries) {
			if len(state.pendingDirs) == 0 {
				break
			}
			directory := state.pendingDirs[len(state.pendingDirs)-1]
			state.pendingDirs = state.pendingDirs[:len(state.pendingDirs)-1]
			children, readErr := readSearchDirectory(directory)
			if readErr != nil {
				state.mu.Unlock()
				files.mu.Lock()
				delete(files.searches, id)
				files.mu.Unlock()
				if errors.Is(readErr, ErrSearchIncomplete) {
					return SearchPage{}, ErrSearchIncomplete
				}
				return SearchPage{}, operationError(readErr, ErrNotDirectory)
			}
			state.currentEntries = state.currentEntries[:0]
			for _, child := range children {
				state.currentEntries = append(state.currentEntries, filepath.Join(directory, child.Name()))
			}
			sort.Strings(state.currentEntries)
			state.currentIndex = 0
			continue
		}
		child := state.currentEntries[state.currentIndex]
		state.currentIndex++
		childInfo, statErr := os.Lstat(child)
		if statErr != nil {
			continue
		}
		item := entry(child, childInfo)
		if item.Kind == "directory" {
			state.pendingDirs = append(state.pendingDirs, child)
		}
		nameMatch := containsFold(item.Name, nameContains)
		globMatch := matchesFileGlob(nameGlob, item.Name)
		lines, truncated := []int(nil), false
		if contentContains != "" && item.Kind == "file" {
			lines, truncated, err = matchingLines(child, contentContains)
			if err != nil {
				state.mu.Unlock()
				files.mu.Lock()
				delete(files.searches, id)
				files.mu.Unlock()
				return SearchPage{}, err
			}
			if truncated {
				state.incompletePaths[child] = struct{}{}
			}
		}
		if nameMatch || globMatch || len(lines) > 0 {
			matches = append(matches, SearchMatch{Entry: item, MatchedLines: lines, ContentScanTruncated: truncated})
		}
	}
	complete := state.currentIndex >= len(state.currentEntries) && len(state.pendingDirs) == 0
	paths := make([]string, 0, len(state.incompletePaths))
	for path := range state.incompletePaths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	page := SearchPage{Matches: matches, IncompleteContentPaths: paths, Complete: complete && len(paths) == 0}
	if complete {
		state.completed = true
	} else {
		page.Continuation = id
	}
	state.mu.Unlock()
	files.mu.Lock()
	if generation != files.generation {
		files.mu.Unlock()
		return SearchPage{}, ErrSearchContinuationExpired
	}
	if !complete {
		files.searches[id] = state
	}
	files.mu.Unlock()
	return page, nil
}

func readSearchDirectory(path string) ([]os.DirEntry, error) {
	directory, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(maxSearchEntries + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > maxSearchEntries {
		return nil, ErrSearchIncomplete
	}
	return entries, nil
}

func matchingLines(path, needle string) ([]int, bool, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, operationError(err, ErrNotRegularFile)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSearchFileSize+1))
	if err != nil {
		return nil, false, operationError(err, ErrInvalidPath)
	}
	truncated := len(data) > maxSearchFileSize
	data = data[:min(len(data), maxSearchFileSize)]
	if !readableText(data) || needle == "" {
		return nil, truncated, nil
	}
	lines := strings.Split(string(data), "\n")
	matched := make([]int, 0, min(len(lines), maxSearchLines))
	for index, line := range lines {
		if containsFold(line, needle) {
			matched = append(matched, index+1)
			if len(matched) == maxSearchLines {
				break
			}
		}
	}
	return matched, truncated, nil
}

func containsFold(value, needle string) bool {
	return needle != "" && strings.Contains(strings.ToLower(value), strings.ToLower(needle))
}

func matchesFileGlob(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	matched, err := filepath.Match(pattern, name)
	return err == nil && matched
}
