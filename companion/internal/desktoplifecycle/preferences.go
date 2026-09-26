//go:build linux

package desktoplifecycle

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

type filePausePreference struct{ path string }

// NewFilePausePreference stores only the per-origin pause bit in the user's
// private PersonaStack config directory.
func NewFilePausePreference() (PausePreference, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return nil, ErrUnavailable
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, ErrUnavailable
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, ErrUnavailable
	}
	directory := filepath.Join(root, "personastack")
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, ErrUnavailable
	}
	if err := verifyPreferenceDirectory(directory); err != nil {
		return nil, ErrUnavailable
	}
	return filePausePreference{path: filepath.Join(directory, "desktop-control-preferences.json")}, nil
}

func (p filePausePreference) Load(origin string) (bool, error) {
	preferences, err := p.read()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return preferences[origin], nil
}

func (p filePausePreference) Save(origin string, paused bool) error {
	if origin == "" || p.path == "" {
		return ErrUnavailable
	}
	preferences, err := p.read()
	if errors.Is(err, os.ErrNotExist) {
		preferences = make(map[string]bool)
	} else if err != nil {
		return err
	}
	preferences[origin] = paused
	data, err := json.Marshal(preferences)
	if err != nil {
		return ErrUnavailable
	}
	info, err := os.Lstat(p.path)
	if err == nil && verifyPreferenceFile(info) != nil {
		return ErrUnavailable
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrUnavailable
	}
	file, err := os.OpenFile(p.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return ErrUnavailable
	}
	info, err = file.Stat()
	if err != nil || verifyPreferenceFile(info) != nil {
		_ = file.Close()
		return ErrUnavailable
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return ErrUnavailable
	}
	return nil
}

func (p filePausePreference) read() (map[string]bool, error) {
	if p.path == "" {
		return nil, ErrUnavailable
	}
	file, err := os.OpenFile(p.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || verifyPreferenceFile(info) != nil {
		return nil, ErrUnavailable
	}
	var preferences map[string]bool
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&preferences); err != nil || preferences == nil {
		return nil, ErrUnavailable
	}
	return preferences, nil
}

func verifyPreferenceDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return ErrUnavailable
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return ErrUnavailable
	}
	return nil
}

func verifyPreferenceFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return ErrUnavailable
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return ErrUnavailable
	}
	return nil
}
