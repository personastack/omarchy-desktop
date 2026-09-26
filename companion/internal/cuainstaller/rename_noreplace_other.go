//go:build !linux || !amd64

package cuainstaller

func renameNoReplace(string, string) error {
	return ErrUnavailable
}
