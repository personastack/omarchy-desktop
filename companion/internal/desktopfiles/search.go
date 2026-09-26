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
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/language"
	textsearch "golang.org/x/text/search"
)

const (
	maxSearches        = 32
	maxSearchEntries   = 100_000
	maxSearchFileSize  = 4 * 1024 * 1024
	maxSearchLines     = 100
	maxPOSIXGlobBytes  = 1024
	maxGlobMatchStates = 100_000
)

var localizedSearchMatcher = textsearch.New(language.Und, textsearch.IgnoreCase)

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
	nameContains    *string
	nameGlob        *string
	contentContains *string
	pendingDirs     []string
	currentEntries  []string
	currentIndex    int
	incompletePaths map[string]struct{}
	lastAccess      time.Time
	completed       bool
}

func (files *OpenFiles) Search(root string, nameContains, nameGlob, contentContains *string, limit int, continuation *string) (SearchPage, error) {
	clean, err := validPath(root)
	if err != nil {
		return SearchPage{}, ErrNotDirectory
	}
	if limit < 1 || limit > MaxPageSize || !hasSearchTerm(nameContains) && !hasSearchTerm(nameGlob) && !hasSearchTerm(contentContains) {
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
	if continuation != nil {
		state = files.searches[*continuation]
		if state == nil {
			files.mu.Unlock()
			return SearchPage{}, ErrSearchContinuationExpired
		}
		state.mu.Lock()
		if state.completed || state.root != root || !sameOptionalString(state.nameContains, nameContains) ||
			!sameOptionalString(state.nameGlob, nameGlob) || !sameOptionalString(state.contentContains, contentContains) {
			state.mu.Unlock()
			files.mu.Unlock()
			return SearchPage{}, ErrSearchContinuationExpired
		}
		state.lastAccess = now
		delete(files.searches, *continuation)
		state.mu.Unlock()
		id = *continuation
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
		nameMatch := nameContains != nil && containsFold(item.Name, *nameContains)
		globMatch := false
		if nameGlob != nil {
			globMatch, err = matchesFileGlob(*nameGlob, item.Name)
			if err != nil {
				state.mu.Unlock()
				files.mu.Lock()
				delete(files.searches, id)
				files.mu.Unlock()
				return SearchPage{}, err
			}
		}
		lines, truncated := make([]int, 0), false
		if contentContains != nil && item.Kind == "file" {
			lines, truncated, err = matchingLines(child, *contentContains)
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

func hasSearchTerm(value *string) bool { return value != nil && *value != "" }

func sameOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
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
	if !utf8.Valid(data) {
		return []int{}, truncated, nil
	}
	if needle == "" {
		return []int{}, truncated, nil
	}
	lines := splitSearchLines(string(data))
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
	if needle == "" {
		return false
	}
	valueParts, needleParts := splitSearchTokens(value), splitSearchTokens(needle)
	if len(needleParts) == 1 && !needleParts[0].boundary {
		for _, part := range valueParts {
			if !part.boundary && containsFoldSegment(part.value, needleParts[0].value) {
				return true
			}
		}
		return false
	}
	firstText, lastText := -1, -1
	for index, part := range needleParts {
		if !part.boundary {
			if firstText < 0 {
				firstText = index
			}
			lastText = index
		}
	}
	for start := 0; start+len(needleParts) <= len(valueParts); start++ {
		matched := true
		for offset, needlePart := range needleParts {
			valuePart := valueParts[start+offset]
			if needlePart.boundary != valuePart.boundary {
				matched = false
				break
			}
			if needlePart.boundary {
				if needlePart.value != valuePart.value {
					matched = false
					break
				}
				continue
			}
			if offset == firstText || offset == lastText {
				if !containsFoldSegment(valuePart.value, needlePart.value) {
					matched = false
					break
				}
			} else if !equalFoldSegment(valuePart.value, needlePart.value) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

type searchToken struct {
	value    string
	boundary bool
}

func splitSearchTokens(value string) []searchToken {
	tokens := make([]searchToken, 0, 1)
	start := 0
	for index, char := range value {
		if char != '\x00' && !isDefaultIgnorable(char) {
			continue
		}
		if start < index {
			tokens = append(tokens, searchToken{value: value[start:index]})
		}
		tokens = append(tokens, searchToken{value: string(char), boundary: true})
		start = index + len(string(char))
	}
	if start < len(value) {
		tokens = append(tokens, searchToken{value: value[start:]})
	}
	return tokens
}

func equalFoldSegment(left, right string) bool {
	left, right = normalizeSharpS(left, right)
	leftStart, leftEnd := localizedSearchMatcher.IndexString(left, right)
	rightStart, rightEnd := localizedSearchMatcher.IndexString(right, left)
	return leftStart == 0 && leftEnd == len(left) && rightStart == 0 && rightEnd == len(right)
}

func isDefaultIgnorable(char rune) bool {
	return unicode.Is(unicode.Cf, char) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, char) ||
		unicode.Is(unicode.Variation_Selector, char)
}

func containsFoldSegment(value, needle string) bool {
	value, needle = normalizeSharpS(value, needle)
	start, _ := localizedSearchMatcher.IndexString(value, needle)
	return start >= 0
}

func normalizeSharpS(value, needle string) (string, string) {
	if containsSharpS(value) && strings.Contains(strings.ToLower(needle), "ss") ||
		containsSharpS(needle) && strings.Contains(strings.ToLower(value), "ss") {
		value = replaceSharpS(value)
		needle = replaceSharpS(needle)
	}
	return value, needle
}

func containsSharpS(value string) bool { return strings.ContainsAny(value, "ßẞ") }

func replaceSharpS(value string) string {
	return strings.NewReplacer("ß", "ss", "ẞ", "ss").Replace(value)
}

func splitSearchLines(value string) []string {
	lines := make([]string, 0, strings.Count(value, "\n")+1)
	start := 0
	for index, char := range value {
		if index < start {
			continue
		}
		if char != '\n' && char != '\v' && char != '\f' && char != '\r' &&
			char != '\u0085' && char != '\u2028' && char != '\u2029' {
			continue
		}
		lines = append(lines, value[start:index])
		start = index + len(string(char))
	}
	return append(lines, value[start:])
}

func matchesFileGlob(pattern, name string) (bool, error) {
	if pattern == "" {
		return false, nil
	}
	if len(pattern) > maxPOSIXGlobBytes {
		return false, ErrSearchIncomplete
	}
	return matchesPOSIXGlob(pattern, name)
}

type globToken struct {
	kind    byte
	value   byte
	negated bool
	ranges  [][2]byte
	values  []byte
	classes []string
}

func matchesPOSIXGlob(pattern, name string) (bool, error) {
	tokens, ok := parsePOSIXGlob(pattern)
	if !ok {
		return false, nil
	}
	nameBytes := []byte(name)
	if len(tokens)+1 > maxGlobMatchStates/(len(nameBytes)+1) {
		return false, ErrSearchIncomplete
	}
	type position struct{ token, name int }
	memo := make(map[position]bool)
	visited := make(map[position]bool)
	var match func(int, int) bool
	match = func(tokenIndex, nameIndex int) bool {
		position := position{token: tokenIndex, name: nameIndex}
		if visited[position] {
			return memo[position]
		}
		visited[position] = true
		result := false
		if tokenIndex == len(tokens) {
			result = nameIndex == len(nameBytes)
		} else {
			token := tokens[tokenIndex]
			switch token.kind {
			case '*':
				result = match(tokenIndex+1, nameIndex) || nameIndex < len(nameBytes) && nameBytes[nameIndex] != '/' && match(tokenIndex, nameIndex+1)
			case '?':
				result = nameIndex < len(nameBytes) && nameBytes[nameIndex] != '/' && match(tokenIndex+1, nameIndex+1)
			case 'l':
				result = nameIndex < len(nameBytes) && nameBytes[nameIndex] == token.value && match(tokenIndex+1, nameIndex+1)
			case 'c':
				result = nameIndex < len(nameBytes) && nameBytes[nameIndex] != '/' && globClassMatches(token, nameBytes[nameIndex]) && match(tokenIndex+1, nameIndex+1)
			}
		}
		memo[position] = result
		return result
	}
	return match(0, 0), nil
}

func parsePOSIXGlob(pattern string) ([]globToken, bool) {
	patternBytes := []byte(pattern)
	tokens := make([]globToken, 0, len(patternBytes))
	for index := 0; index < len(patternBytes); index++ {
		switch patternBytes[index] {
		case '\\':
			index++
			if index == len(patternBytes) {
				return nil, false
			}
			tokens = append(tokens, globToken{kind: 'l', value: patternBytes[index]})
		case '*', '?':
			tokens = append(tokens, globToken{kind: patternBytes[index]})
		case '[':
			token, next, ok := parsePOSIXGlobClass(patternBytes, index+1)
			if !ok {
				return nil, false
			}
			tokens = append(tokens, token)
			index = next
		default:
			tokens = append(tokens, globToken{kind: 'l', value: patternBytes[index]})
		}
	}
	return tokens, true
}

func parsePOSIXGlobClass(pattern []byte, index int) (globToken, int, bool) {
	token := globToken{kind: 'c'}
	if index < len(pattern) && (pattern[index] == '!' || pattern[index] == '^') {
		token.negated = true
		index++
	}
	items := 0
	for index < len(pattern) {
		if pattern[index] == ']' && items > 0 {
			return token, index, true
		}
		if index+1 < len(pattern) && pattern[index] == '[' && pattern[index+1] == ':' {
			end := index + 2
			for end+1 < len(pattern) && !(pattern[end] == ':' && pattern[end+1] == ']') {
				end++
			}
			if end+1 >= len(pattern) {
				return globToken{}, 0, false
			}
			className := string(pattern[index+2 : end])
			if !validPOSIXClass(className) {
				return globToken{}, 0, false
			}
			token.classes = append(token.classes, className)
			items++
			index = end + 2
			continue
		}
		if index+1 < len(pattern) && pattern[index] == '[' && (pattern[index+1] == '.' || pattern[index+1] == '=') {
			marker := pattern[index+1]
			end := index + 2
			for end+1 < len(pattern) && !(pattern[end] == marker && pattern[end+1] == ']') {
				end++
			}
			if end+1 >= len(pattern) || end == index+2 {
				return globToken{}, 0, false
			}
			element := pattern[index+2 : end]
			if len(element) != 1 {
				return globToken{}, 0, false
			}
			token.values = append(token.values, element[0])
			items++
			index = end + 2
			continue
		}
		first := pattern[index]
		if first == '\\' && index+1 < len(pattern) {
			index++
			first = pattern[index]
		}
		if index+2 < len(pattern) && pattern[index+1] == '-' {
			end := index + 2
			if pattern[end] == '\\' {
				end++
				if end >= len(pattern) {
					return globToken{}, 0, false
				}
			}
			if pattern[end] != ']' {
				token.ranges = append(token.ranges, [2]byte{first, pattern[end]})
				index = end + 1
				items++
				continue
			}
			token.ranges = append(token.ranges, [2]byte{first, first})
			index++
		} else {
			token.ranges = append(token.ranges, [2]byte{first, first})
			index++
		}
		items++
	}
	return globToken{}, 0, false
}

func validPOSIXClass(name string) bool {
	switch name {
	case "alnum", "alpha", "blank", "cntrl", "digit", "graph", "lower", "print", "punct", "space", "upper", "xdigit":
		return true
	default:
		return false
	}
}

func globClassMatches(token globToken, value byte) bool {
	matched := false
	for _, classRange := range token.ranges {
		matched = matched || classRange[0] <= value && value <= classRange[1]
	}
	for _, element := range token.values {
		matched = matched || element == value
	}
	for _, className := range token.classes {
		matched = matched || posixClassMatches(className, value)
	}
	return matched != token.negated
}

func posixClassMatches(name string, character byte) bool {
	value := rune(character)
	switch name {
	case "alnum":
		return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
	case "alpha":
		return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
	case "blank":
		return value == ' ' || value == '\t'
	case "cntrl":
		return value <= 0x1f || value == 0x7f
	case "digit":
		return value >= '0' && value <= '9'
	case "graph":
		return value >= 0x21 && value <= 0x7e
	case "lower":
		return value >= 'a' && value <= 'z'
	case "print":
		return value >= 0x20 && value <= 0x7e
	case "punct":
		return value >= '!' && value <= '/' || value >= ':' && value <= '@' ||
			value >= '[' && value <= '`' || value >= '{' && value <= '~'
	case "space":
		return value == ' ' || value >= '\t' && value <= '\r'
	case "upper":
		return value >= 'A' && value <= 'Z'
	case "xdigit":
		return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
	default:
		return false
	}
}
