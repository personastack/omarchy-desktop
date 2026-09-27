package desktopfiles

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	MaxReadBytes = 256 * 1024
	MaxPageSize  = 500
	MaxOpenFiles = 32
	MaxPatchSize = 4 * 1024 * 1024
	MaxPathBytes = 4096
)

var (
	ErrInvalidPath         = errors.New("invalid file path")
	ErrNotRegularFile      = errors.New("path is not a regular file")
	ErrNotDirectory        = errors.New("path is not a directory")
	ErrMissingHandle       = errors.New("file handle is unavailable")
	ErrInvalidRange        = errors.New("invalid file range")
	ErrContentTooLarge     = errors.New("file content exceeds limit")
	ErrTooManyOpenFiles    = errors.New("too many open file handles")
	ErrDestinationExists   = errors.New("destination already exists")
	ErrPatchMismatch       = errors.New("patch did not match exactly once")
	ErrWriteOutcomeUnknown = errors.New("file write outcome is unknown")
	ErrPermissionDenied    = errors.New("file permission denied")
	ErrMetadataTooLarge    = errors.New("file metadata exceeds limit")
	ErrMetadataUnsupported = errors.New("file metadata cannot be preserved safely")
	ErrSessionEnded        = errors.New("file session ended")
)

type Entry struct {
	Path          string    `json:"path"`
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	Size          int64     `json:"size"`
	ModifiedAt    time.Time `json:"modified_at"`
	SymlinkTarget string    `json:"symlink_target,omitempty"`
}

type Read struct {
	Path             string `json:"path"`
	ByteOffset       int64  `json:"byte_offset"`
	ContentBase64    string `json:"content_base64"`
	ContentText      string `json:"content_text,omitempty"`
	Encoding         string `json:"encoding"`
	ByteLength       int    `json:"byte_length"`
	NextOffset       int64  `json:"next_offset"`
	EndOfFile        bool   `json:"end_of_file"`
	ChangedSinceOpen bool   `json:"changed_since_open"`
	LineStart        int    `json:"line_start,omitempty"`
	NextLine         int    `json:"next_line,omitempty"`
	LineCount        *int   `json:"line_count,omitempty"`
	Truncated        bool   `json:"truncated"`
}

type Opened struct {
	Handle     string    `json:"handle"`
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	Revision   string    `json:"revision"`
	Read
}

type WriteMode string

const (
	WriteCreate  WriteMode = "create"
	WriteReplace WriteMode = "replace"
	WriteAppend  WriteMode = "append"
)

type OpenFiles struct {
	mu         sync.Mutex
	files      map[string]*openFile
	searches   map[string]*searchState
	generation uint64
}

type openFile struct {
	mu      sync.Mutex
	file    *os.File
	path    string
	size    int64
	modTime time.Time
	text    bool
}

func New() *OpenFiles {
	return &OpenFiles{files: make(map[string]*openFile), searches: make(map[string]*searchState)}
}

func Metadata(path string) (Entry, error) {
	clean, err := validPath(path)
	if err != nil {
		return Entry{}, err
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return Entry{}, operationError(err, ErrInvalidPath)
	}
	return entry(clean, info), nil
}

func List(path string, offset, limit int) ([]Entry, *int, error) {
	clean, err := validPath(path)
	if err != nil {
		return nil, nil, err
	}
	if offset < 0 || offset > 100_000 || limit < 1 || limit > MaxPageSize {
		return nil, nil, ErrInvalidRange
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return nil, nil, operationError(err, ErrNotDirectory)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return nil, nil, ErrNotDirectory
	}
	directory, err := os.Open(resolved)
	if err != nil {
		return nil, nil, operationError(err, ErrNotDirectory)
	}
	defer directory.Close()
	needed := offset + limit + 1
	if needed > 100_001 {
		needed = 100_001
	}
	items, err := directory.ReadDir(needed)
	if err != nil && err != io.EOF {
		return nil, nil, operationError(err, ErrNotDirectory)
	}
	if len(items) > 100_000 {
		return nil, nil, ErrContentTooLarge
	}
	if offset >= len(items) {
		return []Entry{}, nil, nil
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	entries := make([]Entry, 0, end-offset)
	for _, item := range items[offset:end] {
		full := filepath.Join(resolved, item.Name())
		childInfo, statErr := os.Lstat(full)
		if statErr != nil {
			return nil, nil, operationError(statErr, ErrInvalidPath)
		}
		entries = append(entries, entry(full, childInfo))
	}
	if end < len(items) {
		next := end
		return entries, &next, nil
	}
	return entries, nil, nil
}

func (files *OpenFiles) Open(path string) (Opened, error) {
	clean, err := validPath(path)
	if err != nil {
		return Opened{}, err
	}
	files.mu.Lock()
	generation := files.generation
	if len(files.files) >= MaxOpenFiles {
		files.mu.Unlock()
		return Opened{}, ErrTooManyOpenFiles
	}
	files.mu.Unlock()
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return Opened{}, operationError(err, ErrInvalidPath)
	}
	fd, err := syscall.Open(resolved, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Opened{}, operationError(err, ErrNotRegularFile)
	}
	file := os.NewFile(uintptr(fd), resolved)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return Opened{}, ErrNotRegularFile
	}
	sample := make([]byte, MaxReadBytes+3)
	n, readErr := file.ReadAt(sample, 0)
	if readErr != nil && readErr != io.EOF {
		_ = file.Close()
		return Opened{}, operationError(readErr, ErrInvalidPath)
	}
	textFile := readableText(sample[:n])
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		_ = file.Close()
		return Opened{}, err
	}
	idBytes[6] = (idBytes[6] & 0x0f) | 0x40
	idBytes[8] = (idBytes[8] & 0x3f) | 0x80
	encodedID := hex.EncodeToString(idBytes[:])
	id := fmt.Sprintf("%s-%s-%s-%s-%s", encodedID[:8], encodedID[8:12], encodedID[12:16], encodedID[16:20], encodedID[20:])
	opened := &openFile{file: file, path: resolved, size: info.Size(), modTime: info.ModTime(), text: textFile}
	files.mu.Lock()
	if generation != files.generation {
		files.mu.Unlock()
		_ = file.Close()
		return Opened{}, ErrSessionEnded
	}
	if len(files.files) >= MaxOpenFiles {
		files.mu.Unlock()
		_ = file.Close()
		return Opened{}, ErrTooManyOpenFiles
	}
	files.files[id] = opened
	files.mu.Unlock()
	first, err := files.Read(id, 0, MaxReadBytes)
	if err != nil {
		_ = files.Close(id)
		return Opened{}, err
	}
	revision := fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
	return Opened{Handle: id, Path: resolved, Size: info.Size(), ModifiedAt: info.ModTime(), Revision: revision, Read: first}, nil
}

func (files *OpenFiles) Read(id string, offset int64, length int) (Read, error) {
	opened, err := files.get(id)
	if err != nil {
		return Read{}, err
	}
	if offset < 0 || length < 0 || length > MaxReadBytes || offset > int64(^uint64(0)>>1)-int64(length) {
		return Read{}, ErrInvalidRange
	}
	opened.mu.Lock()
	defer opened.mu.Unlock()
	buf := make([]byte, length)
	n, readErr := opened.file.ReadAt(buf, offset)
	if readErr != nil && readErr != io.EOF {
		return Read{}, operationError(readErr, ErrInvalidPath)
	}
	data := buf[:n]
	if opened.text {
		data = data[:utf8AlignedPrefix(data)]
	}
	info, statErr := opened.file.Stat()
	changed := statErr != nil || info.Size() != opened.size || !info.ModTime().Equal(opened.modTime)
	next := offset + int64(len(data))
	currentSize := opened.size
	if statErr == nil {
		currentSize = info.Size()
	}
	eof := len(data) == 0 || next >= currentSize
	var lineCount *int
	encoding, contentText := "base64", ""
	if readableText(data) {
		encoding = "utf-8"
		contentText = string(data)
		count := countLines(contentText)
		lineCount = &count
	}
	nextLine := 0
	if lineCount != nil && offset == 0 {
		nextLine = 1 + *lineCount
	}
	return Read{Path: opened.path, ByteOffset: offset, ContentBase64: base64.StdEncoding.EncodeToString(data), ContentText: contentText,
		Encoding: encoding, ByteLength: len(data), NextOffset: next, EndOfFile: eof,
		ChangedSinceOpen: changed, LineStart: boolInt(offset == 0, 1), NextLine: nextLine, LineCount: lineCount}, nil
}

func (files *OpenFiles) ReadLines(id string, startLine, lineCount, length int) (Read, error) {
	if startLine < 1 || lineCount < 1 || lineCount > 10_000 || length < 1 || length > MaxReadBytes {
		return Read{}, ErrInvalidRange
	}
	opened, err := files.get(id)
	if err != nil {
		return Read{}, err
	}
	opened.mu.Lock()
	defer opened.mu.Unlock()
	offset := int64(0)
	line := 1
	buffer := make([]byte, 64*1024)
	for line < startLine {
		n, readErr := opened.file.ReadAt(buffer, offset)
		if readErr != nil && readErr != io.EOF {
			return Read{}, operationError(readErr, ErrInvalidPath)
		}
		if n == 0 {
			break
		}
		found := false
		for i, b := range buffer[:n] {
			if b == '\n' {
				line++
				if line == startLine {
					offset += int64(i + 1)
					found = true
					break
				}
			}
		}
		if !found {
			offset += int64(n)
		}
	}
	data := make([]byte, length)
	n, readErr := opened.file.ReadAt(data, offset)
	if readErr != nil && readErr != io.EOF {
		return Read{}, operationError(readErr, ErrInvalidPath)
	}
	data = data[:n]
	if opened.text {
		data = data[:utf8AlignedPrefix(data)]
	}
	seen, cut := 0, len(data)
	for i, b := range data {
		if b == '\n' {
			seen++
			if seen == lineCount {
				cut = i + 1
				break
			}
		}
	}
	data = data[:cut]
	info, statErr := opened.file.Stat()
	changed := statErr != nil || info.Size() != opened.size || !info.ModTime().Equal(opened.modTime)
	text := string(data)
	count := countLines(text)
	currentSize := opened.size
	if statErr == nil {
		currentSize = info.Size()
	}
	next := offset + int64(len(data))
	truncated := seen < lineCount && next < currentSize
	completedLines := strings.Count(text, "\n")
	if next >= currentSize && len(data) > 0 && data[len(data)-1] != '\n' {
		completedLines++
	}
	encoding, contentText := "base64", ""
	var reportedLines *int
	if opened.text && readableText(data) {
		encoding, contentText, reportedLines = "utf-8", text, &count
	}
	return Read{Path: opened.path, ByteOffset: offset, ContentBase64: base64.StdEncoding.EncodeToString(data), ContentText: contentText,
		Encoding: encoding, ByteLength: len(data), NextOffset: next, EndOfFile: next >= currentSize,
		ChangedSinceOpen: changed, LineStart: startLine, NextLine: boolInt(reportedLines != nil, startLine+completedLines), LineCount: reportedLines, Truncated: truncated}, nil
}

func (files *OpenFiles) Close(id string) error {
	files.mu.Lock()
	opened := files.files[id]
	delete(files.files, id)
	files.mu.Unlock()
	if opened == nil {
		return ErrMissingHandle
	}
	opened.mu.Lock()
	defer opened.mu.Unlock()
	return opened.file.Close()
}

func (files *OpenFiles) CloseAll() {
	files.mu.Lock()
	opened := files.files
	files.files = make(map[string]*openFile)
	files.searches = make(map[string]*searchState)
	files.generation++
	files.mu.Unlock()
	for _, item := range opened {
		item.mu.Lock()
		_ = item.file.Close()
		item.mu.Unlock()
	}
}

func Write(path string, content []byte, mode WriteMode, offset *int64) (Entry, error) {
	clean, err := validPath(path)
	if err != nil {
		return Entry{}, err
	}
	if len(content) > MaxReadBytes {
		return Entry{}, ErrContentTooLarge
	}
	if offset != nil {
		if mode != WriteReplace || *offset < 0 {
			return Entry{}, ErrInvalidRange
		}
		fd, openErr := syscall.Open(clean, syscall.O_WRONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if openErr != nil {
			return Entry{}, operationError(openErr, ErrNotRegularFile)
		}
		file := os.NewFile(uintptr(fd), clean)
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			_ = file.Close()
			return Entry{}, ErrNotRegularFile
		}
		if _, err = file.WriteAt(content, *offset); err != nil {
			_ = file.Close()
			return Entry{}, operationError(err, ErrWriteOutcomeUnknown)
		}
		_ = file.Close()
	} else {
		switch mode {
		case WriteCreate:
			file, createErr := os.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
			if createErr != nil {
				return Entry{}, operationError(createErr, ErrInvalidPath)
			}
			if _, err = file.Write(content); err == nil {
				err = file.Sync()
			}
			closeErr := file.Close()
			if err != nil {
				return Entry{}, operationError(err, ErrWriteOutcomeUnknown)
			}
			if closeErr != nil {
				return Entry{}, operationError(closeErr, ErrWriteOutcomeUnknown)
			}
		case WriteAppend:
			file, appendErr := os.OpenFile(clean, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
			if appendErr != nil {
				return Entry{}, operationError(appendErr, ErrNotRegularFile)
			}
			info, statErr := file.Stat()
			if statErr != nil || !info.Mode().IsRegular() {
				_ = file.Close()
				return Entry{}, ErrNotRegularFile
			}
			_, err = file.Write(content)
			closeErr := file.Close()
			if err != nil {
				return Entry{}, operationError(err, ErrWriteOutcomeUnknown)
			}
			if closeErr != nil {
				return Entry{}, operationError(closeErr, ErrWriteOutcomeUnknown)
			}
		case WriteReplace:
			info, statErr := os.Lstat(clean)
			if statErr != nil || !info.Mode().IsRegular() {
				return Entry{}, ErrNotRegularFile
			}
			err = replaceFile(clean, content, info.Mode(), info)
			if err != nil {
				return Entry{}, operationError(err, ErrInvalidPath)
			}
		default:
			return Entry{}, ErrInvalidRange
		}
	}
	return Metadata(clean)
}

func Patch(path, expected, replacement string) (Entry, error) {
	clean, err := validPath(path)
	if err != nil {
		return Entry{}, err
	}
	if expected == "" {
		return Entry{}, ErrPatchMismatch
	}
	fd, err := syscall.Open(clean, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Entry{}, operationError(err, ErrNotRegularFile)
	}
	file := os.NewFile(uintptr(fd), clean)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Entry{}, operationError(err, ErrPatchMismatch)
	}
	if !info.Mode().IsRegular() || info.Size() > MaxPatchSize {
		return Entry{}, ErrPatchMismatch
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxPatchSize+1))
	if err != nil {
		return Entry{}, operationError(err, ErrInvalidPath)
	}
	if len(data) > MaxPatchSize || !utf8.Valid(data) {
		return Entry{}, ErrPatchMismatch
	}
	current := string(data)
	if strings.Count(current, expected) != 1 {
		return Entry{}, ErrPatchMismatch
	}
	if len(replacement) > MaxPatchSize || len(data)-len(expected) > MaxPatchSize-len(replacement) {
		return Entry{}, ErrContentTooLarge
	}
	updated := strings.Replace(current, expected, replacement, 1)
	if err = replaceFile(clean, []byte(updated), info.Mode(), info); err != nil {
		return Entry{}, operationError(err, ErrInvalidPath)
	}
	return Metadata(clean)
}

func MakeDirectory(path string) error {
	clean, err := validPath(path)
	if err != nil {
		return err
	}
	if err = os.Mkdir(clean, 0o755); err != nil {
		return operationError(err, ErrInvalidPath)
	}
	return nil
}

func Move(source, destination string) error {
	from, err := validPath(source)
	if err != nil {
		return err
	}
	to, err := validPath(destination)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(to); err == nil {
		return ErrDestinationExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return operationError(err, ErrInvalidPath)
	}
	if err = renameNoReplace(from, to); err != nil {
		return operationError(err, ErrInvalidPath)
	}
	return nil
}

func Remove(path string) error {
	clean, err := validPath(path)
	if err != nil || clean == "/" {
		return ErrInvalidPath
	}
	if _, err = os.Lstat(clean); err != nil {
		return operationError(err, ErrInvalidPath)
	}
	if err = os.Remove(clean); err != nil {
		return operationError(err, ErrInvalidPath)
	}
	return nil
}

func (files *OpenFiles) get(id string) (*openFile, error) {
	files.mu.Lock()
	defer files.mu.Unlock()
	opened := files.files[id]
	if opened == nil {
		return nil, ErrMissingHandle
	}
	return opened, nil
}

func validPath(path string) (string, error) {
	if path == "" || len(path) > MaxPathBytes || !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return "", ErrInvalidPath
	}
	return filepath.Clean(path), nil
}

func entry(path string, info os.FileInfo) Entry {
	kind := "other"
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		kind = "symlink"
	case info.IsDir():
		kind = "directory"
	case info.Mode().IsRegular():
		kind = "file"
	}
	item := Entry{Path: path, Name: filepath.Base(path), Kind: kind, Size: info.Size(), ModifiedAt: info.ModTime()}
	if kind == "symlink" {
		if target, err := os.Readlink(path); err == nil {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			item.SymlinkTarget = resolveExistingPrefix(target)
		}
	}
	return item
}

func resolveExistingPrefix(path string) string {
	clean := filepath.Clean(path)
	prefix := clean
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(prefix)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return clean
		}
		suffix = append(suffix, filepath.Base(prefix))
		prefix = parent
	}
}

func replaceFile(path string, content []byte, mode os.FileMode, expected os.FileInfo) error {
	sourceFD, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return operationError(err, ErrNotRegularFile)
	}
	source := os.NewFile(uintptr(sourceFD), path)
	defer source.Close()
	original, err := source.Stat()
	if err != nil || !original.Mode().IsRegular() {
		return ErrNotRegularFile
	}
	if expected != nil && (!os.SameFile(expected, original) || expected.Size() != original.Size() || !expected.ModTime().Equal(original.ModTime()) || !platformMetadataUnchanged(expected, original)) {
		return ErrPatchMismatch
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(path), ".personastack-replace-"+hex.EncodeToString(nonce[:]))
	file, err := os.OpenFile(temp, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	if _, err = file.Write(content); err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = preservePlatformMetadata(sourceFD, int(file.Fd()), original, mode)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() {
		return ErrNotRegularFile
	}
	if expected != nil && (!os.SameFile(expected, current) || expected.Size() != current.Size() || !expected.ModTime().Equal(current.ModTime()) || !platformMetadataUnchanged(expected, current)) {
		return ErrPatchMismatch
	}
	return os.Rename(temp, path)
}

func operationError(err, fallback error) error {
	if errors.Is(err, ErrPatchMismatch) || errors.Is(err, ErrMetadataTooLarge) || errors.Is(err, ErrMetadataUnsupported) || errors.Is(err, ErrDestinationExists) {
		return err
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		return ErrPermissionDenied
	}
	if errors.Is(err, os.ErrExist) {
		return ErrDestinationExists
	}
	return fallback
}

func readableText(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for _, r := range string(data) {
		if r == 0 || r < 0x20 && r != '\n' && r != '\r' && r != '\t' || r == 0x7f {
			return false
		}
	}
	return true
}

func utf8AlignedPrefix(data []byte) int {
	if utf8.Valid(data) {
		return len(data)
	}
	for suffix := 1; suffix <= 3 && suffix <= len(data); suffix++ {
		prefix := data[:len(data)-suffix]
		if utf8.Valid(prefix) && !utf8.FullRune(data[len(data)-suffix:]) {
			return len(prefix)
		}
	}
	return len(data)
}

func countLines(text string) int {
	if text == "" {
		return 0
	}
	count := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		count++
	}
	return count
}

func boolInt(ok bool, value int) int {
	if ok {
		return value
	}
	return 0
}

func unixFileMode(mode os.FileMode) uint32 {
	result := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		result |= 0o4000
	}
	if mode&os.ModeSetgid != 0 {
		result |= 0o2000
	}
	if mode&os.ModeSticky != 0 {
		result |= 0o1000
	}
	return result
}
