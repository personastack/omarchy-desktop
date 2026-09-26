package cuainstaller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	DriverVersion      = "0.29.1"
	version            = DriverVersion
	archiveSHA256      = "61a0c0f24d6b03e31bb7a73390db875ecf0de2ce53aa435eadb03d70979d79a5"
	archiveURL         = "https://github.com/trycua/cua/releases/download/cua-driver-rs-v0.29.1/cua-driver-rs-0.29.1-linux-x86_64.tar.gz"
	maximumArchive     = 80 << 20
	maximumExpanded    = 180 << 20
	maximumFileBytes   = 60 << 20
	maximumMarker      = 256
	maximumTreeEntries = 64
	installTimeout     = 2 * time.Minute
)

//go:embed CUA-NODE-NOTICE.md MPL-2.0.txt
var thirdPartyNotices embed.FS

var (
	ErrUnavailable      = errors.New("Cua installer unavailable")
	ErrInvalidArchive   = errors.New("Cua release archive is invalid")
	ErrChecksumMismatch = errors.New("Cua release checksum did not match")
	ErrForeignInstall   = errors.New("Cua install path is not owned by PersonaStack")
	assetHashes         = map[string]string{
		"LICENSE":                                   "c0779290c1d4783169aa3dbfb55feb505e563ef8a004bbf55298ceffcfbda8d9",
		"cua-cursor-theme":                          "49cd40354577a6c6a6ff7e1954ed09787d4b2c7b6a445bb835dd0481f4e42181",
		"cua-driver":                                "a9c3262817103cdff6c09e351f6a3410206624a6f40eea5bd14b4abb3ddf9362",
		"cua_driver_abi.h":                          "e952620e41ac81b2d900886c7b0a24ebc6268a4edb8df88cc9971ae34af4ba0d",
		"cua_driver_node_runtime.node":              "bcd60acbb89d042d25ec9f311863b89261f17771382aecf3fe2ec02538937381",
		"libcua_driver_sdk.so":                      "d02455548901590bfe85a34aa27e7cac6c03d782e627e1f55da900194325a864",
		"wayland-helper/README.md":                  "0c38155388bdb5b3a276c4d434fbc5311ed7bb775ae6b04969f50de81c8c2703",
		"wayland-helper/install.sh":                 "e13fc5700d281fed547fbb529a31bc1a0df250e6f9811c9cdddc99d465e219a0",
		"wayland-helper/winrects@cua/extension.js":  "27aac56799574ecd201e810d32772d3695d8b6ace5ab4a68d026648009004eed",
		"wayland-helper/winrects@cua/metadata.json": "3f6624c882bde9d611848201e2f245c813fbc322a57a003d52847afc8e6f4c50",
	}
	directoryEntries = map[string]struct{}{
		"wayland-helper":              {},
		"wayland-helper/winrects@cua": {},
	}
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Installer struct {
	root    string
	client  HTTPDoer
	release release
	notices []noticeFile
	rename  func(string, string) error
}

type noticeFile struct {
	name    string
	content []byte
}

type release struct {
	version       string
	archivePrefix string
	archiveURL    string
	archiveSHA256 string
	assetHashes   map[string]string
	directories   map[string]struct{}
}

var pinnedRelease = release{
	version:       version,
	archivePrefix: "cua-driver-rs-0.29.1-linux-x86_64/",
	archiveURL:    archiveURL,
	archiveSHA256: archiveSHA256,
	assetHashes:   assetHashes,
	directories:   directoryEntries,
}

type managedMarker struct {
	Version       string `json:"version"`
	ArchiveSHA256 string `json:"archive_sha256"`
}

func New() (*Installer, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return nil, ErrUnavailable
	}
	dataRoot := os.Getenv("XDG_DATA_HOME")
	if dataRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, ErrUnavailable
		}
		dataRoot = filepath.Join(home, ".local", "share")
	}
	root := filepath.Join(dataRoot, "personastack", "omarchy-desktop", "cua-driver", version)
	return newInstaller(root, runtime.GOOS, runtime.GOARCH, http.DefaultClient)
}

func newInstaller(root, operatingSystem, architecture string, client HTTPDoer) (*Installer, error) {
	notices, err := loadThirdPartyNotices()
	if err != nil {
		return nil, ErrUnavailable
	}
	return newInstallerForRelease(root, operatingSystem, architecture, client, pinnedRelease, notices)
}

func newInstallerForRelease(root, operatingSystem, architecture string, client HTTPDoer, selected release, notices []noticeFile) (*Installer, error) {
	if operatingSystem != "linux" || architecture != "amd64" || client == nil {
		return nil, ErrUnavailable
	}
	absolute, err := filepath.Abs(root)
	if err != nil || !filepath.IsAbs(root) || absolute != filepath.Clean(root) || selected.version == "" || selected.archiveURL == "" || selected.archiveSHA256 == "" || len(selected.assetHashes) == 0 || len(notices) == 0 {
		return nil, ErrUnavailable
	}
	return &Installer{root: absolute, client: client, release: selected, notices: cloneNotices(notices), rename: renameNoReplace}, nil
}

func loadThirdPartyNotices() ([]noticeFile, error) {
	files := []noticeFile{{name: "THIRD-PARTY-NOTICES/CUA-NODE-NOTICE.md"}, {name: "THIRD-PARTY-NOTICES/MPL-2.0.txt"}}
	for index := range files {
		content, err := thirdPartyNotices.ReadFile(filepath.Base(files[index].name))
		if err != nil {
			return nil, err
		}
		files[index].content = content
	}
	return files, nil
}

func cloneNotices(notices []noticeFile) []noticeFile {
	cloned := make([]noticeFile, len(notices))
	for index, notice := range notices {
		cloned[index] = noticeFile{name: notice.name, content: append([]byte(nil), notice.content...)}
	}
	return cloned
}

func (i *Installer) Install(ctx context.Context) (string, error) {
	if i == nil || ctx == nil {
		return "", ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := verifyTree(ctx, i.root, i.release, i.notices); err == nil {
		return filepath.Join(i.root, "cua-driver"), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	archive, err := i.download(ctx)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	stage, err := i.makeStage(ctx)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err := extractVerified(ctx, archive, stage, i.release); err != nil {
		return "", err
	}
	for _, notice := range i.notices {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		path := filepath.Join(stage, filepath.FromSlash(notice.name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", err
		}
		if err := writePrivateFile(ctx, path, notice.content, 0o600); err != nil {
			return "", err
		}
	}
	marker, err := json.Marshal(managedMarker{Version: i.release.version, ArchiveSHA256: i.release.archiveSHA256})
	if err != nil {
		return "", fmt.Errorf("encode Cua ownership marker: %w", err)
	}
	marker = append(marker, '\n')
	if err := writePrivateFile(ctx, filepath.Join(stage, ".personastack-cua.json"), marker, 0o600); err != nil {
		return "", err
	}
	if err := verifyTree(ctx, stage, i.release, i.notices); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := i.rename(stage, i.root); err != nil {
		if errors.Is(err, os.ErrExist) {
			if verifyErr := verifyTree(ctx, i.root, i.release, i.notices); verifyErr == nil {
				return filepath.Join(i.root, "cua-driver"), nil
			}
		}
		return "", fmt.Errorf("install Cua release: %w", err)
	}
	if err := verifyTree(ctx, i.root, i.release, i.notices); err != nil {
		return "", err
	}
	return filepath.Join(i.root, "cua-driver"), nil
}

func (i *Installer) Verify() error {
	if i == nil {
		return ErrUnavailable
	}
	return verifyTree(context.Background(), i.root, i.release, i.notices)
}

func (i *Installer) download(ctx context.Context) ([]byte, error) {
	downloadContext, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(downloadContext, http.MethodGet, i.release.archiveURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create Cua download request: %w", err)
	}
	response, err := i.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download Cua release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > maximumArchive {
		return nil, ErrUnavailable
	}
	archive, err := io.ReadAll(io.LimitReader(response.Body, maximumArchive+1))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil || len(archive) == 0 || len(archive) > maximumArchive {
		return nil, ErrUnavailable
	}
	digest := sha256.Sum256(archive)
	if hex.EncodeToString(digest[:]) != i.release.archiveSHA256 {
		return nil, ErrChecksumMismatch
	}
	return archive, nil
}

func (i *Installer) makeStage(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	parent := filepath.Dir(i.root)
	if err := ensurePrivateDirectoryPath(parent, true); err != nil {
		return "", fmt.Errorf("create Cua data directory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !currentUserOwnsDirectory(info) {
		return "", ErrForeignInstall
	}
	stage, err := os.MkdirTemp(parent, ".cua-driver-install-")
	if err != nil {
		return "", fmt.Errorf("create Cua install staging directory: %w", err)
	}
	if err := os.Chmod(stage, 0o700); err != nil {
		os.RemoveAll(stage)
		return "", fmt.Errorf("protect Cua install staging directory: %w", err)
	}
	return stage, nil
}

func extractVerified(ctx context.Context, archive []byte, root string, selected release) error {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return ErrInvalidArchive
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	seen := make(map[string]struct{}, len(selected.assetHashes)+len(selected.directories))
	seenArchiveRoot := false
	var expanded int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || header.Size < 0 || header.Size > maximumFileBytes {
			return ErrInvalidArchive
		}
		if selected.archivePrefix != "" && header.Name == selected.archivePrefix {
			if seenArchiveRoot || header.Typeflag != tar.TypeDir || header.Size != 0 {
				return ErrInvalidArchive
			}
			seenArchiveRoot = true
			continue
		}
		if !strings.HasPrefix(header.Name, selected.archivePrefix) {
			return ErrInvalidArchive
		}
		relativeName := strings.TrimPrefix(header.Name, selected.archivePrefix)
		name := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relativeName)))
		directoryName := strings.TrimSuffix(name, "/")
		if name == "." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") || (header.Typeflag != tar.TypeDir && name != relativeName) || (header.Typeflag == tar.TypeDir && relativeName != directoryName && relativeName != directoryName+"/") {
			return ErrInvalidArchive
		}
		if _, exists := seen[name]; exists {
			return ErrInvalidArchive
		}
		seen[name] = struct{}{}
		switch header.Typeflag {
		case tar.TypeDir:
			if _, ok := selected.directories[directoryName]; !ok || header.Size != 0 {
				return ErrInvalidArchive
			}
			if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(directoryName)), 0o700); err != nil {
				return ErrInvalidArchive
			}
		case tar.TypeReg, tar.TypeRegA:
			expected, ok := selected.assetHashes[name]
			if !ok {
				return ErrInvalidArchive
			}
			expanded += header.Size
			if expanded > maximumExpanded {
				return ErrInvalidArchive
			}
			content, err := io.ReadAll(io.LimitReader(interruptibleReader{ctx: ctx, reader: reader}, maximumFileBytes+1))
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			if err != nil || int64(len(content)) != header.Size {
				return ErrInvalidArchive
			}
			digest := sha256.Sum256(content)
			if hex.EncodeToString(digest[:]) != expected {
				return ErrChecksumMismatch
			}
			mode := expectedFileMode(name)
			if err := writePrivateFile(ctx, filepath.Join(root, filepath.FromSlash(name)), content, mode); err != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return contextErr
				}
				return ErrInvalidArchive
			}
		default:
			return ErrInvalidArchive
		}
	}
	for name := range selected.assetHashes {
		if _, ok := seen[name]; !ok {
			return ErrInvalidArchive
		}
	}
	for name := range selected.directories {
		if _, ok := seen[name]; !ok {
			return ErrInvalidArchive
		}
	}
	if selected.archivePrefix != "" && !seenArchiveRoot {
		return ErrInvalidArchive
	}
	return nil
}

func verifyTree(ctx context.Context, root string, selected release, notices []noticeFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensurePrivateDirectoryPath(filepath.Dir(root), false); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !currentUserOwnsDirectory(info) {
		return ErrForeignInstall
	}
	markerPath := filepath.Join(root, ".personastack-cua.json")
	markerInfo, err := os.Lstat(markerPath)
	if err != nil || !markerInfo.Mode().IsRegular() || markerInfo.Mode().Perm() != 0o600 || markerInfo.Size() <= 0 || markerInfo.Size() > maximumMarker {
		return ErrForeignInstall
	}
	markerFD, err := syscall.Open(markerPath, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ErrForeignInstall
	}
	markerFile := os.NewFile(uintptr(markerFD), markerPath)
	if markerFile == nil {
		_ = syscall.Close(markerFD)
		return ErrForeignInstall
	}
	openedInfo, statErr := markerFile.Stat()
	marker, readErr := io.ReadAll(io.LimitReader(interruptibleReader{ctx: ctx, reader: markerFile}, maximumMarker+1))
	closeErr := markerFile.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if statErr != nil || readErr != nil || closeErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(markerInfo, openedInfo) || int64(len(marker)) > maximumMarker {
		return ErrForeignInstall
	}
	wantMarker, _ := json.Marshal(managedMarker{Version: selected.version, ArchiveSHA256: selected.archiveSHA256})
	wantMarker = append(wantMarker, '\n')
	if !bytes.Equal(marker, wantMarker) {
		return ErrForeignInstall
	}
	allowedFiles := make(map[string]string, len(selected.assetHashes)+len(notices)+1)
	for name, digest := range selected.assetHashes {
		allowedFiles[name] = digest
	}
	allowedFiles[".personastack-cua.json"] = ""
	allowedDirectories := make(map[string]struct{}, len(selected.directories)+1)
	for name := range selected.directories {
		allowedDirectories[name] = struct{}{}
	}
	for _, notice := range notices {
		digest := sha256.Sum256(notice.content)
		allowedFiles[notice.name] = hex.EncodeToString(digest[:])
		parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(notice.name)))
		if parent != "." {
			allowedDirectories[parent] = struct{}{}
		}
	}
	if len(allowedFiles)+len(allowedDirectories) > maximumTreeEntries {
		return ErrForeignInstall
	}
	children := expectedChildren(allowedFiles, allowedDirectories)
	verifiedFiles := make(map[string]struct{}, len(allowedFiles))
	verifiedDirectories := make(map[string]struct{}, len(allowedDirectories))
	if err := verifyDirectory(ctx, root, ".", children, allowedFiles, allowedDirectories, verifiedFiles, verifiedDirectories); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return ErrForeignInstall
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(verifiedFiles) != len(allowedFiles) || len(verifiedDirectories) != len(allowedDirectories) {
		return ErrForeignInstall
	}
	return nil
}

func expectedChildren(files map[string]string, directories map[string]struct{}) map[string]map[string]struct{} {
	children := make(map[string]map[string]struct{})
	add := func(path string) {
		parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(path)))
		name := filepath.Base(filepath.FromSlash(path))
		if children[parent] == nil {
			children[parent] = make(map[string]struct{})
		}
		children[parent][name] = struct{}{}
	}
	for path := range files {
		add(path)
	}
	for path := range directories {
		add(path)
	}
	return children
}

func verifyDirectory(ctx context.Context, root, relative string, children map[string]map[string]struct{}, files map[string]string, directories map[string]struct{}, verifiedFiles, verifiedDirectories map[string]struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := root
	if relative != "." {
		path = filepath.Join(root, filepath.FromSlash(relative))
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !currentUserOwnsDirectory(info) {
		return ErrForeignInstall
	}
	if relative != "." {
		verifiedDirectories[relative] = struct{}{}
	}
	expected := children[relative]
	directory, err := openDirectory(path)
	if err != nil {
		return ErrForeignInstall
	}
	openedInfo, statErr := directory.Stat()
	if statErr != nil || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
		_ = directory.Close()
		return ErrForeignInstall
	}
	entries, readErr := directory.ReadDir(len(expected) + 1)
	closeErr := directory.Close()
	if closeErr != nil || (readErr != nil && !errors.Is(readErr, io.EOF)) || len(entries) != len(expected) {
		return ErrForeignInstall
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, ok := expected[entry.Name()]; !ok {
			return ErrForeignInstall
		}
		child := entry.Name()
		if relative != "." {
			child = relative + "/" + child
		}
		if _, ok := directories[child]; ok {
			if err := verifyDirectory(ctx, root, child, children, files, directories, verifiedFiles, verifiedDirectories); err != nil {
				return err
			}
			continue
		}
		digest, ok := files[child]
		if !ok {
			return ErrForeignInstall
		}
		if err := verifyFile(ctx, filepath.Join(root, filepath.FromSlash(child)), child, digest); err != nil {
			return err
		}
		verifiedFiles[child] = struct{}{}
	}
	return nil
}

func verifyFile(ctx context.Context, path, relative, digest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maximumFileBytes || info.Mode().Perm() != expectedFileMode(relative).Perm() {
		return ErrForeignInstall
	}
	if digest == "" {
		return nil
	}
	fileFD, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ErrForeignInstall
	}
	file := os.NewFile(uintptr(fileFD), path)
	if file == nil {
		_ = syscall.Close(fileFD)
		return ErrForeignInstall
	}
	openedInfo, statErr := file.Stat()
	hash := sha256.New()
	copied, copyErr := io.Copy(hash, io.LimitReader(interruptibleReader{ctx: ctx, reader: file}, maximumFileBytes+1))
	closeErr := file.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) || openedInfo.Size() > maximumFileBytes || copied > maximumFileBytes || copied != openedInfo.Size() || copyErr != nil || closeErr != nil || hex.EncodeToString(hash.Sum(nil)) != digest {
		return ErrForeignInstall
	}
	return nil
}

func openDirectory(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, ErrForeignInstall
	}
	return file, nil
}

func expectedFileMode(name string) os.FileMode {
	switch name {
	case "cua-driver", "cua-cursor-theme", "wayland-helper/install.sh":
		return 0o700
	default:
		return 0o600
	}
}

func writePrivateFile(ctx context.Context, path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create Cua runtime file: %w", err)
	}
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return err
		}
		chunk := data
		if len(chunk) > 64<<10 {
			chunk = chunk[:64<<10]
		}
		written, err := file.Write(chunk)
		if err != nil || written == 0 {
			_ = file.Close()
			if err == nil {
				err = io.ErrShortWrite
			}
			return fmt.Errorf("write Cua runtime file: %w", err)
		}
		data = data[written:]
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close Cua runtime file: %w", err)
	}
	return nil
}

type interruptibleReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r interruptibleReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
